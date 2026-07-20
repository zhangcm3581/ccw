package sync

import "testing"

func TestSafeRelPath(t *testing.T) {
	bad := []string{"/etc/passwd", "../x", "a/../../x", "a\\..\\x", "", "a/../b"}
	for _, p := range bad {
		if _, err := SafeRelPath(p); err == nil {
			t.Fatalf("must reject %q", p)
		}
	}
	got, err := SafeRelPath("a\\b\\c.txt") // Windows客户端路径统一为forward-slash
	if err != nil || got != "a/b/c.txt" {
		t.Fatalf("normalize failed: %q %v", got, err)
	}
}

func TestDefaultExcluded(t *testing.T) {
	for _, p := range []string{".env", ".cclaude/index.db", ".ssh/id_rsa", ".aws/credentials", ".claude/creds.json"} {
		if !DefaultExcluded(p) {
			t.Fatalf("%q must be excluded by default", p)
		}
	}
	if DefaultExcluded("src/main.go") {
		t.Fatal("normal file must not be excluded")
	}
}
