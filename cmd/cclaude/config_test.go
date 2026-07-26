package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// C14客户端寻址改造（console-fleet-design §6.7/§11.2）：
// 域名不写死进二进制，~/.ccw/config.json(0600)取代旧的单行cdk文件。

func noPrompt(t *testing.T) func(string) (string, error) {
	return func(string) (string, error) {
		t.Helper()
		t.Fatal("不应走到交互提示")
		return "", nil
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadLocalConfigMissing(t *testing.T) {
	c, err := loadLocalConfig(t.TempDir())
	if err != nil {
		t.Fatalf("配置不存在不是错误: %v", err)
	}
	if c.API != "" || c.CDK != "" {
		t.Errorf("空配置应为零值: %+v", c)
	}
}

// A22：config.json权限0600、目录0700——CDK在里面，权限即安全边界。
func TestSaveLocalConfigPermsAndRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	if err := saveLocalConfig(dir, localConfig{API: "https://api-01.example.com", CDK: "ccw_a.b"}); err != nil {
		t.Fatal(err)
	}
	c, err := loadLocalConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.API != "https://api-01.example.com" || c.CDK != "ccw_a.b" {
		t.Errorf("读回不一致: %+v", c)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "config.json"))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("config.json权限=%o，want 0600（A22）", fi.Mode().Perm())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Errorf("目录权限=%o，want 0700", di.Mode().Perm())
		}
	}
}

// A23：旧的~/.ccw/cdk单行文件自动迁移为config.json且不丢CDK，迁移后删除旧文件。
func TestLegacyCDKMigration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cdk"), []byte("ccw_legacy.secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadLocalConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.CDK != "ccw_legacy.secret" {
		t.Errorf("迁移应保留CDK（去除换行），got %q", c.CDK)
	}
	if _, err := os.Stat(filepath.Join(dir, "cdk")); !os.IsNotExist(err) {
		t.Error("迁移后旧cdk文件应被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Error("迁移后应生成config.json")
	}
	// 再次加载：从config.json读，不再依赖旧文件。
	c2, err := loadLocalConfig(dir)
	if err != nil || c2.CDK != "ccw_legacy.secret" {
		t.Errorf("二次加载失败: %+v, %v", c2, err)
	}
}

// config.json已存在时忽略旧cdk文件（不迁移、不删除——不动用户既有文件更安全）。
func TestLegacyIgnoredWhenConfigExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	if err := saveLocalConfig(dir, localConfig{API: "https://x.example.com", CDK: "ccw_new.s"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "cdk"), []byte("ccw_old.s"), 0o600)
	c, err := loadLocalConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.CDK != "ccw_new.s" {
		t.Errorf("config.json应优先于旧文件，got %q", c.CDK)
	}
	if _, err := os.Stat(filepath.Join(dir, "cdk")); err != nil {
		t.Error("config.json存在时不应动旧文件")
	}
}

// 寻址优先级（§6.7）：--api > CCW_API > config.json > 交互提示。
func TestResolveAPIPrecedence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	saveLocalConfig(dir, localConfig{API: "https://from-file.example.com"})

	// --api最高，且显式指定时回写本地配置（支持换节点）；裸域名归一化补/api。
	got, err := resolveAPI(dir, "https://from-flag.example.com", envOf(map[string]string{"CCW_API": "https://from-env.example.com"}), noPrompt(t))
	if err != nil || got != "https://from-flag.example.com/api" {
		t.Fatalf("flag应最优先: %q %v", got, err)
	}
	c, _ := loadLocalConfig(dir)
	if c.API != "https://from-flag.example.com/api" {
		t.Errorf("--api应回写本地配置（归一化后），got %q", c.API)
	}

	// 环境变量次之，但**不**回写（会话级覆盖，不是换节点）。
	got, err = resolveAPI(dir, "", envOf(map[string]string{"CCW_API": "https://from-env.example.com"}), noPrompt(t))
	if err != nil || got != "https://from-env.example.com/api" {
		t.Fatalf("env应次优先: %q %v", got, err)
	}
	if c, _ := loadLocalConfig(dir); c.API != "https://from-flag.example.com/api" {
		t.Errorf("env不应回写配置，got %q", c.API)
	}

	// 都没有时读文件。
	got, err = resolveAPI(dir, "", envOf(nil), noPrompt(t))
	if err != nil || got != "https://from-flag.example.com/api" {
		t.Fatalf("file应第三优先: %q %v", got, err)
	}
}

// 全新安装无任何写死域名（A22前半）：走交互提示，输入后持久化。
func TestResolveAPIFirstRunPrompts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	prompted := false
	got, err := resolveAPI(dir, "", envOf(nil), func(msg string) (string, error) {
		prompted = true
		return "https://api-03.example.com/", nil // 带尾斜杠，应被归一
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prompted {
		t.Fatal("首次运行应交互提示，而不是回落到任何写死域名")
	}
	if got != "https://api-03.example.com/api" {
		t.Errorf("应归一化（去尾斜杠、补/api），got %q", got)
	}
	if c, _ := loadLocalConfig(dir); c.API != "https://api-03.example.com/api" {
		t.Errorf("首次输入应持久化，got %q", c.API)
	}
}

func TestNormalizeAPI(t *testing.T) {
	ok := map[string]string{
		// 裸域名/IP：自动补/api（公开路径合同/api/*；设计§6.6示例即裸域名）
		"https://api-01.example.com":  "https://api-01.example.com/api",
		"https://api-01.example.com/": "https://api-01.example.com/api",
		"http://203.0.113.7":          "http://203.0.113.7/api", // IP+HTTP测试模式（DEPLOY.md第9节）
		" https://x.example.com ":     "https://x.example.com/api",
		// 显式路径原样保留（兼容旧的CCW_API=https://域名/api写法）
		"https://x.example.com/api":  "https://x.example.com/api",
		"https://x.example.com/api/": "https://x.example.com/api",
	}
	for in, want := range ok {
		got, err := normalizeAPI(in)
		if err != nil || got != want {
			t.Errorf("normalizeAPI(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x.example.com", "api-01.example.com", "https://", "://x"} {
		if _, err := normalizeAPI(bad); err == nil {
			t.Errorf("normalizeAPI(%q)应报错", bad)
		}
	}
}

// CDK优先级：CCW_CDK > config.json > 交互；交互输入后持久化且保留API字段。
func TestResolveCDKPrecedenceAndPersist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	saveLocalConfig(dir, localConfig{API: "https://keep.example.com"})

	got, err := resolveCDK(dir, envOf(map[string]string{"CCW_CDK": "ccw_env.s"}), noPrompt(t))
	if err != nil || got != "ccw_env.s" {
		t.Fatalf("env应最优先: %q %v", got, err)
	}
	if c, _ := loadLocalConfig(dir); c.CDK != "" {
		t.Error("env提供的CDK不应写盘")
	}

	got, err = resolveCDK(dir, envOf(nil), func(string) (string, error) { return " ccw_typed.s\n", nil })
	if err != nil || got != "ccw_typed.s" {
		t.Fatalf("交互输入: %q %v", got, err)
	}
	c, _ := loadLocalConfig(dir)
	if c.CDK != "ccw_typed.s" || c.API != "https://keep.example.com" {
		t.Errorf("交互输入应持久化且保留API字段: %+v", c)
	}

	got, err = resolveCDK(dir, envOf(nil), noPrompt(t))
	if err != nil || got != "ccw_typed.s" {
		t.Fatalf("file应次优先: %q %v", got, err)
	}
}

// 认证失败时只清CDK、保留API——用户重输CDK即可，不必重配域名。
func TestClearCDKKeepsAPI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	saveLocalConfig(dir, localConfig{API: "https://keep.example.com", CDK: "ccw_bad.s"})
	if err := clearCDKField(dir); err != nil {
		t.Fatal(err)
	}
	c, _ := loadLocalConfig(dir)
	if c.CDK != "" || c.API != "https://keep.example.com" {
		t.Errorf("应只清CDK: %+v", c)
	}
}

// logout：清除全部本地配置（含旧格式文件）；对不存在的配置幂等。
func TestLogoutClearsAll(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	saveLocalConfig(dir, localConfig{API: "https://x.example.com", CDK: "ccw_a.b"})
	os.WriteFile(filepath.Join(dir, "cdk"), []byte("ccw_old.s"), 0o600)
	if err := clearLocalConfig(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Error("config.json应被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "cdk")); !os.IsNotExist(err) {
		t.Error("旧cdk文件应被删除")
	}
	if err := clearLocalConfig(dir); err != nil {
		t.Errorf("重复logout应幂等: %v", err)
	}
}

// 源码级守卫（A24的静态面）：CDK绝不做命令行参数——命令行参数会进shell history
// 与ps输出。这里断言main包没有定义任何cdk相关的flag。
func TestNoCDKFlagDefined(t *testing.T) {
	// resolveCDK的输入面只有env与prompt；若有人加了--cdk flag，这个测试提醒他读设计§6.7。
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Skip("源码不可读")
	}
	for _, forbidden := range []string{`flag.String("cdk"`, `StringVar(&cdk`, `"cdk",`} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("main.go出现%q——CDK不得做命令行参数（设计§6.7，A24）", forbidden)
		}
	}
}

// 迁移遇到不可解析的config.json：如实报错，不静默清空（那会丢用户的CDK）。
func TestLoadCorruptConfigErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ccw")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600)
	if _, err := loadLocalConfig(dir); err == nil {
		t.Error("损坏的config.json应报错而非静默返回空配置")
	}
}
