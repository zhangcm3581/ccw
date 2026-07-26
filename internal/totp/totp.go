// Package totp实现RFC 6238的时基一次性密码（Console管理员登录的第二因子，
// console-fleet-design §8.1）。
//
// 自己实现而不是引入依赖：算法总共几十行标准库调用（HMAC-SHA1 + 动态截断），
// 而go.mod目前只有6个直接依赖，为此新增一棵依赖树不划算。正确性由RFC 6238
// 附录B的官方测试向量保证。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	step    = 30 * time.Second // RFC 6238推荐步长，认证器App默认值
	digits  = 6
	skew    = 1  // 容忍±1步（±30秒）的时钟漂移；再大等于削弱二次因子
	secLen  = 20 // 密钥字节数（SHA-1的块内长度，认证器通用）
	b32Alph = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret生成新的base32密钥（无填充，可直接给认证器扫码或手输）。
func GenerateSecret() (string, error) {
	buf := make([]byte, secLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("totp: read random: %w", err)
	}
	return b32.EncodeToString(buf), nil
}

// Code计算给定时刻的六位码。
func Code(secret string, at time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("totp: secret is not valid base32")
	}
	if len(key) < 10 {
		return "", fmt.Errorf("totp: secret too short")
	}
	return codeAt(key, uint64(at.Unix())/uint64(step.Seconds())), nil
}

func codeAt(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(buf[:])
	sum := m.Sum(nil)
	// 动态截断（RFC 4226 §5.4）
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, v%1_000_000)
}

// Verify校验用户提交的码，容忍±skew步的时钟漂移。
//
// 用constant-time比较：TOTP空间只有10^6，计时侧信道配合限速仍是可利用的。
// 输入的空白会被去掉——认证器App复制常带空格，让用户为此失败没有意义。
func Verify(secret, code string, now time.Time) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != digits {
		return false
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) < 10 {
		return false
	}
	counter := uint64(now.Unix()) / uint64(step.Seconds())
	ok := false
	for d := -skew; d <= skew; d++ {
		// 不提前return：全部窗口都算一遍，避免命中位置造成的耗时差异。
		if subtle.ConstantTimeCompare([]byte(codeAt(key, counter+uint64(d))), []byte(code)) == 1 {
			ok = true
		}
	}
	return ok
}

// ProvisioningURI生成认证器可扫的otpauth://链接（也可手动输入secret）。
func ProvisioningURI(secret, account, issuer string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(digits))
	q.Set("period", fmt.Sprint(int(step.Seconds())))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
