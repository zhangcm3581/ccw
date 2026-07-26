package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"ccw/internal/auth"
	"ccw/internal/secretbox"
	"ccw/internal/totp"
)

type fakeAdminCreator struct {
	count        int
	username     string
	passwordHash string
	enc, nonce   []byte
	err          error
}

func (f *fakeAdminCreator) CountAdmins(context.Context) (int, error) { return f.count, f.err }
func (f *fakeAdminCreator) CreateAdmin(_ context.Context, u, hash string, enc, nonce []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.username, f.passwordHash, f.enc, f.nonce = u, hash, enc, nonce
	f.count++
	return "u-1", nil
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key, _ := hex.DecodeString(strings.Repeat("ab", 32))
	b, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCreateAdmin(t *testing.T) {
	f := &fakeAdminCreator{}
	box := testBox(t)
	const pw = "a-strong-admin-password"
	var out, errBuf bytes.Buffer

	code := runCreateAdmin([]string{"--username", "admin"}, &out, &errBuf, f, box,
		func(string) (string, error) { return pw, nil })
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf.String())
	}
	if f.username != "admin" {
		t.Errorf("username=%q", f.username)
	}
	// 库里只存Argon2id哈希，明文密码绝不落库
	if !strings.HasPrefix(f.passwordHash, "argon2id$") || strings.Contains(f.passwordHash, pw) {
		t.Errorf("密码必须以Argon2id哈希存储: %s", f.passwordHash)
	}
	if !auth.VerifySecret(pw, f.passwordHash) {
		t.Error("哈希应能校验原密码")
	}
	// TOTP secret以信封加密存储，且用的AAD与登录侧一致
	plain, err := box.Open(f.enc, f.nonce, adminTOTPContext)
	if err != nil {
		t.Fatalf("TOTP secret应能用同一AAD解开（否则登录时解不开）: %v", err)
	}
	// 输出的密钥要与落库的一致，且可用于算码
	if !strings.Contains(out.String(), string(plain)) {
		t.Error("输出应含刚生成的TOTP密钥（只显示一次）")
	}
	if _, err := totp.Code(string(plain), timeNowForTest()); err != nil {
		t.Errorf("生成的TOTP密钥应可用: %v", err)
	}
	if !strings.Contains(out.String(), "otpauth://totp/") {
		t.Error("输出应含认证器可扫的otpauth链接")
	}
	// 密码绝不回显
	if strings.Contains(out.String(), pw) || strings.Contains(errBuf.String(), pw) {
		t.Error("密码不得出现在任何输出里")
	}
}

func TestCreateAdminRejectsWeakPassword(t *testing.T) {
	f := &fakeAdminCreator{}
	var out, errBuf bytes.Buffer
	code := runCreateAdmin([]string{"--username", "admin"}, &out, &errBuf, f, testBox(t),
		func(string) (string, error) { return "short", nil })
	if code == 0 {
		t.Fatal("弱口令应被拒绝")
	}
	if f.count != 0 {
		t.Error("拒绝时不得创建账号")
	}
}

func TestCreateAdminRejectsMismatch(t *testing.T) {
	f := &fakeAdminCreator{}
	var out, errBuf bytes.Buffer
	n := 0
	code := runCreateAdmin([]string{"--username", "admin"}, &out, &errBuf, f, testBox(t),
		func(string) (string, error) {
			n++
			if n == 1 {
				return "a-strong-admin-password", nil
			}
			return "a-different-password-x", nil
		})
	if code == 0 {
		t.Fatal("两次输入不一致应失败")
	}
	if !strings.Contains(errBuf.String(), "不一致") {
		t.Errorf("应提示不一致: %s", errBuf.String())
	}
}

func TestCreateAdminRequiresUsername(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runCreateAdmin(nil, &out, &errBuf, &fakeAdminCreator{}, testBox(t),
		func(string) (string, error) { return "", errors.New("不应提示输入") }); code == 0 {
		t.Fatal("缺--username应失败")
	}
}

// 源码级守卫：密码不得做命令行参数（会进shell history与ps输出）。
func TestNoPasswordFlag(t *testing.T) {
	src, err := readFileString("admin.go")
	if err != nil {
		t.Skip("源码不可读")
	}
	for _, bad := range []string{`fs.String("password"`, `StringVar(&password`} {
		if strings.Contains(src, bad) {
			t.Errorf("admin.go出现%q——密码不得做命令行参数", bad)
		}
	}
}

func timeNowForTest() time.Time { return time.Unix(1111111111, 0) }

func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
