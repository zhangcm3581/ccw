package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	AudSession  = "session"
	AudTerminal = "terminal"
	AudSync     = "sync"
)

var (
	ErrInvalid  = errors.New("token: invalid")
	ErrExpired  = errors.New("token: expired")
	ErrAudience = errors.New("token: audience mismatch")
)

type Claims struct {
	ProjectID string    `json:"p"`
	Audience  string    `json:"a"`
	ExpiresAt time.Time `json:"e"`
}

func Mint(key []byte, projectID, audience string, ttl time.Duration, now time.Time) (string, error) {
	body, err := json.Marshal(Claims{ProjectID: projectID, Audience: audience, ExpiresAt: now.Add(ttl).UTC()})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + sign(key, payload), nil
}

func Verify(key []byte, tok, audience string, now time.Time) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 2 || !hmac.Equal([]byte(sign(key, parts[0])), []byte(parts[1])) {
		return Claims{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrInvalid
	}
	if now.After(c.ExpiresAt) {
		return Claims{}, ErrExpired
	}
	if c.Audience != audience {
		return Claims{}, ErrAudience
	}
	return c, nil
}

func sign(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
