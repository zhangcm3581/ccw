package dns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// CAA查询（§6.5的zone接入预检）。
//
// 标准库没有CAA支持（net.Resolver只到A/AAAA/MX/TXT等），因此这里用
// golang.org/x/net/dns/dnsmessage自己拼查询与解析应答——那是Go团队维护、
// net包内部同款的DNS报文库，不引入第三方DNS客户端。
//
// 为什么值得做：CAA是**按zone**配置的。若example.com设了只允许某家CA，
// 该zone下每一台节点的证书都会失败——一次性预检可以避免逐台排查（§6.5）。

// CAARecord是一条CAA记录。
type CAARecord struct {
	Flags uint8
	Tag   string
	Value string
}

// ErrNoCAA表示该域名没有CAA记录（RFC 8659：无记录＝不限制任何CA）。
var ErrNoCAA = errors.New("dns: 无CAA记录")

// typeCAA是CAA记录的IANA类型号（RFC 8659）。
const typeCAA = dnsmessage.Type(257)

// LookupCAA向指定解析器查询CAA记录。
//
// **CAA的查找是沿域名树向上的**：api-01.example.com没有CAA时，
// CA会继续查example.com、再查com。这里只查注册域名本身——它是管理员能配置、
// 也是实际会出问题的那一层。
func LookupCAA(ctx context.Context, resolver, domain string) ([]CAARecord, error) {
	name, err := dnsmessage.NewName(strings.TrimSuffix(domain, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("dns: 域名非法: %w", err)
	}
	// dnsmessage没有TypeCAA常量，直接写IANA的类型号257；
	// 应答里它会成为UnknownResource，由parseCAARdata按RFC 8659解RDATA。
	// 随机query ID并在应答里校验：固定ID的UDP查询容易被离路径攻击者抢答，
	// 伪造一条"允许LE"的CAA会让预检失去意义。
	var idBuf [2]byte
	if _, err := rand.Read(idBuf[:]); err != nil {
		return nil, err
	}
	queryID := binary.BigEndian.Uint16(idBuf[:])
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{ID: queryID, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  typeCAA,
			Class: dnsmessage.ClassINET,
		}},
	}
	packed, err := msg.Pack()
	if err != nil {
		return nil, err
	}

	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "udp", resolver)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(packed); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}

	var resp dnsmessage.Message
	if err := resp.Unpack(buf[:n]); err != nil {
		return nil, fmt.Errorf("dns: 应答无法解析: %w", err)
	}
	// ID与question都要对上：只看ID仍可能收到别的查询的应答。
	if resp.Header.ID != queryID || !resp.Header.Response {
		return nil, fmt.Errorf("dns: 应答与查询不匹配（可能是伪造或串包）")
	}
	if len(resp.Questions) != 1 || resp.Questions[0].Type != typeCAA ||
		!strings.EqualFold(resp.Questions[0].Name.String(), name.String()) {
		return nil, fmt.Errorf("dns: 应答的question段与查询不符")
	}
	if resp.Header.RCode != dnsmessage.RCodeSuccess {
		if resp.Header.RCode == dnsmessage.RCodeNameError {
			return nil, ErrNoCAA // NXDOMAIN
		}
		return nil, fmt.Errorf("dns: 应答码%v", resp.Header.RCode)
	}

	var out []CAARecord
	for _, ans := range resp.Answers {
		if ans.Header.Type != typeCAA {
			continue // CNAME等其它记录
		}
		unknown, ok := ans.Body.(*dnsmessage.UnknownResource)
		if !ok {
			continue
		}
		rec, err := parseCAARdata(unknown.Data)
		if err != nil {
			continue // 单条解析失败不影响其余记录
		}
		out = append(out, rec)
	}
	if len(out) == 0 {
		return nil, ErrNoCAA
	}
	return out, nil
}

// parseCAARdata解析CAA的RDATA（RFC 8659 §4.1）：
//
//	flags(1字节) | tag长度(1字节) | tag | value(剩余全部)
func parseCAARdata(b []byte) (CAARecord, error) {
	if len(b) < 2 {
		return CAARecord{}, errors.New("dns: CAA记录过短")
	}
	flags := b[0]
	tagLen := int(b[1])
	if 2+tagLen > len(b) {
		return CAARecord{}, errors.New("dns: CAA的tag长度越界")
	}
	return CAARecord{
		Flags: flags,
		Tag:   string(b[2 : 2+tagLen]),
		Value: string(b[2+tagLen:]),
	}, nil
}

// CAAAllowsLetsEncrypt判断一组CAA记录是否允许Let's Encrypt签发。
//
// 语义（RFC 8659）：只要存在issue/issuewild标签，就构成白名单——
// 未列出的CA一律不得签发。因此"有issue记录但都不含letsencrypt.org"＝阻挡。
// 空列表（无CAA）＝不限制，由调用方按ErrNoCAA处理。
func CAAAllowsLetsEncrypt(recs []CAARecord) bool {
	hasIssue := false
	for _, r := range recs {
		tag := strings.ToLower(r.Tag)
		if tag != "issue" && tag != "issuewild" {
			continue
		}
		hasIssue = true
		v := strings.ToLower(strings.TrimSpace(r.Value))
		// 值形如 "letsencrypt.org" 或 "letsencrypt.org; accounturi=..."
		if domain, _, _ := strings.Cut(v, ";"); strings.TrimSpace(domain) == "letsencrypt.org" {
			return true
		}
	}
	return !hasIssue // 没有任何issue记录＝不限制
}
