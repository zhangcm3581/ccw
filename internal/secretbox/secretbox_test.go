package secretbox

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// C2信封加密（console-fleet-design §12.1的C2、§8.4）：
// 托管SSH私钥、AWS凭据、TOTP secret都靠它落库。单测覆盖
// 「密文不可预测、篡改可检出、错误密钥不可解」三条。

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := hex.DecodeString(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	b, err := New(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("ssh-ed25519 PRIVATE KEY MATERIAL")
	ct, nonce, err := b.Seal(plain, "node-cred")
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Open(ct, nonce, "node-cred")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("解密不一致: %q", got)
	}
}

// 相同明文两次加密必须产生不同密文（随机nonce）——否则能从密文相等推断明文相等。
func TestCiphertextNotDeterministic(t *testing.T) {
	b, _ := New(testKey(t))
	plain := []byte("same-secret")
	c1, n1, _ := b.Seal(plain, "ctx")
	c2, n2, _ := b.Seal(plain, "ctx")
	if bytes.Equal(c1, c2) || bytes.Equal(n1, n2) {
		t.Error("相同明文的两次加密不得产生相同密文/nonce")
	}
	if len(n1) != NonceSize {
		t.Errorf("nonce长度=%d，want %d", len(n1), NonceSize)
	}
	// 密文不得包含明文片段
	if bytes.Contains(c1, plain) {
		t.Error("密文里出现了明文")
	}
}

// 篡改必须被检出（GCM认证标签）：密文、nonce、上下文任一被改都应失败。
func TestTamperDetected(t *testing.T) {
	b, _ := New(testKey(t))
	plain := []byte("aws-access-key")
	ct, nonce, _ := b.Seal(plain, "zone-cred")

	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xff
	if _, err := b.Open(bad, nonce, "zone-cred"); err == nil {
		t.Error("密文被篡改应解密失败")
	}

	badNonce := append([]byte(nil), nonce...)
	badNonce[0] ^= 0xff
	if _, err := b.Open(ct, badNonce, "zone-cred"); err == nil {
		t.Error("nonce被篡改应解密失败")
	}

	// 上下文（AAD）绑定用途：node凭据的密文不能在zone凭据的位置解开——
	// 防止把一列的密文搬到另一列冒充。
	if _, err := b.Open(ct, nonce, "node-cred"); err == nil {
		t.Error("上下文不匹配应解密失败（AAD绑定用途）")
	}
}

func TestWrongKeyFails(t *testing.T) {
	b1, _ := New(testKey(t))
	other, _ := hex.DecodeString(strings.Repeat("cd", 32))
	b2, _ := New(other)
	ct, nonce, _ := b1.Seal([]byte("x"), "ctx")
	if _, err := b2.Open(ct, nonce, "ctx"); err == nil {
		t.Error("错误密钥不得解开")
	}
}

// 密钥必须是32字节hex：短密钥、非hex、空值一律硬失败——
// 沿用「缺失即硬失败、无默认值」，绝不接受弱密钥静默降级。
func TestNewRejectsBadKeys(t *testing.T) {
	for _, k := range [][]byte{nil, make([]byte, 16), make([]byte, 31), make([]byte, 33)} {
		if _, err := New(k); err == nil {
			t.Errorf("%d字节密钥应被拒绝", len(k))
		}
	}
}

func TestParseKeyHex(t *testing.T) {
	k, err := ParseKeyHex(strings.Repeat("ab", 32))
	if err != nil || len(k) != KeySize {
		t.Fatalf("合法hex应通过: %v %d", err, len(k))
	}
	for _, bad := range []string{"", "zz", strings.Repeat("ab", 16), strings.Repeat("ab", 33)} {
		if _, err := ParseKeyHex(bad); err == nil {
			t.Errorf("ParseKeyHex(%q)应失败", bad)
		}
	}
}

// 错误信息绝不能带出明文或密钥（凭据永不进日志与错误信息，CLAUDE.md）。
func TestErrorsLeakNothing(t *testing.T) {
	b, _ := New(testKey(t))
	ct, nonce, _ := b.Seal([]byte("TOPSECRETVALUE"), "ctx")
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xff
	_, err := b.Open(bad, nonce, "ctx")
	if err == nil {
		t.Fatal("应失败")
	}
	msg := err.Error()
	for _, leak := range []string{"TOPSECRET", hex.EncodeToString(ct[:8]), strings.Repeat("ab", 8)} {
		if strings.Contains(msg, leak) {
			t.Errorf("错误信息泄漏了内容: %q", msg)
		}
	}
}
