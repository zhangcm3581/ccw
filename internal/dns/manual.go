package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Manual是默认的DNS实现（§6.3）：Console展示待添加的记录，管理员到自己的
// DNS控制台手动添加，然后点"校验"。
//
// 校验用**至少两个独立公共解析器交叉查询**，都指向节点IP才通过。
// 只查一个解析器会被单点缓存误导：某个解析器还缓存着旧记录（或NXDOMAIN），
// 会让已经生效的记录被判为未生效，反过来也可能过早放行。
type Manual struct {
	// Resolvers是用于交叉验证的公共解析器地址（host:port）。
	// 默认1.1.1.1与8.8.8.8——分属不同运营方，不共享缓存。
	Resolvers []string
	// Timeout是单次查询超时。
	Timeout time.Duration
	// lookupFn可注入，用于单测；生产为nil走真实DNS。
	lookupFn func(ctx context.Context, resolver, name string) ([]string, error)
}

func (m *Manual) resolvers() []string {
	if len(m.Resolvers) > 0 {
		return m.Resolvers
	}
	return []string{"1.1.1.1:53", "8.8.8.8:53"}
}

func (m *Manual) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return 5 * time.Second
}

// UpsertA在manual模式下不做任何网络操作：它返回一个描述性的changeID，
// 由调用方把"请添加这条记录"展示给管理员（§6.3）。
func (m *Manual) UpsertA(_ context.Context, z Zone, name, ip string, ttl int) (string, error) {
	return fmt.Sprintf("manual:%s A %s (TTL %d)", name, ip, ttl), nil
}

// DeleteA同样只是提示：退役时必须由管理员删记录，否则子域名会悬挂
// 指向已被云厂商回收的IP，被他人接管（§6.4）。
func (m *Manual) DeleteA(_ context.Context, z Zone, name, ip string) error {
	return nil
}

// WaitPropagated在manual模式下是**一次性校验而不是等待**：
// 记录要靠人去加，阻塞轮询几分钟没有意义——不如立刻返回"尚未生效"，
// 让流水线停在这一步，管理员加完记录后点重试（断点续跑，§5.3）。
func (m *Manual) WaitPropagated(ctx context.Context, z Zone, changeID string) error {
	name, ip, ok := parseManualChange(changeID)
	if !ok {
		return fmt.Errorf("dns: 无法解析changeID: %q", changeID)
	}
	return m.Verify(ctx, name, ip)
}

// Verify交叉查询全部解析器，要求都能解析出期望IP。
func (m *Manual) Verify(ctx context.Context, name, wantIP string) error {
	var problems []string
	for _, r := range m.resolvers() {
		ips, err := m.lookup(ctx, r, name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: 查询失败(%v)", r, err))
			continue
		}
		found := false
		for _, got := range ips {
			if got == wantIP {
				found = true
			}
		}
		if !found {
			got := "无记录"
			if len(ips) > 0 {
				got = strings.Join(ips, ",")
			}
			problems = append(problems, fmt.Sprintf("%s: 解析到%s，期望%s", r, got, wantIP))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w：%s", ErrNotPropagated, strings.Join(problems, "；"))
	}
	return nil
}

// CheckCAA检查zone是否有阻挡Let's Encrypt的CAA记录（§6.5）。
// 无CAA记录＝允许任何CA（RFC 8659的默认）。
//
// 查询失败（网络问题）时返回(true, err)：**不因为查不到就阻断zone接入**——
// CAA只是提前发现问题的手段，它自己不可用时不该变成新的故障点。
// 调用方应把err如实展示给管理员。
func (m *Manual) CheckCAA(ctx context.Context, z Zone) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()
	var lastErr error
	for _, r := range m.resolvers() {
		recs, err := LookupCAA(ctx, r, z.Domain)
		if errors.Is(err, ErrNoCAA) {
			return true, nil // 无CAA＝不限制
		}
		if err != nil {
			lastErr = err
			continue
		}
		return CAAAllowsLetsEncrypt(recs), nil
	}
	return true, lastErr
}

func (m *Manual) lookup(ctx context.Context, resolver, name string) ([]string, error) {
	if m.lookupFn != nil {
		return m.lookupFn(ctx, resolver, name)
	}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: m.timeout()}
			return d.DialContext(ctx, network, resolver)
		},
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()
	addrs, err := r.LookupHost(ctx, name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			out = append(out, a) // 只看A记录：节点地址是IPv4
		}
	}
	return out, nil
}

// parseManualChange解析UpsertA产出的changeID。
func parseManualChange(s string) (name, ip string, ok bool) {
	rest, found := strings.CutPrefix(s, "manual:")
	if !found {
		return "", "", false
	}
	parts := strings.Fields(rest)
	if len(parts) < 3 || parts[1] != "A" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// Instructions生成给管理员看的记录说明（向导页展示用）。
func Instructions(fqdn, ip string) string {
	return fmt.Sprintf("类型 A ｜ 主机记录 %s ｜ 记录值 %s ｜ TTL 60", fqdn, ip)
}
