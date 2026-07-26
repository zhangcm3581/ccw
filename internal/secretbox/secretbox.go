// Package secretbox是Console的信封加密工具（console-fleet-design的C2、§8.4）。
//
// 用途：托管SSH私钥、AWS凭据（Route 53）、TOTP secret落库前的加密。
// 目标服务器密码**不属于**本包的用途——它永不落库（§8.4），只在harden步骤的
// 进程内存里存在。
//
// 密钥来自CCW_SECRET_KEY（32字节hex），遵循「缺失即硬失败、无默认值」。
// 该密钥丢失＝全部托管私钥与凭据不可解，需对每台机器重新纳管（设计§14的风险表），
// 因此部署runbook必须写明它的备份要求。
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	KeySize   = 32 // AES-256
	NonceSize = 12 // GCM标准nonce长度
)

// ErrDecrypt是所有解密失败的统一错误：密钥错、密文被篡改、nonce被换、
// 上下文不匹配都返回它——不区分原因，也**绝不携带密文或明文片段**
// （CLAUDE.md：凭据永不进日志与错误信息）。
var ErrDecrypt = errors.New("secretbox: decrypt failed")

type Box struct{ aead cipher.AEAD }

// New构造加密器。密钥必须恰好32字节，短密钥一律拒绝——
// 绝不接受"不够长就补零"这类静默降级。
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// ParseKeyHex解析CCW_SECRET_KEY（64位hex＝32字节）。
func ParseKeyHex(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("secretbox: CCW_SECRET_KEY is required (64 hex chars); no default is provided")
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, errors.New("secretbox: CCW_SECRET_KEY must be hex-encoded")
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: CCW_SECRET_KEY must decode to %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// Seal加密plaintext，返回密文与随机nonce（分列存储，对应schema的_enc/_nonce两列）。
//
// context作为AAD绑定用途（如"node-cred"、"zone-cred"、"totp"）：把某一列的密文
// 搬到另一列冒充时解密会失败。它不是机密，只是用途标签。
func (b *Box) Seal(plaintext []byte, context string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secretbox: read nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, plaintext, []byte(context)), nonce, nil
}

// Open解密；任何失败都返回ErrDecrypt，不透露具体原因。
func (b *Box) Open(ciphertext, nonce []byte, context string) ([]byte, error) {
	if len(nonce) != NonceSize {
		return nil, ErrDecrypt
	}
	plain, err := b.aead.Open(nil, nonce, ciphertext, []byte(context))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
