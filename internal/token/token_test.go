package token

import (
	"testing"
	"time"
)

var key = make([]byte, 32)

func TestMintVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, err := Mint(key, "pa", AudTerminal, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Verify(key, tok, AudTerminal, now.Add(14*time.Minute))
	if err != nil || c.ProjectID != "pa" {
		t.Fatalf("want pa, got %+v err=%v", c, err)
	}
}

func TestExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, _ := Mint(key, "pa", AudSync, 15*time.Minute, now)
	if _, err := Verify(key, tok, AudSync, now.Add(16*time.Minute)); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestAudienceMismatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, _ := Mint(key, "pa", AudSync, time.Minute, now)
	if _, err := Verify(key, tok, AudTerminal, now); err != ErrAudience {
		t.Fatalf("sync token must not open terminal, got %v", err)
	}
}

func TestTampered(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, _ := Mint(key, "pa", AudSync, time.Minute, now)
	bad := tok[:len(tok)-2] + "!!"
	if _, err := Verify(key, bad, AudSync, now); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}
