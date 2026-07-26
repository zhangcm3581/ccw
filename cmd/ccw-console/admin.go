package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"ccw/internal/auth"
	"ccw/internal/consolestore"
	"ccw/internal/secretbox"
	"ccw/internal/totp"
)

// adminCreator是create-admin需要的存储能力面。
type adminCreator interface {
	CountAdmins(ctx context.Context) (int, error)
	CreateAdmin(ctx context.Context, username, passwordHash string, totpEnc, totpNonce []byte) (string, error)
}

// runCreateAdmin创建管理员账号（console-fleet-design §8.1）。
//
// 密码只走交互输入或环境变量，**不接受命令行参数**——参数会进shell history，
// 在Linux上还会出现在ps输出里（与CDK同一条规则，设计§6.7）。
// TOTP secret当场生成、以信封加密落库，明文只在本次输出里出现一次。
func runCreateAdmin(args []string, stdout, stderr io.Writer, st adminCreator, box *secretbox.Box,
	readPassword func(prompt string) (string, error)) int {
	fs := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("username", "", "管理员用户名（必填）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *username == "" {
		fmt.Fprintln(stderr, "create-admin: --username 必填")
		return 2
	}

	ctx := context.Background()
	if _, err := st.CountAdmins(ctx); err != nil {
		fmt.Fprintln(stderr, "create-admin:", err)
		return 1
	}

	password := os.Getenv("CCW_ADMIN_PASSWORD")
	if password == "" {
		p1, err := readPassword("设置密码: ")
		if err != nil {
			fmt.Fprintln(stderr, "create-admin:", err)
			return 1
		}
		p2, err := readPassword("再输一次: ")
		if err != nil {
			fmt.Fprintln(stderr, "create-admin:", err)
			return 1
		}
		if p1 != p2 {
			fmt.Fprintln(stderr, "create-admin: 两次输入不一致")
			return 2
		}
		password = p1
	}
	password = strings.TrimSpace(password)
	// 管理后台权限等同全机队root，弱口令没有商量余地。
	if len(password) < 12 {
		fmt.Fprintln(stderr, "create-admin: 密码至少12个字符（管理后台权限等同全机队root）")
		return 2
	}

	hash, err := auth.HashSecret(password)
	if err != nil {
		fmt.Fprintln(stderr, "create-admin:", err)
		return 1
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		fmt.Fprintln(stderr, "create-admin:", err)
		return 1
	}
	enc, nonce, err := box.Seal([]byte(secret), adminTOTPContext)
	if err != nil {
		fmt.Fprintln(stderr, "create-admin:", err)
		return 1
	}
	if _, err := st.CreateAdmin(ctx, *username, hash, enc, nonce); err != nil {
		fmt.Fprintln(stderr, "create-admin:", err)
		return 1
	}

	fmt.Fprintf(stdout, "管理员已创建：%s\n\n", *username)
	fmt.Fprintln(stdout, "两步验证密钥（只显示一次，立即添加到认证器App）:")
	fmt.Fprintf(stdout, "  密钥: %s\n", secret)
	fmt.Fprintf(stdout, "  链接: %s\n\n", totp.ProvisioningURI(secret, *username, "ccw Console"))
	fmt.Fprintln(stdout, "添加后请立即用它登录一次验证；丢失该密钥需要重建账号。")
	return 0
}

// adminTOTPContext必须与internal/console里解密时用的AAD一致，
// 否则登录时解不开TOTP secret。两处都用这个常量值。
const adminTOTPContext = "admin-totp"

// readPasswordTTY从终端读密码（不回显）。
func readPasswordTTY(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// 编译期断言：*consolestore.Store满足create-admin的需要。
var _ adminCreator = (*consolestore.Store)(nil)
