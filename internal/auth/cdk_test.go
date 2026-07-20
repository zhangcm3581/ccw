package auth

import (
	"strings"
	"testing"
)

func TestNewCDKFormatAndSplit(t *testing.T) {
	plain, pub, err := NewCDK()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "ccw_") || !strings.Contains(plain, ".") {
		t.Fatalf("cdk must look like ccw_<public>.<secret>: %q", plain)
	}
	gotPub, secret, err := SplitCDK(plain)
	if err != nil || gotPub != pub || secret == "" {
		t.Fatalf("split failed: %q %q %v", gotPub, secret, err)
	}
	if _, _, err := SplitCDK("ccw_nosecret"); err == nil {
		t.Fatal("cdk without secret part must be rejected")
	}
}

func TestSecretHashRoundTrip(t *testing.T) {
	plain, _, _ := NewCDK()
	_, secret, _ := SplitCDK(plain)
	enc, err := HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, secret) {
		t.Fatal("encoded hash must not contain plaintext secret")
	}
	if !VerifySecret(secret, enc) {
		t.Fatal("verify must succeed for correct secret")
	}
	if VerifySecret(secret+"x", enc) {
		t.Fatal("verify must fail for wrong secret")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashSecret("same-secret")
	b, _ := HashSecret("same-secret")
	if a == b {
		t.Fatal("two hashes of the same secret must differ (random salt)")
	}
}
