package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccw/internal/consolestore"
)

type fakeRegistrar struct {
	registered []consolestore.Artifact
	version    string
	published  []string
}

func (f *fakeRegistrar) RegisterRelease(_ context.Context, version, notes string, arts []consolestore.Artifact) error {
	f.version, f.registered = version, arts
	return nil
}
func (f *fakeRegistrar) Publish(_ context.Context, version string) error {
	f.published = append(f.published, version)
	return nil
}

func writeArtifact(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestScanArtifacts(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "cclaude_v0.1.0_linux_amd64", "AAA")
	writeArtifact(t, dir, "cclaude_v0.1.0_windows_amd64.exe", "BBBB")
	writeArtifact(t, dir, "cclaude_v9.9.9_linux_amd64", "其他版本不该被扫进来")

	arts, missing, err := scanArtifacts(dir, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("应找到2个产物，got %d", len(arts))
	}
	wantSHA := sha256.Sum256([]byte("AAA"))
	if arts[0].Filename != "cclaude_v0.1.0_linux_amd64" || arts[0].SizeBytes != 3 ||
		arts[0].SHA256 != hex.EncodeToString(wantSHA[:]) {
		t.Errorf("linux产物字段错误: %+v", arts[0])
	}
	if len(missing) != 4 {
		t.Errorf("六目标缺四个，missing=%v", missing)
	}

	if _, _, err := scanArtifacts(dir, "v0.0.0"); err == nil {
		t.Error("一个产物都没有时应报错")
	}
}

func TestRegisterReleaseFlow(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "cclaude_v0.2.0_linux_amd64", "bin")
	f := &fakeRegistrar{}
	var out, errBuf bytes.Buffer

	code := runRegisterRelease([]string{"--version", "v0.2.0"}, &out, &errBuf, f, dir)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errBuf.String())
	}
	if f.version != "v0.2.0" || len(f.registered) != 1 {
		t.Errorf("登记错误: %s %d", f.version, len(f.registered))
	}
	if len(f.published) != 0 {
		t.Error("无--publish不得发布")
	}
	// 缺失目标必须打出来（no silent caps）。
	if !strings.Contains(out.String(), "缺少目标") {
		t.Error("应警告缺失的平台目标")
	}

	code = runRegisterRelease([]string{"--version", "v0.2.0", "--publish"}, &out, &errBuf, f, dir)
	if code != 0 || len(f.published) != 1 {
		t.Errorf("--publish应触发发布: exit=%d published=%v", code, f.published)
	}
}

func TestRegisterReleaseRequiresVersion(t *testing.T) {
	f := &fakeRegistrar{}
	var out, errBuf bytes.Buffer
	if code := runRegisterRelease(nil, &out, &errBuf, f, t.TempDir()); code == 0 {
		t.Fatal("缺--version应失败")
	}
	if !strings.Contains(errBuf.String(), "--version") {
		t.Errorf("错误应提示--version，got %s", errBuf.String())
	}
}
