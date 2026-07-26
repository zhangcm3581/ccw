// Package dns是Console的DNS抽象与子域名分配（console-fleet-design §6、C8）。
//
// 为什么每节点一条A记录、不能用通配符：通配符只能指向单一IP，而每台节点有各自
// 的公网IP；让通配符指向Console再反代到各节点会把Console放进用户数据路径，
// 摧毁"Console宕机不影响已部署节点"这条性质（§6.1）。
//
// **manual是默认实现，零依赖**：Console展示待添加的记录、管理员手动添加、点校验。
// Route 53自动化（C9）是可选项——做成接口意味着系统第一天就能用manual跑通全流程。
package dns

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Zone是一个托管域名。
type Zone struct {
	ID              string
	Domain          string // example.com
	Provider        string // manual|route53
	ProviderRef     string // route53的hosted zone id
	SubdomainPrefix string // api-01里的"api"
}

// Provider是DNS操作的抽象（§6.3）。
type Provider interface {
	// UpsertA幂等地把name指向ip；返回changeID供WaitPropagated轮询。
	UpsertA(ctx context.Context, z Zone, name, ip string, ttl int) (changeID string, err error)
	DeleteA(ctx context.Context, z Zone, name, ip string) error
	// WaitPropagated阻塞到记录生效；manual实现靠交叉查询公共解析器。
	WaitPropagated(ctx context.Context, z Zone, changeID string) error
	// CheckCAA检查该zone的CAA是否允许Let's Encrypt签发（§6.5）。
	CheckCAA(ctx context.Context, z Zone) (allowsLetsEncrypt bool, err error)
}

// ErrNotPropagated表示记录尚未生效——manual模式下这是常态（管理员还没加记录），
// 流水线据此阻断并提示，**绝不继续到起Caddy那一步**（§5.3关键约束2）。
var ErrNotPropagated = errors.New("dns: 记录尚未生效")

// ErrPendingManual表示manual模式需要管理员先手动添加记录。
var ErrPendingManual = errors.New("dns: 等待管理员手动添加记录")

// FQDN拼出完整域名。
func FQDN(prefix string, seq int, domain string) string {
	return fmt.Sprintf("%s-%02d.%s", prefix, seq, domain)
}

// reservedSubdomains不可分配给节点（§6.2）：它们要么有约定用途，
// 要么已被站点/后台占用，分出去会造成冲突或被误认。
var reservedSubdomains = map[string]bool{
	"www": true, "admin": true, "api": true, "app": true, "docs": true,
	"status": true, "mail": true, "smtp": true, "imap": true,
	"_acme-challenge": true, "_dmarc": true, "_domainkey": true,
}

// nsPattern匹配ns、ns1、ns02这类名字（域名服务器约定用途）。
var nsPattern = regexp.MustCompile(`^ns\d*$`)

// IsReserved判断某个子域名标签是否保留。extra传入CCW_SITE_DOMAIN/
// CCW_ADMIN_DOMAIN在该zone下已占用的标签。
func IsReserved(label string, extra ...string) bool {
	l := strings.ToLower(label)
	if reservedSubdomains[l] || nsPattern.MatchString(l) {
		return true
	}
	for _, e := range extra {
		if strings.EqualFold(l, e) {
			return true
		}
	}
	return false
}

// SeqAllocator分配zone内单调递增的序号。
//
// **序号永不回收**（§6.2）：节点退役后其序号作废、不再分配给新机器。
// 这是安全属性不是洁癖——客户端会把域名持久化到本地，旧客户端或旧文档里残留的
// api-07若被重新分配给新客户的机器，会导致误连（把请求发到别人的节点上）。
type SeqAllocator interface {
	// NextSeq原子地取出并递增zone的next_seq。
	NextSeq(ctx context.Context, zoneID string) (int, error)
}

// Allocate为节点分配一个子域名：取下一个序号，跳过保留名与已占用的名字。
func Allocate(ctx context.Context, alloc SeqAllocator, z Zone, taken func(string) bool, reservedExtra ...string) (seq int, fqdn string, err error) {
	// 最多试若干次：保留名是有限的，连续撞上说明配置有问题，不该无限循环。
	for attempt := 0; attempt < 50; attempt++ {
		seq, err = alloc.NextSeq(ctx, z.ID)
		if err != nil {
			return 0, "", err
		}
		label := fmt.Sprintf("%s-%02d", z.SubdomainPrefix, seq)
		if IsReserved(label, reservedExtra...) {
			continue // 序号照常消耗——永不回收，跳过的号也不再使用
		}
		fqdn = label + "." + z.Domain
		if taken != nil && taken(fqdn) {
			continue
		}
		return seq, fqdn, nil
	}
	return 0, "", errors.New("dns: 连续50次分配都撞上保留名或已占用，检查zone配置")
}
