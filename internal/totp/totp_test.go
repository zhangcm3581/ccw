package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238附录B的官方测试向量（SHA-1，密钥"12345678901234567890"的base32）。
// 用官方向量而不是自造期望值——自造只能证明实现与自己一致。
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32("12345678901234567890")

func TestRFC6238Vectors(t *testing.T) {
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		got, err := Code(rfcSecret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatalf("unix=%d: %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("unix=%d: got %s, want %s（RFC 6238向量）", c.unix, got, c.want)
		}
	}
}

func TestVerifyAcceptsCurrentAndAdjacentWindows(t *testing.T) {
	now := time.Unix(1111111111, 0)
	cur, _ := Code(rfcSecret, now)
	prev, _ := Code(rfcSecret, now.Add(-30*time.Second))
	next, _ := Code(rfcSecret, now.Add(30*time.Second))

	for name, code := range map[string]string{"当前": cur, "前一窗口": prev, "后一窗口": next} {
		if !Verify(rfcSecret, code, now) {
			t.Errorf("%s窗口的码应被接受（容忍±1步的时钟漂移）", name)
		}
	}
	// ±2步之外必须拒绝：窗口开太大等于削弱二次因子。
	tooOld, _ := Code(rfcSecret, now.Add(-90*time.Second))
	if Verify(rfcSecret, tooOld, now) {
		t.Error("超出容忍窗口的码必须拒绝")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	now := time.Unix(1111111111, 0)
	for _, bad := range []string{"", "000000", "12345", "1234567", "abcdef", "  050471 "} {
		if bad == "  050471 " {
			// 带空白的合法码应被容忍（用户从认证器复制常带空格）
			if !Verify(rfcSecret, bad, now) {
				t.Error("带空白的正确码应被接受（去空白后比对）")
			}
			continue
		}
		if Verify(rfcSecret, bad, now) {
			t.Errorf("Verify应拒绝 %q", bad)
		}
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1111111111, 0)
	code, _ := Code(rfcSecret, now)
	other, _ := GenerateSecret()
	if Verify(other, code, now) {
		t.Error("其他密钥的码不得通过")
	}
}

func TestGenerateSecret(t *testing.T) {
	s1, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := GenerateSecret()
	if s1 == s2 {
		t.Error("两次生成的密钥不得相同")
	}
	if len(s1) != 32 { // 20字节 → base32无填充32字符
		t.Errorf("密钥长度=%d，want 32（20字节base32）", len(s1))
	}
	if strings.ContainsAny(s1, "=018") {
		t.Errorf("base32密钥不应含填充或易混字符: %s", s1)
	}
	// 生成的密钥可用于算码
	if _, err := Code(s1, time.Now()); err != nil {
		t.Errorf("生成的密钥应可用: %v", err)
	}
}

func TestProvisioningURI(t *testing.T) {
	uri := ProvisioningURI(rfcSecret, "admin", "ccw Console")
	for _, want := range []string{"otpauth://totp/", "secret=" + rfcSecret, "issuer=ccw+Console", "admin"} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI缺少%q: %s", want, uri)
		}
	}
}

func TestCodeRejectsBadSecret(t *testing.T) {
	for _, bad := range []string{"", "not-base32!!", "A"} {
		if _, err := Code(bad, time.Now()); err == nil {
			t.Errorf("Code(%q)应报错", bad)
		}
	}
}
