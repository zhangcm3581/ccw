// Package provision是节点纳管的编排层（console-fleet-design §5、§9、C6/C11）：
// 凭据生命周期与bootstrap流水线的步骤定义。
//
// 它把sshexec（连接与执行）、pipeline（步骤编排与记账）、consolestore（持久化）
// 粘起来，本身不含它们任一的职责。
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"ccw/internal/sshexec"
)

// Runner是provision需要的SSH能力面（单测注入假实现，避免每个测试都起SSH server）。
type Runner interface {
	Run(ctx context.Context, cmd string) (sshexec.Result, error)
}

// Dialer抽象拨号，便于单测验证"用新密钥重新拨号"这一步真的发生了。
type Dialer func(ctx context.Context, t sshexec.Target, auth []ssh.AuthMethod) (*sshexec.Client, error)

// HardenResult是harden步骤的产物：托管密钥（私钥待加密落库）与实际使用的运维用户。
type HardenResult struct {
	KeyPair sshexec.KeyPair
	SSHUser string
}

// Harden完成凭据生命周期的核心交接（§9）：
//
//	生成密钥 → 注入公钥 → **用私钥重新拨号验证** → 验证通过才返回
//
// 顺序不可颠倒。先落库后验证会在注入失败时留下一把连不上的"托管密钥"，
// 而那时首登密码已经被丢弃——节点就此失联，只能人工上机救。
//
// 密码由调用方持有并在返回后立即置零；本函数不保存它、不记录它。
// sudoPassword非空时用于`sudo -S`（同样不落库）。
func Harden(ctx context.Context, cli Runner, dial Dialer, target sshexec.Target,
	sudoPassword string, log func(string, ...any)) (HardenResult, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	kp, err := sshexec.GenerateKeyPair("ccw-console")
	if err != nil {
		return HardenResult{}, err
	}

	// 注入公钥：追加而不是覆盖authorized_keys——管理员自己的密钥必须保留，
	// 否则Console一旦失联就没有第二条路进机器。
	// grep -qxF先判重，重复执行不会累积重复行（幂等，§5.3）。
	inject := fmt.Sprintf(`set -e
mkdir -p ~/.ssh && chmod 700 ~/.ssh
touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
grep -qxF %s ~/.ssh/authorized_keys || echo %s >> ~/.ssh/authorized_keys`,
		shellQuote(kp.AuthorizedKey), shellQuote(kp.AuthorizedKey))

	res, err := cli.Run(ctx, inject)
	if err != nil {
		return HardenResult{}, fmt.Errorf("注入公钥失败: %w", err)
	}
	if res.ExitCode != 0 {
		return HardenResult{}, fmt.Errorf("注入公钥失败（退出码%d）: %s", res.ExitCode, firstLine(res.Stderr))
	}
	log("公钥已注入 ~/.ssh/authorized_keys")

	// 验证：用新私钥重新拨号。这一步失败就必须整体失败——
	// 它是"密码可以丢弃了"的唯一凭据。
	auth, err := sshexec.AuthFromPrivateKey(kp.PrivatePEM)
	if err != nil {
		return HardenResult{}, err
	}
	verifyTarget := target
	verifyTarget.KnownFingerprint = target.KnownFingerprint // 指纹已在首连时固定，这里必须复用
	c2, err := dial(ctx, verifyTarget, []ssh.AuthMethod{auth})
	if err != nil {
		return HardenResult{}, fmt.Errorf("托管密钥验证登录失败（公钥可能未生效，密码仍需保留）: %w", err)
	}
	defer c2.Close()
	if res, err := c2.Run(ctx, "true"); err != nil || res.ExitCode != 0 {
		return HardenResult{}, errors.New("托管密钥可连接但无法执行命令")
	}
	log("托管密钥验证通过：后续操作不再需要密码")

	return HardenResult{KeyPair: kp, SSHUser: target.User}, nil
}

// SudoPrefix返回执行特权命令的前缀。
//
// 免密sudo时用`sudo`；需要密码时用`sudo -S`并从stdin喂密码——
// **密码只出现在这个进程内存与SSH加密通道里**，不落库、不进日志
// （sshexec的输出脱敏会把`echo '...' | sudo -S`形态也抹掉，见internal/redact）。
func SudoPrefix(user, sudoPassword string) string {
	if user == "root" {
		return ""
	}
	if sudoPassword == "" {
		return "sudo -n "
	}
	return "echo " + shellQuote(sudoPassword) + " | sudo -S -p '' "
}

// DetectSudo探测目标用户能否取得root：root用户直接true，
// 否则先试免密sudo，再试带密码的sudo。
func DetectSudo(ctx context.Context, cli Runner, user, sudoPassword string) (needPassword bool, err error) {
	if user == "root" {
		return false, nil
	}
	res, err := cli.Run(ctx, "sudo -n true")
	if err != nil {
		return false, err
	}
	if res.ExitCode == 0 {
		return false, nil
	}
	if sudoPassword == "" {
		return false, errors.New("该用户需要sudo密码，但未提供；或该用户不在sudoers中")
	}
	res, err = cli.Run(ctx, "echo "+shellQuote(sudoPassword)+" | sudo -S -p '' true")
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		// 不做任何猜测性修复（§9.1）：明确失败比半自动改sudoers安全。
		return false, errors.New("sudo校验失败：该用户可能不在sudoers中，或密码不正确")
	}
	return true, nil
}

// shellQuote用单引号安全包裹参数（含公钥、密码这类不可信/敏感内容）。
// 单引号内除了单引号本身没有任何转义，因此只需处理单引号。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ZeroString尽力清除内存中的密码副本。
//
// **这是尽力而为，不是保证**：Go的字符串不可变，无法原地清零；真正的销毁靠
// "密码从不被复制到长生命周期对象里"（不落库、不进日志、不放进结构体字段）。
// 保留本函数是为了让调用点显式表达"用完即弃"的意图。
func ZeroString(s *string) {
	if s != nil {
		*s = ""
	}
}

// DefaultDialer是生产用的Dialer（固定超时）。
func DefaultDialer(timeout time.Duration) Dialer {
	return func(ctx context.Context, t sshexec.Target, auth []ssh.AuthMethod) (*sshexec.Client, error) {
		return sshexec.Dial(ctx, t, auth, timeout)
	}
}

// sha256Hex是push-artifacts的precheck用的内容摘要（与节点上sha256sum口径一致）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sortedKeys让上传顺序稳定，日志与重跑可比对。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
