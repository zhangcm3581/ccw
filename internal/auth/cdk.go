package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

var ErrMalformedCDK = errors.New("auth: malformed cdk")

// NewCDK生成ccw_<public-id>.<secret>；public-id用于数据库O(1)检索，secret参与Argon2id验证。
func NewCDK() (plain, publicID string, err error) {
	pub := make([]byte, 8)
	sec := make([]byte, 32)
	if _, err := rand.Read(pub); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(sec); err != nil {
		return "", "", err
	}
	publicID = hex.EncodeToString(pub)
	return "ccw_" + publicID + "." + hex.EncodeToString(sec), publicID, nil
}

func SplitCDK(plain string) (publicID, secret string, err error) {
	body, ok := strings.CutPrefix(plain, "ccw_")
	if !ok {
		return "", "", ErrMalformedCDK
	}
	publicID, secret, ok = strings.Cut(body, ".")
	if !ok || publicID == "" || secret == "" {
		return "", "", ErrMalformedCDK
	}
	return publicID, secret, nil
}

func HashSecret(secret string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(h)), nil
}

func VerifySecret(plain, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(plain), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
