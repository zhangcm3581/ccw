package sshexec

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// C6凭据生命周期的密钥部分（console-fleet-design §9）。
//
// 流程：首登用密码 → 生成ed25519密钥对 → 公钥注入节点 → 用私钥重新拨号验证
// → 验证通过才把私钥加密落库 → 密码缓冲区置零、永不落库。
// 顺序不可颠倒：先落库后验证会在注入失败时留下一把连不上的"托管密钥"。

// KeyPair是新生成的托管密钥。PrivatePEM需经信封加密后落库（§8.4）。
type KeyPair struct {
	PrivatePEM    []byte // OpenSSH格式私钥
	AuthorizedKey string // authorized_keys里的一行（含注释）
}

// GenerateKeyPair生成ed25519托管密钥。
// 选ed25519而不是RSA：密钥短、生成快、无参数可选错（RSA还要选位数）。
func GenerateKeyPair(comment string) (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sshexec: 生成密钥: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sshexec: 序列化私钥: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return KeyPair{}, fmt.Errorf("sshexec: 公钥: %w", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return KeyPair{PrivatePEM: pem.EncodeToMemory(block), AuthorizedKey: line}, nil
}

// AuthFromPrivateKey把私钥PEM转成ssh.AuthMethod（解密落库的私钥后使用）。
func AuthFromPrivateKey(pem []byte) (ssh.AuthMethod, error) {
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		// 错误可能带出密钥内容的片段，不外传细节。
		return nil, fmt.Errorf("sshexec: 私钥无法解析")
	}
	return ssh.PublicKeys(signer), nil
}
