package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// FROM 里用到的 ARG 必须在**第一个 FROM 之前**声明。
//
// 这是 Dockerfile 的硬规则：只有出现在第一个 FROM 之前的 ARG 才是全局的，
// 写在某个阶段里就只属于那个阶段。2026-08-01 给 Dockerfile.claude 加状态行的
// 构建阶段时把 `ARG UBUNTU_TAG=24.04` 挤到了新阶段后面，于是
// `FROM ubuntu:${UBUNTU_TAG}` 解析成 `ubuntu:`，报
// "failed to parse stage name: invalid reference format"——整个 compose-up
// 就是这样失败的，而**不构建根本看不出来**。
func TestDockerfileGlobalArgsDeclaredBeforeFirstFrom(t *testing.T) {
	files, err := filepath.Glob("../../deploy/Dockerfile.*")
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 Dockerfile：%v", err)
	}
	argRe := regexp.MustCompile(`^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)`)
	fromRe := regexp.MustCompile(`^\s*FROM\s+(\S+)`)
	varRe := regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		global := map[string]bool{}
		seenFrom := false
		for i, line := range strings.Split(string(b), "\n") {
			// 注释里也可能出现 FROM/ARG（比如解释这条规则的注释本身），
			// 不跳过的话会误报——写这条测试时就先踩了一次。
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := fromRe.FindStringSubmatch(line); m != nil {
				for _, v := range varRe.FindAllStringSubmatch(m[1], -1) {
					if !global[v[1]] {
						t.Errorf("%s:%d FROM 用了 %s，但它不是全局 ARG"+
							"（必须声明在第一个 FROM 之前，否则解析成空值）：%s",
							filepath.Base(f), i+1, v[1], strings.TrimSpace(line))
					}
				}
				seenFrom = true
				continue
			}
			if m := argRe.FindStringSubmatch(line); m != nil && !seenFrom {
				global[m[1]] = true
			}
		}
	}
}
