package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"ccw/internal/consolestore"
)

// releaseRegistrar是register-release需要的存储能力面（单测注入假实现）。
type releaseRegistrar interface {
	RegisterRelease(ctx context.Context, version, notes string, arts []consolestore.Artifact) error
	Publish(ctx context.Context, version string) error
}

// releaseTargets是发布流水线的六个目标（设计§3.2）。
var releaseTargets = [][2]string{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

// artifactFilename是产物命名约定：cclaude_{version}_{os}_{arch}[.exe]。
// build-release.sh按同一约定产出；两处改任何一处都必须同步。
func artifactFilename(version, osName, arch string) string {
	name := fmt.Sprintf("cclaude_%s_%s_%s", version, osName, arch)
	if osName == "windows" {
		name += ".exe"
	}
	return name
}

// scanArtifacts按命名约定扫描dist目录，对存在的产物计算sha256与大小。
// 返回找到的产物与缺失的目标列表——缺失不吞掉（no silent caps）：
// 六个目标只发布了四个时，调用方必须把缺口打印出来。
func scanArtifacts(dir, version string) (arts []consolestore.Artifact, missing []string, err error) {
	for _, t := range releaseTargets {
		osName, arch := t[0], t[1]
		name := artifactFilename(version, osName, arch)
		f, oerr := os.Open(filepath.Join(dir, name))
		if oerr != nil {
			if os.IsNotExist(oerr) {
				missing = append(missing, osName+"/"+arch)
				continue
			}
			return nil, nil, oerr
		}
		h := sha256.New()
		n, cerr := io.Copy(h, f)
		f.Close()
		if cerr != nil {
			return nil, nil, fmt.Errorf("读%s失败: %w", name, cerr)
		}
		arts = append(arts, consolestore.Artifact{
			Version: version, OS: osName, Arch: arch, Filename: name,
			SizeBytes: n, SHA256: hex.EncodeToString(h.Sum(nil)),
		})
	}
	if len(arts) == 0 {
		return nil, nil, fmt.Errorf("目录%s中没有版本%s的任何产物（期望cclaude_%s_<os>_<arch>命名，先跑 make release VERSION=%s）",
			dir, version, version, version)
	}
	return arts, missing, nil
}

// runRegisterRelease把构建产物登记进releases/release_artifacts（设计§3.2）。
// 未发布（无--publish）的版本对下载页完全不可见——半成品保护由表状态保证。
func runRegisterRelease(args []string, stdout, stderr io.Writer, st releaseRegistrar, defaultDir string) int {
	fs := flag.NewFlagSet("register-release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "版本号（必填，与构建时VERSION一致）")
	notes := fs.String("notes", "", "版本说明（可选）")
	publish := fs.Bool("publish", false, "登记后立即发布（下载页可见）")
	dir := fs.String("dir", defaultDir, "产物目录（默认CCW_DIST_DIR）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *version == "" {
		fmt.Fprintln(stderr, "register-release: --version 必填")
		return 2
	}

	arts, missing, err := scanArtifacts(*dir, *version)
	if err != nil {
		fmt.Fprintln(stderr, "register-release:", err)
		return 1
	}
	ctx := context.Background()
	if err := st.RegisterRelease(ctx, *version, *notes, arts); err != nil {
		fmt.Fprintln(stderr, "register-release:", err)
		return 1
	}
	for _, a := range arts {
		fmt.Fprintf(stdout, "登记 %-40s %10d bytes  sha256=%s\n", a.Filename, a.SizeBytes, a.SHA256)
	}
	for _, m := range missing {
		fmt.Fprintf(stdout, "警告：缺少目标 %s 的产物——该平台将无法下载此版本\n", m)
	}
	if *publish {
		if err := st.Publish(ctx, *version); err != nil {
			fmt.Fprintln(stderr, "register-release: publish:", err)
			return 1
		}
		fmt.Fprintf(stdout, "已发布：%s（%d个产物，下载页立即可见）\n", *version, len(arts))
	} else {
		fmt.Fprintf(stdout, "已登记未发布：%s（%d个产物）。确认无误后：ccw-console register-release --version %s --publish\n",
			*version, len(arts), *version)
	}
	return 0
}
