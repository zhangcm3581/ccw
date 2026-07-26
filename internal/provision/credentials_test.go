package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"ccw/internal/sshexec"
)

// fakeRunner记录执行过的命令，并按前缀给出结果。
type fakeRunner struct {
	cmds    []string
	results map[string]sshexec.Result // 前缀匹配
	err     error
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (sshexec.Result, error) {
	f.cmds = append(f.cmds, cmd)
	if f.err != nil {
		return sshexec.Result{}, f.err
	}
	for prefix, r := range f.results {
		if strings.HasPrefix(cmd, prefix) {
			return r, nil
		}
	}
	return sshexec.Result{}, nil
}

func (f *fakeRunner) joined() string { return strings.Join(f.cmds, "\n---\n") }

func TestGenerateKeyPair(t *testing.T) {
	kp, err := sshexec.GenerateKeyPair("ccw-console")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(kp.AuthorizedKey, "ssh-ed25519 ") {
		t.Errorf("应为ed25519公钥行: %s", kp.AuthorizedKey)
	}
	if !strings.Contains(kp.AuthorizedKey, "ccw-console") {
		t.Error("公钥行应带注释便于在authorized_keys里辨认")
	}
	// 私钥可解析回AuthMethod（落库→解密→使用的链路）
	if _, err := sshexec.AuthFromPrivateKey(kp.PrivatePEM); err != nil {
		t.Errorf("私钥应可解析: %v", err)
	}
	// 两次生成不同
	kp2, _ := sshexec.GenerateKeyPair("ccw-console")
	if kp.AuthorizedKey == kp2.AuthorizedKey {
		t.Error("两次生成的密钥不得相同")
	}
	if _, err := sshexec.AuthFromPrivateKey([]byte("not a key")); err == nil {
		t.Error("非法私钥应报错")
	}
}

// Harden必须：注入公钥（追加不覆盖、幂等）→ 用新私钥重新拨号验证 → 才返回成功。
func TestHardenInjectsThenVerifies(t *testing.T) {
	f := &fakeRunner{}
	dialed := false
	var dialedAuth []ssh.AuthMethod
	dial := func(_ context.Context, tgt sshexec.Target, auth []ssh.AuthMethod) (*sshexec.Client, error) {
		dialed = true
		dialedAuth = auth
		if tgt.KnownFingerprint != "SHA256:known" {
			t.Errorf("验证拨号必须复用已固定的指纹，got %q", tgt.KnownFingerprint)
		}
		return nil, errors.New("stub") // 让调用方走失败路径，下面单独测成功路径
	}
	_, err := Harden(context.Background(), f, dial,
		sshexec.Target{Host: "203.0.113.7", User: "ccw", KnownFingerprint: "SHA256:known"}, "", nil)
	if err == nil {
		t.Fatal("拨号失败时Harden必须失败——那是'密码可以丢弃'的唯一凭据")
	}
	if !dialed || len(dialedAuth) != 1 {
		t.Fatal("应当用新私钥重新拨号")
	}

	cmd := f.joined()
	for _, want := range []string{"mkdir -p ~/.ssh", "chmod 700 ~/.ssh", "chmod 600 ~/.ssh/authorized_keys",
		"grep -qxF", ">> ~/.ssh/authorized_keys"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("注入命令缺少%q:\n%s", want, cmd)
		}
	}
	// 追加而非覆盖：管理员自己的密钥必须保留
	if strings.Contains(cmd, "> ~/.ssh/authorized_keys") && !strings.Contains(cmd, ">> ~/.ssh/authorized_keys") {
		t.Error("必须追加（>>）而不是覆盖（>）authorized_keys")
	}
}

func TestHardenFailsWhenInjectFails(t *testing.T) {
	f := &fakeRunner{results: map[string]sshexec.Result{
		"set -e": {ExitCode: 1, Stderr: "Permission denied\n"},
	}}
	dial := func(context.Context, sshexec.Target, []ssh.AuthMethod) (*sshexec.Client, error) {
		t.Error("注入失败时不应继续拨号")
		return nil, nil
	}
	if _, err := Harden(context.Background(), f, dial, sshexec.Target{User: "ccw"}, "", nil); err == nil {
		t.Fatal("注入失败应返回错误")
	}
}

// 公钥里若含单引号等字符，注入命令不得被截断（shell注入面）。
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":       "'simple'",
		"with space":   "'with space'",
		"it's":         `'it'\''s'`,
		"a'; rm -rf /": `'a'\''; rm -rf /'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSudoPrefix(t *testing.T) {
	if got := SudoPrefix("root", ""); got != "" {
		t.Errorf("root不需要sudo，got %q", got)
	}
	if got := SudoPrefix("ccw", ""); got != "sudo -n " {
		t.Errorf("免密sudo，got %q", got)
	}
	got := SudoPrefix("ccw", "p@ss'word")
	if !strings.Contains(got, "sudo -S") || !strings.Contains(got, `'p@ss'\''word'`) {
		t.Errorf("带密码sudo应正确引用: %q", got)
	}
}

func TestDetectSudo(t *testing.T) {
	ctx := context.Background()

	// root：不需要探测
	if need, err := DetectSudo(ctx, &fakeRunner{}, "root", ""); need || err != nil {
		t.Errorf("root: need=%v err=%v", need, err)
	}
	// 免密sudo
	f := &fakeRunner{results: map[string]sshexec.Result{"sudo -n true": {ExitCode: 0}}}
	if need, err := DetectSudo(ctx, f, "ccw", ""); need || err != nil {
		t.Errorf("免密sudo: need=%v err=%v", need, err)
	}
	// 需要密码且提供了
	f = &fakeRunner{results: map[string]sshexec.Result{
		"sudo -n true": {ExitCode: 1},
		"echo ":        {ExitCode: 0},
	}}
	if need, err := DetectSudo(ctx, f, "ccw", "pw"); !need || err != nil {
		t.Errorf("带密码sudo: need=%v err=%v", need, err)
	}
	// 需要密码但没提供：明确失败，不做猜测性修复（§9.1）
	f = &fakeRunner{results: map[string]sshexec.Result{"sudo -n true": {ExitCode: 1}}}
	if _, err := DetectSudo(ctx, f, "ccw", ""); err == nil {
		t.Error("需要密码但未提供时应失败")
	}
	// 密码不对/不在sudoers
	f = &fakeRunner{results: map[string]sshexec.Result{
		"sudo -n true": {ExitCode: 1},
		"echo ":        {ExitCode: 1},
	}}
	if _, err := DetectSudo(ctx, f, "ccw", "wrong"); err == nil {
		t.Error("sudo校验失败应报错")
	}
}

// 密码绝不出现在任何返回值里（它只应活在调用方的局部变量中）。
func TestHardenNeverReturnsPassword(t *testing.T) {
	const pw = "首登密码-SHOULD-NOT-LEAK"
	f := &fakeRunner{}
	dial := func(context.Context, sshexec.Target, []ssh.AuthMethod) (*sshexec.Client, error) {
		return nil, errors.New("stub")
	}
	res, err := Harden(context.Background(), f, dial, sshexec.Target{User: "ccw"}, pw, nil)
	if err != nil && strings.Contains(err.Error(), pw) {
		t.Error("错误信息不得含密码")
	}
	if strings.Contains(res.SSHUser, pw) || strings.Contains(string(res.KeyPair.PrivatePEM), pw) {
		t.Error("返回值不得含密码")
	}
	// 注入命令里也不该有密码（注入用的是首登会话，不需要sudo）
	if strings.Contains(f.joined(), pw) {
		t.Error("公钥注入命令不应包含密码")
	}
}
