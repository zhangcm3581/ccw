# 双项目远程Claude工作空间Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建同一管理员使用的双项目远程Claude Code工作空间：CDK登录、容器隔离、tmux会话保持、双向清单同步、磁盘配额与5小时/7天内部额度闸门。

**Architecture:** Go单仓三个二进制——`cclaude`本地CLI（三平台）、`control-api`（CDK/令牌/额度/门户）、`worker-agent`（Docker编排、WebSocket终端与同步、JSONL用量采集）；PostgreSQL存元数据；每项目一个容器+三个命名卷；短期连接令牌用HMAC签名不落库。

**Tech Stack:** Go≥1.22、PostgreSQL、pgx/v5、golang.org/x/crypto（argon2）、gorilla/websocket、fsnotify、golang.org/x/term、creack/pty、Docker Engine、tmux、systemd。

**Spec:** `docs/superpowers/specs/2026-07-19-remote-claude-workspace-design.md`（本计划一切以该spec为准）

## Global Constraints

- Go版本≥1.22；开发机需先安装Go（Task 1 Step 0），Docker仅在目标VPS要求。
- 数据库中永远不存CDK明文，只存Argon2id哈希；日志与错误信息不得输出CDK、令牌或OAuth凭据。
- 同步路径一律为forward-slash的UTF-8相对路径；拒绝绝对路径、`..`、项目外符号链接。
- 所有时间窗口计算用数据库`now()`与UTC。
- 用量单位对外一律称"内部额度单位"，不得标注为官方订阅百分比。
- 每个任务TDD：先写失败测试，再实现，`go test ./... `全绿后提交。
- 中文文档遵守中英文之间无空格的排版规则（代码块除外；不要对本文件跑clean_cjk_spaces.py，会破坏命令语法）。
- 仓库根：`/root/code1/remote-claude-workspace/`；所有路径相对仓库根。

---

### Task 1: Go工具链、项目骨架与配置加载

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `cmd/control-api/main.go`
- Create: `cmd/worker-agent/main.go`
- Create: `cmd/cclaude/main.go`

**Interfaces:**
- Produces: `config.Load(getenv func(string) string) (Config, error)`；`Config{DatabaseURL string; TokenSigningKey []byte; WorkspaceRoot string; ListenAddr string; AgentListenAddr string; AdminSocketPath string; ClaudeImage string}`。TokenSigningKey由环境变量`CCW_TOKEN_KEY`（hex，≥32字节）解码，无默认值。

- [ ] **Step 0: 安装Go工具链（开发机当前没有Go）**

```bash
curl -fsSL https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -o /tmp/claude-0/-root/efa99086-5fe4-4de1-9a62-c41b8e8d35ef/scratchpad/go.tgz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/claude-0/-root/efa99086-5fe4-4de1-9a62-c41b8e8d35ef/scratchpad/go.tgz
export PATH=$PATH:/usr/local/go/bin && go version
```

Expected: `go version go1.22.5 linux/amd64`（如无外网，使用系统包管理器`apt install golang-go`并确认≥1.22）

- [ ] **Step 1: 初始化模块并写失败测试**

```bash
cd /root/code1/remote-claude-workspace && go mod init ccw
```

`internal/config/config_test.go`：

```go
package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"CCW_DATABASE_URL":  "postgres://ccw:pw@localhost:5432/ccw",
		"CCW_TOKEN_KEY":     "8ba7167acf1c9ee1cbfbcbf0b2c7e51ecdf8b1d0a9b3c2d1e0f1a2b3c4d5e6f7",
		"CCW_WORKSPACE_ROOT": "/srv/ccw",
	}
}

func TestLoadValid(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.TokenSigningKey) != 32 {
		t.Fatalf("want 32-byte key, got %d", len(c.TokenSigningKey))
	}
	if c.ListenAddr != ":8080" || c.AgentListenAddr != ":8081" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestLoadMissingEach(t *testing.T) {
	for _, k := range []string{"CCW_DATABASE_URL", "CCW_TOKEN_KEY", "CCW_WORKSPACE_ROOT"} {
		m := valid()
		delete(m, k)
		if _, err := Load(env(m)); err == nil {
			t.Fatalf("missing %s: want error, got nil", k)
		}
	}
}

func TestLoadShortKeyRejected(t *testing.T) {
	m := valid()
	m["CCW_TOKEN_KEY"] = "abcd"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("short key must be rejected")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/config -v`
Expected: FAIL（`Load`未定义，编译错误）

- [ ] **Step 3: 实现配置加载**

`internal/config/config.go`：

```go
package config

import (
	"encoding/hex"
	"fmt"
)

type Config struct {
	DatabaseURL     string
	TokenSigningKey []byte
	WorkspaceRoot   string
	ListenAddr      string
	AgentListenAddr string
	AdminSocketPath string
	ClaudeImage     string
}

func Load(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:     getenv("CCW_DATABASE_URL"),
		WorkspaceRoot:   getenv("CCW_WORKSPACE_ROOT"),
		ListenAddr:      or(getenv("CCW_LISTEN_ADDR"), ":8080"),
		AgentListenAddr: or(getenv("CCW_AGENT_LISTEN_ADDR"), ":8081"),
		AdminSocketPath: or(getenv("CCW_ADMIN_SOCKET"), "/run/ccw/admin.sock"),
		ClaudeImage:     or(getenv("CCW_CLAUDE_IMAGE"), "ccw-claude:latest"),
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: CCW_DATABASE_URL is required")
	}
	if c.WorkspaceRoot == "" {
		return Config{}, fmt.Errorf("config: CCW_WORKSPACE_ROOT is required")
	}
	keyHex := getenv("CCW_TOKEN_KEY")
	if keyHex == "" {
		return Config{}, fmt.Errorf("config: CCW_TOKEN_KEY is required (hex, >=32 bytes); no default is provided")
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) < 32 {
		return Config{}, fmt.Errorf("config: CCW_TOKEN_KEY must be hex-encoded and at least 32 bytes")
	}
	c.TokenSigningKey = key
	return c, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
```

三个`main.go`先做最小入口（以`cmd/control-api/main.go`为例，另两个把名字换掉）：

```go
package main

import (
	"fmt"
	"os"

	"ccw/internal/config"
)

func main() {
	if _, err := config.Load(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("control-api: config ok")
}
```

- [ ] **Step 4: 运行测试与整体编译**

Run: `go test ./internal/config -v && go build ./...`
Expected: PASS，三个二进制编译通过

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "chore: scaffold remote workspace services"
```

### Task 2: 数据库迁移、CDK认证与单项目绑定

**Files:**
- Create: `migrations/001_initial.sql`
- Create: `internal/auth/cdk.go`
- Create: `internal/auth/cdk_test.go`
- Create: `internal/store/postgres.go`
- Create: `internal/project/service.go`
- Create: `internal/project/service_test.go`

**Interfaces:**
- Consumes: `config.Config`
- Produces:
  - `auth.HashCDK(plain string) (string, error)`（编码为`argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>`）
  - `auth.VerifyCDK(plain, encoded string) bool`
  - `auth.NewCDK() (plain string, err error)`（`ccw_`前缀+32字节随机hex）
  - `store.New(ctx, dsn string) (*Store, error)`、`(*Store).Migrate(ctx) error`
  - `project.Project{ID, AccountID, Slug, ContainerName string; DiskLimit, FiveHourLimit, SevenDayLimit int64}`
  - `project.Resolver`接口：`ResolveCDK(ctx, plain string) (Project, error)`；错误恒为`project.ErrInvalidCDK`（不区分不存在/禁用/过期）

- [ ] **Step 1: 写失败测试**

`internal/auth/cdk_test.go`：

```go
package auth

import (
	"strings"
	"testing"
)

func TestHashRoundTrip(t *testing.T) {
	plain, err := NewCDK()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "ccw_") {
		t.Fatalf("cdk must have ccw_ prefix: %q", plain)
	}
	enc, err := HashCDK(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, plain) || strings.Contains(enc, strings.TrimPrefix(plain, "ccw_")) {
		t.Fatal("encoded hash must not contain plaintext")
	}
	if !VerifyCDK(plain, enc) {
		t.Fatal("verify must succeed for correct cdk")
	}
	if VerifyCDK(plain+"x", enc) {
		t.Fatal("verify must fail for wrong cdk")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashCDK("ccw_same")
	b, _ := HashCDK("ccw_same")
	if a == b {
		t.Fatal("two hashes of the same cdk must differ (random salt)")
	}
}
```

`internal/project/service_test.go`（内存版Resolver测隔离逻辑，不需要数据库）：

```go
package project

import (
	"context"
	"testing"

	"ccw/internal/auth"
)

func TestCDKBindsExactlyOneProject(t *testing.T) {
	cdkA, _ := auth.NewCDK()
	cdkB, _ := auth.NewCDK()
	hashA, _ := auth.HashCDK(cdkA)
	hashB, _ := auth.HashCDK(cdkB)
	r := NewMemoryResolver(map[string]Project{
		hashA: {ID: "pa", Slug: "project-a"},
		hashB: {ID: "pb", Slug: "project-b"},
	})
	p, err := r.ResolveCDK(context.Background(), cdkA)
	if err != nil || p.ID != "pa" {
		t.Fatalf("cdkA must resolve to project A only, got %+v err=%v", p, err)
	}
	if _, err := r.ResolveCDK(context.Background(), "ccw_unknown"); err != ErrInvalidCDK {
		t.Fatalf("unknown cdk must return ErrInvalidCDK, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/auth ./internal/project -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

```bash
go get golang.org/x/crypto@latest github.com/jackc/pgx/v5@latest github.com/google/uuid@latest
```

`internal/auth/cdk.go`：

```go
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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

func NewCDK() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ccw_" + hex.EncodeToString(b), nil
}

func HashCDK(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(h)), nil
}

func VerifyCDK(plain, encoded string) bool {
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
```

`internal/project/service.go`：

```go
package project

import (
	"context"
	"errors"

	"ccw/internal/auth"
)

type Project struct {
	ID            string
	AccountID     string
	Slug          string
	ContainerName string
	DiskLimit     int64
	FiveHourLimit int64
	SevenDayLimit int64
}

var ErrInvalidCDK = errors.New("invalid cdk")

type Resolver interface {
	ResolveCDK(ctx context.Context, plain string) (Project, error)
}

// MemoryResolver：单元测试与后续HTTP测试共用；生产实现在store包。
type MemoryResolver struct{ byHash map[string]Project }

func NewMemoryResolver(byHash map[string]Project) *MemoryResolver {
	return &MemoryResolver{byHash: byHash}
}

func (r *MemoryResolver) ResolveCDK(_ context.Context, plain string) (Project, error) {
	for enc, p := range r.byHash {
		if auth.VerifyCDK(plain, enc) {
			return p, nil
		}
	}
	return Project{}, ErrInvalidCDK
}
```

`migrations/001_initial.sql`（照抄spec第4节引用的初版计划六张表，此处必须完整落盘）：

```sql
CREATE TABLE accounts (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  upstream_pool TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id UUID PRIMARY KEY,
  account_id UUID NOT NULL REFERENCES accounts(id),
  slug TEXT NOT NULL UNIQUE,
  container_name TEXT NOT NULL UNIQUE,
  disk_limit_bytes BIGINT NOT NULL,
  five_hour_limit BIGINT NOT NULL,
  seven_day_limit BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE cdks (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  token_hash TEXT NOT NULL UNIQUE,
  expires_at TIMESTAMPTZ,
  disabled_at TIMESTAMPTZ
);

CREATE TABLE usage_events (
  id BIGSERIAL PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  occurred_at TIMESTAMPTZ NOT NULL,
  model TEXT NOT NULL,
  input_tokens BIGINT NOT NULL,
  output_tokens BIGINT NOT NULL,
  cache_read_tokens BIGINT NOT NULL,
  cache_write_tokens BIGINT NOT NULL,
  weighted_units BIGINT NOT NULL,
  source_event_id TEXT NOT NULL UNIQUE
);
CREATE INDEX usage_events_window ON usage_events (project_id, occurred_at);

CREATE TABLE file_index (
  project_id UUID NOT NULL REFERENCES projects(id),
  path TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  sha256 TEXT NOT NULL,
  revision BIGINT NOT NULL,
  deleted BOOLEAN NOT NULL DEFAULT FALSE,
  PRIMARY KEY (project_id, path)
);

CREATE TABLE sessions (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  tmux_name TEXT NOT NULL,
  connected_at TIMESTAMPTZ,
  last_seen_at TIMESTAMPTZ NOT NULL,
  state TEXT NOT NULL
);
```

`internal/store/postgres.go`：

```go
package store

import (
	"context"
	"embed"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"ccw/internal/auth"
	"ccw/internal/project"
)

//go:embed migrations
var migrationsFS embed.FS // 构建时将migrations/软链接或复制进包目录；见Step 3末命令

type Store struct{ Pool *pgxpool.Pool }

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		sql, err := migrationsFS.ReadFile("migrations/" + n)
		if err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("store: migrate %s: %w", n, err)
		}
	}
	return nil
}

// ResolveCDK实现project.Resolver：取未禁用未过期的cdk行逐一验证哈希。
func (s *Store) ResolveCDK(ctx context.Context, plain string) (project.Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.token_hash, p.id, p.account_id, p.slug, p.container_name,
		       p.disk_limit_bytes, p.five_hour_limit, p.seven_day_limit
		FROM cdks c JOIN projects p ON p.id = c.project_id
		WHERE c.disabled_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > now())`)
	if err != nil {
		return project.Project{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var p project.Project
		if err := rows.Scan(&hash, &p.ID, &p.AccountID, &p.Slug, &p.ContainerName,
			&p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit); err != nil {
			return project.Project{}, err
		}
		if auth.VerifyCDK(plain, hash) {
			return p, nil
		}
	}
	return project.Project{}, project.ErrInvalidCDK
}
```

```bash
mkdir -p internal/store/migrations && cp migrations/001_initial.sql internal/store/migrations/
```

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/auth ./internal/project -v && go build ./...`
Expected: PASS（store包的数据库集成在Task 12 e2e覆盖；本任务只要求编译通过）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: bind hashed cdk tokens to projects"
```

### Task 3: HMAC短期连接令牌

**Files:**
- Create: `internal/token/token.go`
- Create: `internal/token/token_test.go`

**Interfaces:**
- Consumes: `config.Config.TokenSigningKey`
- Produces:
  - `token.Audience`常量：`AudSession="session"`、`AudTerminal="terminal"`、`AudSync="sync"`
  - `token.Mint(key []byte, projectID, audience string, ttl time.Duration, now time.Time) (string, error)`
  - `token.Verify(key []byte, tok, audience string, now time.Time) (Claims, error)`；`Claims{ProjectID, Audience string; ExpiresAt time.Time}`
  - 错误：`token.ErrInvalid`（签名/格式错）、`token.ErrExpired`、`token.ErrAudience`

- [ ] **Step 1: 写失败测试**

`internal/token/token_test.go`：

```go
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
	bad := tok[:len(tok)-2] + "zz"
	if _, err := Verify(key, bad, AudSync, now); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/token -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/token/token.go`：

```go
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
```

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/token -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: add hmac-signed short-lived connection tokens"
```

### Task 4: 项目容器运行时与持久卷（假Docker API）

**Files:**
- Create: `internal/runtime/docker.go`
- Create: `internal/runtime/docker_test.go`
- Create: `configs/worker-policy.json`
- Create: `deploy/compose.yaml`
- Create: `docs/admin-login-runbook.md`

**Interfaces:**
- Consumes: `project.Project`
- Produces:
  - `runtime.DockerAPI`接口：`EnsureVolume(ctx, name string) error`；`EnsureContainer(ctx, spec ContainerSpec) error`；`RemoveContainer(ctx, name string) error`
  - `runtime.ContainerSpec{Name, Image string; Mounts []Mount; User string; Cmd []string; Limits Limits}`；`Mount{Volume, Target string}`；`Limits{NanoCPUs int64; MemoryBytes int64; PidsLimit int64}`
  - `runtime.VolumeNames(p project.Project) (workspace, claudeHome, sync string)`（`<slug>-workspace`等）
  - `runtime.EnsureProjectRuntime(ctx, api DockerAPI, p project.Project, image string) error`
  - 容器命令固定：`tmux -L <project-id> new-session -A -s main -c /workspace claude`

- [ ] **Step 1: 写失败测试**

`internal/runtime/docker_test.go`：

```go
package runtime

import (
	"context"
	"strings"
	"testing"

	"ccw/internal/project"
)

type fakeDocker struct {
	volumes    []string
	containers []ContainerSpec
}

func (f *fakeDocker) EnsureVolume(_ context.Context, name string) error {
	f.volumes = append(f.volumes, name)
	return nil
}
func (f *fakeDocker) EnsureContainer(_ context.Context, s ContainerSpec) error {
	f.containers = append(f.containers, s)
	return nil
}
func (f *fakeDocker) RemoveContainer(_ context.Context, string) error { return nil }

var pa = project.Project{ID: "11111111-1111-1111-1111-111111111111", Slug: "project-a", ContainerName: "ccw-project-a"}
var pb = project.Project{ID: "22222222-2222-2222-2222-222222222222", Slug: "project-b", ContainerName: "ccw-project-b"}

func TestMountsNeverCrossProjects(t *testing.T) {
	f := &fakeDocker{}
	if err := EnsureProjectRuntime(context.Background(), f, pa, "img"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectRuntime(context.Background(), f, pb, "img"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.containers {
		for _, m := range c.Mounts {
			if c.Name == pa.ContainerName && strings.Contains(m.Volume, "project-b") {
				t.Fatalf("container A mounts B volume: %+v", m)
			}
			if c.Name == pb.ContainerName && strings.Contains(m.Volume, "project-a") {
				t.Fatalf("container B mounts A volume: %+v", m)
			}
		}
		if len(c.Mounts) != 3 {
			t.Fatalf("must mount exactly 3 volumes, got %d", len(c.Mounts))
		}
	}
}

func TestVolumeNamesStableAcrossRebuild(t *testing.T) {
	w1, c1, s1 := VolumeNames(pa)
	w2, c2, s2 := VolumeNames(pa)
	if w1 != w2 || c1 != c2 || s1 != s2 {
		t.Fatal("volume names must be deterministic")
	}
	if w1 != "project-a-workspace" || c1 != "project-a-claude" || s1 != "project-a-sync" {
		t.Fatalf("unexpected names: %s %s %s", w1, c1, s1)
	}
}

func TestSecurityDefaults(t *testing.T) {
	f := &fakeDocker{}
	_ = EnsureProjectRuntime(context.Background(), f, pa, "img")
	c := f.containers[0]
	if c.User == "" || c.User == "root" || c.User == "0" {
		t.Fatalf("container must run as non-root, got %q", c.User)
	}
	if c.Limits.MemoryBytes == 0 || c.Limits.PidsLimit == 0 || c.Limits.NanoCPUs == 0 {
		t.Fatalf("resource limits must be set: %+v", c.Limits)
	}
	want := "tmux -L " + pa.ID + " new-session -A -s main -c /workspace claude"
	if got := strings.Join(c.Cmd, " "); got != want {
		t.Fatalf("cmd mismatch:\n got %q\nwant %q", got, want)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/runtime -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/runtime/docker.go`：

```go
package runtime

import (
	"context"

	"ccw/internal/project"
)

type Mount struct{ Volume, Target string }

type Limits struct {
	NanoCPUs    int64
	MemoryBytes int64
	PidsLimit   int64
}

type ContainerSpec struct {
	Name   string
	Image  string
	Mounts []Mount
	User   string
	Cmd    []string
	Limits Limits
}

type DockerAPI interface {
	EnsureVolume(ctx context.Context, name string) error
	EnsureContainer(ctx context.Context, spec ContainerSpec) error
	RemoveContainer(ctx context.Context, name string) error
}

func VolumeNames(p project.Project) (workspace, claudeHome, sync string) {
	return p.Slug + "-workspace", p.Slug + "-claude", p.Slug + "-sync"
}

func EnsureProjectRuntime(ctx context.Context, api DockerAPI, p project.Project, image string) error {
	w, c, s := VolumeNames(p)
	for _, v := range []string{w, c, s} {
		if err := api.EnsureVolume(ctx, v); err != nil {
			return err
		}
	}
	return api.EnsureContainer(ctx, ContainerSpec{
		Name:  p.ContainerName,
		Image: image,
		User:  "claude",
		Mounts: []Mount{
			{Volume: w, Target: "/workspace"},
			{Volume: c, Target: "/home/claude/.claude"},
			{Volume: s, Target: "/var/lib/cclaude-sync"},
		},
		Cmd: []string{"tmux", "-L", p.ID, "new-session", "-A", "-s", "main", "-c", "/workspace", "claude"},
		Limits: Limits{
			NanoCPUs:    2_000_000_000,  // 2 CPU
			MemoryBytes: 4 << 30,        // 4 GiB
			PidsLimit:   512,
		},
	})
}
```

`configs/worker-policy.json`：

```json
{
  "no_new_privileges": true,
  "cap_drop": ["ALL"],
  "mount_docker_socket": false,
  "network": "ccw-projects",
  "tmpfs": {"/tmp": "size=512m"}
}
```

`deploy/compose.yaml`（PostgreSQL与两个服务的开发编排）：

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: ccw
      POSTGRES_PASSWORD: ccw-dev-only
      POSTGRES_DB: ccw
    volumes: ["ccw-pg:/var/lib/postgresql/data"]
    ports: ["127.0.0.1:5432:5432"]
volumes:
  ccw-pg: {}
```

`docs/admin-login-runbook.md`要点（完整写入文件）：管理员用`docker exec -it ccw-project-a tmux -L <project-id> attach`分别进入A/B完成一次官方`claude`登录；验证凭据只出现在各自claude卷（`docker run --rm -v project-a-claude:/m alpine ls /m`）；随后执行spec R2验证：双容器同时保持登录运行24小时，各自执行一次对话，确认无一方凭据失效；若失效，改用分时使用流程并记录。CDK侧永远没有该入口。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/runtime -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: isolate project runtimes with persistent volumes"
```

### Task 5: tmux会话与WebSocket终端

**Files:**
- Create: `internal/terminal/session.go`
- Create: `internal/terminal/session_test.go`
- Create: `internal/terminal/ws.go`
- Modify: `cmd/worker-agent/main.go`

**Interfaces:**
- Consumes: `token.Verify`（AudTerminal）、`runtime`容器名约定
- Produces:
  - `terminal.Names(projectID string) (socket, session string)`（socket=projectID，session恒为`main`）
  - `terminal.AttachCmd(containerName, projectID string) []string`——`["docker","exec","-it",container,"tmux","-L",projectID,"attach-session","-t","main"]`
  - `terminal.Serve(w http.ResponseWriter, r *http.Request, key []byte, start func(projectID string) (io.ReadWriteCloser, error))`：WebSocket升级+字节转发+resize控制消息`{"type":"resize","rows":N,"cols":N}`
  - 断开语义：只关闭PTY与WebSocket，绝不kill tmux会话

- [ ] **Step 1: 写失败测试**

`internal/terminal/session_test.go`：

```go
package terminal

import (
	"os/exec"
	"strings"
	"testing"
)

func TestNamesDeterministic(t *testing.T) {
	s1, n1 := Names("pid-1")
	s2, n2 := Names("pid-1")
	if s1 != s2 || n1 != n2 || n1 != "main" || s1 != "pid-1" {
		t.Fatalf("names must be stable: %s/%s vs %s/%s", s1, n1, s2, n2)
	}
}

func TestAttachCmdNeverKills(t *testing.T) {
	cmd := AttachCmd("ccw-project-a", "pid-1")
	joined := strings.Join(cmd, " ")
	if strings.Contains(joined, "kill") {
		t.Fatalf("attach must never contain kill: %q", joined)
	}
	if !strings.Contains(joined, "attach-session") {
		t.Fatalf("must attach existing session: %q", joined)
	}
}

// 真实tmux集成：断开后会话仍在，重连能看到断开前写入的标记。
// 开发机已有tmux 3.4；无tmux的环境跳过。
func TestTmuxSurvivesDetach(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := "ccw-test"
	defer exec.Command("tmux", "-L", sock, "kill-server").Run()
	if err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "main", "sh").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("tmux", "-L", sock, "send-keys", "-t", "main", "echo MARKER-42", "Enter").Run(); err != nil {
		t.Fatal(err)
	}
	// 模拟断开：不做任何attach即为detached状态；重连=capture-pane
	out, err := exec.Command("tmux", "-L", sock, "capture-pane", "-t", "main", "-p").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "MARKER-42") {
		t.Fatalf("marker lost after detach: %q", out)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/terminal -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

```bash
go get github.com/gorilla/websocket@latest github.com/creack/pty@latest
```

`internal/terminal/session.go`：

```go
package terminal

func Names(projectID string) (socket, session string) {
	return projectID, "main"
}

func AttachCmd(containerName, projectID string) []string {
	return []string{"docker", "exec", "-i", containerName,
		"tmux", "-L", projectID, "attach-session", "-t", "main"}
}
```

`internal/terminal/ws.go`：

```go
package terminal

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"ccw/internal/token"
)

var upgrader = websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}

type Resizer interface{ Resize(rows, cols uint16) error }

type ctrlMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Serve验证terminal令牌后把WebSocket与PTY互相转发。
// start由worker-agent注入：为该项目启动/附着PTY（docker exec tmux attach）。
func Serve(w http.ResponseWriter, r *http.Request, key []byte,
	start func(projectID string) (io.ReadWriteCloser, error)) {
	claims, err := token.Verify(key, r.URL.Query().Get("token"), token.AudTerminal, time.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	pty, err := start(claims.ProjectID)
	if err != nil {
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "pty start failed"))
		return
	}
	defer pty.Close() // 只关PTY附着进程；tmux会话继续存活

	go func() { // PTY→客户端
		buf := make([]byte, 32<<10)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				conn.Close()
				return
			}
		}
	}()
	for { // 客户端→PTY；Text帧是控制消息，Binary帧是终端字节
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.TextMessage {
			var m ctrlMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				if rz, ok := pty.(Resizer); ok {
					rz.Resize(m.Rows, m.Cols)
				}
			}
			continue
		}
		if _, err := pty.Write(data); err != nil {
			return
		}
	}
}
```

`cmd/worker-agent/main.go`改为：加载配置→连接store→注册`GET /v1/terminal`路由（用`os/exec`+`creack/pty`启动`AttachCmd`并包装成`io.ReadWriteCloser`，实现`Resizer`调用`pty.Setsize`）→`http.ListenAndServe(cfg.AgentListenAddr, mux)`。完整代码：

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	"github.com/creack/pty"

	"ccw/internal/config"
	"ccw/internal/terminal"
)

type ptySession struct {
	f   *os.File
	cmd *exec.Cmd
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *ptySession) Close() error {
	p.f.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill() // 杀的是docker exec附着进程，不是tmux
	}
	return p.cmd.Wait()
}
func (p *ptySession) Resize(rows, cols uint16) error {
	return pty.Setsize(p.f, &pty.Winsize{Rows: rows, Cols: cols})
}

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	containerFor := func(projectID string) string {
		// 由control-api在令牌claims中放projectID；容器名从环境映射CCW_CONTAINER_<id前8位>读取，
		// e2e前的简化映射，Task 12接入store查询替换。
		if v := os.Getenv("CCW_CONTAINER_" + projectID[:8]); v != "" {
			return v
		}
		return "ccw-" + projectID
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/terminal", func(w http.ResponseWriter, r *http.Request) {
		terminal.Serve(w, r, cfg.TokenSigningKey, func(projectID string) (io.ReadWriteCloser, error) {
			args := terminal.AttachCmd(containerFor(projectID), projectID)
			cmd := exec.Command(args[0], args[1:]...)
			f, err := pty.Start(cmd)
			if err != nil {
				return nil, err
			}
			return &ptySession{f: f, cmd: cmd}, nil
		})
	})
	fmt.Println("worker-agent listening", cfg.AgentListenAddr)
	http.ListenAndServe(cfg.AgentListenAddr, mux)
}
```

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/terminal -v && go build ./...`
Expected: PASS（含真实tmux detach存活测试）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: persist terminal sessions across reconnects"
```

### Task 6: 清单同步与冲突保护

**Files:**
- Create: `internal/sync/manifest.go`
- Create: `internal/sync/manifest_test.go`
- Create: `internal/sync/paths.go`
- Create: `internal/sync/paths_test.go`
- Create: `internal/sync/server.go`
- Create: `internal/sync/server_test.go`

**Interfaces:**
- Consumes: `token.Verify`（AudSync）
- Produces:
  - `sync.FileEntry{Path string; Size int64; SHA256 string; Revision int64; Deleted bool}`
  - `sync.Diff(local, remote []FileEntry) Plan`；`Plan{Upload, Download []FileEntry; Conflicts []Conflict; TombstoneToRemote, TombstoneToLocal []FileEntry}`；`Conflict{Path, LocalSHA, RemoteSHA string}`
  - `sync.SafeRelPath(p string) (string, error)`——校验并规范为forward-slash相对路径；错误`ErrUnsafePath`
  - `sync.DefaultExcluded(path string) bool`——`.env`、`.cclaude/`、`.ssh/`、`.aws/`、`.claude/`前缀
  - `sync.ConflictName(path, device string, at time.Time) string`——`<path>.conflict-<device>-<20060102T150405Z>`
  - `sync.Store`接口（服务端落盘）：`WriteTemp(path string, r io.Reader) (sha string, size int64, err error)`；`Promote(path string, revision int64) error`；`Delete(path string, revision int64) error`；`Manifest() ([]FileEntry, error)`
  - `sync.NewDirStore(root string) Store`——`.cclaude.tmp.<revision>`+SHA校验+原子rename；拒绝符号链接逃逸

- [ ] **Step 1: 写失败测试**

`internal/sync/paths_test.go`：

```go
package sync

import "testing"

func TestSafeRelPath(t *testing.T) {
	bad := []string{"/etc/passwd", "../x", "a/../../x", "a\\..\\x", ""}
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
```

`internal/sync/manifest_test.go`：

```go
package sync

import (
	"testing"
	"time"
)

func e(path, sha string, rev int64, del bool) FileEntry {
	return FileEntry{Path: path, SHA256: sha, Revision: rev, Deleted: del, Size: 10}
}

func TestDiffUploadDownload(t *testing.T) {
	local := []FileEntry{e("a.go", "s1", 1, false), e("new.go", "s2", 0, false)}
	remote := []FileEntry{e("a.go", "s1", 1, false), e("srv.go", "s3", 4, false)}
	p := Diff(local, remote)
	if len(p.Upload) != 1 || p.Upload[0].Path != "new.go" {
		t.Fatalf("want upload new.go, got %+v", p.Upload)
	}
	if len(p.Download) != 1 || p.Download[0].Path != "srv.go" {
		t.Fatalf("want download srv.go, got %+v", p.Download)
	}
	if len(p.Conflicts) != 0 {
		t.Fatalf("no conflicts expected: %+v", p.Conflicts)
	}
}

func TestDiffBothModifiedIsConflict(t *testing.T) {
	// 同一revision基线上双方都改了内容
	local := []FileEntry{e("a.go", "local-sha", 2, false)}
	remote := []FileEntry{e("a.go", "remote-sha", 2, false)}
	p := Diff(local, remote)
	if len(p.Conflicts) != 1 || p.Conflicts[0].Path != "a.go" {
		t.Fatalf("want conflict on a.go, got %+v", p)
	}
	if len(p.Upload)+len(p.Download) != 0 {
		t.Fatal("conflict must not silently transfer")
	}
}

func TestDiffTombstone(t *testing.T) {
	local := []FileEntry{e("gone.go", "s", 3, true)}
	remote := []FileEntry{e("gone.go", "s", 2, false)}
	p := Diff(local, remote)
	if len(p.TombstoneToRemote) != 1 {
		t.Fatalf("deletion must propagate: %+v", p)
	}
}

func TestConflictName(t *testing.T) {
	at := time.Date(2026, 7, 19, 8, 30, 0, 0, time.UTC)
	got := ConflictName("src/a.go", "laptop", at)
	want := "src/a.go.conflict-laptop-20260719T083000Z"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

`internal/sync/server_test.go`（DirStore行为）：

```go
package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirStoreAtomicWriteAndManifest(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	sha, size, err := s.WriteTemp("src/main.go", strings.NewReader("package main\n"))
	if err != nil || size != 13 {
		t.Fatalf("write: sha=%s size=%d err=%v", sha, size, err)
	}
	// promote前目录里只有tmp文件，不可见于清单
	m, _ := s.Manifest()
	if len(m) != 0 {
		t.Fatalf("tmp file must not appear in manifest: %+v", m)
	}
	if err := s.Promote("src/main.go", 1); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Manifest()
	if len(m) != 1 || m[0].Path != "src/main.go" || m[0].SHA256 != sha {
		t.Fatalf("manifest wrong: %+v", m)
	}
}

func TestDirStoreRejectsEscape(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	if _, _, err := s.WriteTemp("../evil", strings.NewReader("x")); err == nil {
		t.Fatal("path escape must be rejected")
	}
	// 符号链接逃逸：workspace内建一个指向外部的链接目录
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "link"))
	if _, _, err := s.WriteTemp("link/evil.txt", strings.NewReader("x")); err == nil {
		t.Fatal("symlink escape must be rejected")
	}
}

func TestDirStoreDeleteTombstone(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	s.WriteTemp("a.txt", strings.NewReader("hello"))
	s.Promote("a.txt", 1)
	if err := s.Delete("a.txt", 2); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Manifest()
	if len(m) != 0 {
		t.Fatalf("deleted file must leave manifest: %+v", m)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/sync -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/sync/paths.go`：

```go
package sync

import (
	"errors"
	"path"
	"strings"
)

var ErrUnsafePath = errors.New("sync: unsafe path")

func SafeRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return "", ErrUnsafePath
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrUnsafePath
	}
	return clean, nil
}

var excludedPrefixes = []string{".env", ".cclaude/", ".ssh/", ".aws/", ".claude/",
	".config/gcloud/", ".azure/", ".kube/"}

func DefaultExcluded(p string) bool {
	for _, pre := range excludedPrefixes {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}
```

`internal/sync/manifest.go`：

```go
package sync

import (
	"fmt"
	"time"
)

type FileEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Revision int64  `json:"revision"`
	Deleted  bool   `json:"deleted"`
}

type Conflict struct{ Path, LocalSHA, RemoteSHA string }

type Plan struct {
	Upload, Download                     []FileEntry
	Conflicts                            []Conflict
	TombstoneToRemote, TombstoneToLocal  []FileEntry
}

// Diff规则：
//   只在一端存在（且未删除）→ 传到另一端；
//   两端同revision但SHA不同 → 冲突（双方都在同一基线上改动）；
//   一端revision更高 → 高者胜出（单侧变化）；
//   一端deleted且revision更高 → tombstone传播。
func Diff(local, remote []FileEntry) Plan {
	li := index(local)
	ri := index(remote)
	var p Plan
	for path, l := range li {
		r, ok := ri[path]
		switch {
		case !ok:
			if !l.Deleted {
				p.Upload = append(p.Upload, l)
			}
		case l.Deleted && l.Revision > r.Revision:
			p.TombstoneToRemote = append(p.TombstoneToRemote, l)
		case r.Deleted && r.Revision > l.Revision:
			p.TombstoneToLocal = append(p.TombstoneToLocal, r)
		case l.SHA256 == r.SHA256:
			// 一致，无事
		case l.Revision == r.Revision:
			p.Conflicts = append(p.Conflicts, Conflict{Path: path, LocalSHA: l.SHA256, RemoteSHA: r.SHA256})
		case l.Revision > r.Revision:
			p.Upload = append(p.Upload, l)
		default:
			p.Download = append(p.Download, r)
		}
	}
	for path, r := range ri {
		if _, ok := li[path]; !ok && !r.Deleted {
			p.Download = append(p.Download, r)
		}
	}
	return p
}

func index(es []FileEntry) map[string]FileEntry {
	m := make(map[string]FileEntry, len(es))
	for _, e := range es {
		m[e.Path] = e
	}
	return m
}

func ConflictName(path, device string, at time.Time) string {
	return fmt.Sprintf("%s.conflict-%s-%s", path, device, at.UTC().Format("20060102T150405Z"))
}
```

`internal/sync/server.go`（DirStore；WebSocket端点在Task 7接好配额后一并挂到worker-agent）：

```go
package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Store interface {
	WriteTemp(path string, r io.Reader) (sha string, size int64, err error)
	Promote(path string, revision int64) error
	Delete(path string, revision int64) error
	Manifest() ([]FileEntry, error)
}

type DirStore struct{ root string }

func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// resolve把相对路径映射到root下，并用EvalSymlinks防符号链接逃逸。
func (d *DirStore) resolve(rel string) (string, error) {
	rel, err := SafeRelPath(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(d.root, filepath.FromSlash(rel))
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(d.root)
	if err != nil {
		return "", err
	}
	if realParent != realRoot && !strings.HasPrefix(realParent+string(os.PathSeparator), realRoot+string(os.PathSeparator)) {
		return "", ErrUnsafePath
	}
	return filepath.Join(realParent, filepath.Base(abs)), nil
}

func (d *DirStore) tmpName(abs string, rev int64) string {
	return filepath.Join(filepath.Dir(abs), fmt.Sprintf(".cclaude.tmp.%d.%s", rev, filepath.Base(abs)))
}

func (d *DirStore) WriteTemp(rel string, r io.Reader) (string, int64, error) {
	abs, err := d.resolve(rel)
	if err != nil {
		return "", 0, err
	}
	f, err := os.Create(d.tmpName(abs, 0))
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (d *DirStore) Promote(rel string, rev int64) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	return os.Rename(d.tmpName(abs, 0), abs)
}

func (d *DirStore) Delete(rel string, rev int64) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *DirStore) Manifest() ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, d.root+string(os.PathSeparator)))
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".cclaude.tmp.") || DefaultExcluded(rel) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return out, err
}
```

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/sync -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: add safe bidirectional project sync core"
```

### Task 7: 磁盘配额与文件索引记账

**Files:**
- Create: `internal/storage/accounting.go`
- Create: `internal/storage/accounting_test.go`

**Interfaces:**
- Consumes: `sync.FileEntry`
- Produces:
  - `storage.Index`接口：`Upsert(ctx, projectID string, e sync.FileEntry) error`；`DiskUsed(ctx, projectID string) (int64, error)`
  - `storage.MemoryIndex`（测试/本地）与`storage.PGIndex`（生产，写`file_index`表，同事务`SUM`）
  - `storage.Gate{Limit int64}`：`Allow(used, oldSize, newSize int64) error`——新增/扩大超限返回`ErrDiskFull`，删除/缩小永远允许
  - `storage.ErrDiskFull`

- [ ] **Step 1: 写失败测试**

`internal/storage/accounting_test.go`：

```go
package storage

import (
	"context"
	"testing"

	"ccw/internal/sync"
)

func TestLogicalBytesExact(t *testing.T) {
	ctx := context.Background()
	idx := NewMemoryIndex()
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "a.bin", Size: 221})
	used, _ := idx.DiskUsed(ctx, "pa")
	if used != 221 {
		t.Fatalf("want 221, got %d", used)
	}
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "b.bin", Size: 31})
	used, _ = idx.DiskUsed(ctx, "pa")
	if used != 252 {
		t.Fatalf("want 252, got %d", used)
	}
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "a.bin", Size: 221, Deleted: true})
	used, _ = idx.DiskUsed(ctx, "pa")
	if used != 31 {
		t.Fatalf("deleted must not count, got %d", used)
	}
}

func TestGate(t *testing.T) {
	g := Gate{Limit: 100}
	if err := g.Allow(90, 0, 20); err != ErrDiskFull {
		t.Fatalf("grow over limit must fail, got %v", err)
	}
	if err := g.Allow(90, 50, 20); err != nil {
		t.Fatalf("shrink must pass, got %v", err)
	}
	if err := g.Allow(100, 30, 0); err != nil {
		t.Fatalf("delete must pass even at limit, got %v", err)
	}
}

func TestProjectsIsolated(t *testing.T) {
	ctx := context.Background()
	idx := NewMemoryIndex()
	idx.Upsert(ctx, "pa", sync.FileEntry{Path: "a", Size: 10})
	used, _ := idx.DiskUsed(ctx, "pb")
	if used != 0 {
		t.Fatalf("pb must be 0, got %d", used)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/storage -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/storage/accounting.go`（注意：本项目包名`sync`与标准库同名，必须用别名`stdsync`与`syncpkg`区分）：

```go
package storage

import (
	"context"
	"errors"
	stdsync "sync"

	syncpkg "ccw/internal/sync"
)

var ErrDiskFull = errors.New("storage: disk quota exceeded")

type Index interface {
	Upsert(ctx context.Context, projectID string, e syncpkg.FileEntry) error
	DiskUsed(ctx context.Context, projectID string) (int64, error)
}

type MemoryIndex struct {
	mu stdsync.Mutex
	m  map[string]map[string]syncpkg.FileEntry
}

func NewMemoryIndex() *MemoryIndex {
	return &MemoryIndex{m: map[string]map[string]syncpkg.FileEntry{}}
}

func (i *MemoryIndex) Upsert(_ context.Context, pid string, e syncpkg.FileEntry) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.m[pid] == nil {
		i.m[pid] = map[string]syncpkg.FileEntry{}
	}
	i.m[pid][e.Path] = e
	return nil
}

func (i *MemoryIndex) DiskUsed(_ context.Context, pid string) (int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var sum int64
	for _, e := range i.m[pid] {
		if !e.Deleted {
			sum += e.Size
		}
	}
	return sum, nil
}

type Gate struct{ Limit int64 }

// Allow：projected = used - oldSize + newSize；只有增量为正且超限才拒绝。
func (g Gate) Allow(used, oldSize, newSize int64) error {
	if newSize <= oldSize {
		return nil
	}
	if used-oldSize+newSize > g.Limit {
		return ErrDiskFull
	}
	return nil
}
```

`PGIndex`生产实现（同文件追加）：`Upsert`在一个事务里`INSERT ... ON CONFLICT (project_id,path) DO UPDATE`并返回；`DiskUsed`执行`SELECT COALESCE(SUM(size_bytes),0) FROM file_index WHERE project_id=$1 AND NOT deleted`。签名与`Index`一致，接收`*pgxpool.Pool`。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/storage -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: enforce logical project storage quotas"
```

### Task 8: JSONL用量采集器

**Files:**
- Create: `internal/usage/collector.go`
- Create: `internal/usage/collector_test.go`
- Create: `internal/usage/testdata/session-sample.jsonl`

**Interfaces:**
- Consumes: 各项目claude卷内`projects/<dir>/<session>.jsonl`（宿主机路径`/var/lib/docker/volumes/<slug>-claude/_data/...`）
- Produces:
  - `usage.Event{SourceEventID string; OccurredAt time.Time; Model string; Input, Output, CacheRead, CacheWrite int64}`
  - `usage.ParseLines(r io.Reader) (events []Event, badLines int)`——容错：坏行/非assistant行/缺usage跳过并计数
  - `usage.Weights{Input, Output, CacheRead, CacheWrite int64}`与`usage.Weighted(e Event, w Weights) int64`
  - `usage.Sink`接口：`Insert(ctx, projectID string, e Event, weighted int64) error`（幂等：`source_event_id`冲突时忽略）
  - `usage.Collector{Dir string; ProjectID string; Sink Sink; Weights Weights}`：`Scan(ctx) error`记录每文件字节偏移量，只读增量

- [ ] **Step 1: 准备真实样例并写失败测试**

`internal/usage/testdata/session-sample.jsonl`（字段结构取自2026-07-19本机实测的真实Claude Code会话记录；含1条正常事件、1条重复ID、1条坏行、1条无usage的user行）：

```jsonl
{"type":"assistant","uuid":"u-1","requestId":"req_A","timestamp":"2026-07-19T08:00:00.000Z","message":{"model":"claude-fable-5","usage":{"input_tokens":1248,"cache_creation_input_tokens":3830,"cache_read_input_tokens":18871,"output_tokens":610}}}
{"type":"user","uuid":"u-2","timestamp":"2026-07-19T08:00:01.000Z","message":{"role":"user"}}
{"type":"assistant","uuid":"u-3","requestId":"req_A","timestamp":"2026-07-19T08:00:02.000Z","message":{"model":"claude-fable-5","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
not-json-at-all
{"type":"assistant","uuid":"u-4","requestId":"req_B","timestamp":"2026-07-19T08:05:00.000Z","message":{"model":"claude-fable-5","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":20}}}
```

`internal/usage/collector_test.go`：

```go
package usage

import (
	"context"
	"os"
	"testing"
)

func TestParseLines(t *testing.T) {
	f, err := os.Open("testdata/session-sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	events, bad := ParseLines(f)
	if len(events) != 3 { // 两条req_A都解析出来，去重是Sink的职责
		t.Fatalf("want 3 events, got %d", len(events))
	}
	if bad != 1 {
		t.Fatalf("want 1 bad line, got %d", bad)
	}
	e := events[0]
	if e.SourceEventID != "req_A" || e.Input != 1248 || e.Output != 610 ||
		e.CacheRead != 18871 || e.CacheWrite != 3830 || e.Model != "claude-fable-5" {
		t.Fatalf("first event wrong: %+v", e)
	}
}

func TestWeighted(t *testing.T) {
	e := Event{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	w := Weights{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12}
	if got := Weighted(e, w); got != 10*10+5*50+100*1+20*12 {
		t.Fatalf("weighted wrong: %d", got)
	}
}

type memSink struct{ ids map[string]bool }

func (m *memSink) Insert(_ context.Context, _ string, e Event, _ int64) error {
	if m.ids == nil {
		m.ids = map[string]bool{}
	}
	m.ids[e.SourceEventID] = true
	return nil
}

func TestCollectorIncrementalScan(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/projects/x", 0o755)
	line := `{"type":"assistant","requestId":"req_C","timestamp":"2026-07-19T09:00:00.000Z","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	path := dir + "/projects/x/s.jsonl"
	os.WriteFile(path, []byte(line), 0o644)
	sink := &memSink{}
	c := &Collector{Dir: dir, ProjectID: "pa", Sink: sink}
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sink.ids["req_C"] {
		t.Fatal("req_C not collected")
	}
	// 追加一行后再扫，只处理增量（通过偏移量），且新事件被采集
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"assistant","requestId":"req_D","timestamp":"2026-07-19T09:01:00.000Z","message":{"model":"m","usage":{"input_tokens":2,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n")
	f.Close()
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sink.ids["req_D"] {
		t.Fatal("incremental event not collected")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/usage -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/usage/collector.go`：

```go
package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Event struct {
	SourceEventID string
	OccurredAt    time.Time
	Model         string
	Input, Output, CacheRead, CacheWrite int64
}

type rawLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func ParseLines(r io.Reader) ([]Event, int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20) // 长行容忍到8MB
	var out []Event
	bad := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rl rawLine
		if err := json.Unmarshal([]byte(line), &rl); err != nil {
			bad++
			continue
		}
		if rl.Type != "assistant" || rl.Message.Usage == nil || rl.RequestID == "" {
			continue // 非用量行，不算坏行
		}
		ts, err := time.Parse(time.RFC3339Nano, rl.Timestamp)
		if err != nil {
			bad++
			continue
		}
		u := rl.Message.Usage
		out = append(out, Event{
			SourceEventID: rl.RequestID, OccurredAt: ts.UTC(), Model: rl.Message.Model,
			Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		})
	}
	return out, bad
}

type Weights struct{ Input, Output, CacheRead, CacheWrite int64 }

func Weighted(e Event, w Weights) int64 {
	return e.Input*w.Input + e.Output*w.Output + e.CacheRead*w.CacheRead + e.CacheWrite*w.CacheWrite
}

type Sink interface {
	Insert(ctx context.Context, projectID string, e Event, weighted int64) error
}

type Collector struct {
	Dir       string
	ProjectID string
	Sink      Sink
	Weights   Weights
	offsets   map[string]int64
}

func (c *Collector) Scan(ctx context.Context) error {
	if c.offsets == nil {
		c.offsets = map[string]int64{}
	}
	return filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil // 单文件读失败不中断整体采集
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		off := c.offsets[p]
		if fi, _ := f.Stat(); fi != nil && fi.Size() < off {
			off = 0 // 文件被截断则重扫（幂等由Sink保证）
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil
		}
		events, _ := ParseLines(f)
		for _, e := range events {
			if err := c.Sink.Insert(ctx, c.ProjectID, e, Weighted(e, c.Weights)); err != nil {
				return err // Sink失败要停：偏移量不前进，下轮重试
			}
		}
		if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
			c.offsets[p] = pos
		}
		return nil
	})
}
```

生产Sink（`store`包追加方法）：`INSERT INTO usage_events (...) VALUES (...) ON CONFLICT (source_event_id) DO NOTHING`。默认权重（可由环境变量覆盖，比例参考官方计费）：`Input=10, Output=50, CacheRead=1, CacheWrite=12`。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/usage -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: collect usage events from claude jsonl transcripts"
```

### Task 9: 额度窗口与双层闸门

**Files:**
- Create: `internal/quota/service.go`
- Create: `internal/quota/service_test.go`

**Interfaces:**
- Consumes: `usage_events`表（生产）/内存事件表（测试）
- Produces:
  - `quota.UsageReader`接口：`WindowUsed(ctx, projectID string, since time.Time) (int64, error)`；`PoolUsed(ctx, accountID string, since time.Time) (int64, error)`
  - `quota.Limits{FiveHour, SevenDay, PoolFiveHour, Reserve, SafetyMargin int64}`
  - `quota.Service{Reader UsageReader}`：`Check(ctx, projectID, accountID string, l Limits, now time.Time) (Decision, error)`
  - `quota.Decision{Over bool; Reason string; FiveHourUsed, SevenDayUsed int64}`；Reason取值：`""`、`"five_hour_limit"`、`"seven_day_limit"`、`"pool_exhausted"`（磁盘原因由调用方叠加`"disk_limit"`）
  - `AccountUsageProvider`接口按spec原样声明，第一版无实现

- [ ] **Step 1: 写失败测试**

`internal/quota/service_test.go`：

```go
package quota

import (
	"context"
	"testing"
	"time"
)

type memReader struct {
	events map[string][]struct {
		at time.Time
		n  int64
	}
	pool map[string][]struct {
		at time.Time
		n  int64
	}
}

func (m *memReader) WindowUsed(_ context.Context, pid string, since time.Time) (int64, error) {
	var s int64
	for _, e := range m.events[pid] {
		if e.at.After(since) {
			s += e.n
		}
	}
	return s, nil
}

func (m *memReader) PoolUsed(_ context.Context, aid string, since time.Time) (int64, error) {
	var s int64
	for _, e := range m.pool[aid] {
		if e.at.After(since) {
			s += e.n
		}
	}
	return s, nil
}

func at(now time.Time, ago time.Duration, n int64) struct {
	at time.Time
	n  int64
} {
	return struct {
		at time.Time
		n  int64
	}{now.Add(-ago), n}
}

func TestWindowBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r := &memReader{events: map[string][]struct {
		at time.Time
		n  int64
	}{
		"pa": {at(now, 4*time.Hour, 100), at(now, 6*time.Hour, 500), at(now, 8*24*time.Hour, 9000)},
	}}
	s := Service{Reader: r}
	d, err := s.Check(context.Background(), "pa", "acc", Limits{FiveHour: 1000, SevenDay: 1000, PoolFiveHour: 1 << 40}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.FiveHourUsed != 100 { // 6小时前的不算
		t.Fatalf("5h window wrong: %d", d.FiveHourUsed)
	}
	if d.SevenDayUsed != 600 { // 8天前的不算
		t.Fatalf("7d window wrong: %d", d.SevenDayUsed)
	}
	if d.Over {
		t.Fatalf("must not be over: %+v", d)
	}
}

func TestProjectIsolationAOverBFree(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r := &memReader{events: map[string][]struct {
		at time.Time
		n  int64
	}{
		"pa": {at(now, time.Hour, 2000)},
		"pb": {at(now, time.Hour, 10)},
	}}
	s := Service{Reader: r}
	l := Limits{FiveHour: 1000, SevenDay: 100000, PoolFiveHour: 1 << 40}
	da, _ := s.Check(context.Background(), "pa", "acc", l, now)
	db, _ := s.Check(context.Background(), "pb", "acc", l, now)
	if !da.Over || da.Reason != "five_hour_limit" {
		t.Fatalf("A must be over: %+v", da)
	}
	if db.Over {
		t.Fatalf("B must still be allowed: %+v", db)
	}
}

func TestPoolSafetyMarginStopsBoth(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r := &memReader{
		events: map[string][]struct {
			at time.Time
			n  int64
		}{"pa": {at(now, time.Hour, 10)}, "pb": {at(now, time.Hour, 10)}},
		pool: map[string][]struct {
			at time.Time
			n  int64
		}{"acc": {at(now, time.Hour, 9800)}},
	}
	s := Service{Reader: r}
	l := Limits{FiveHour: 100000, SevenDay: 100000, PoolFiveHour: 10000, Reserve: 100, SafetyMargin: 200}
	// 池剩余=10000-9800=200，不大于Reserve+SafetyMargin=300 → 双双拒绝
	for _, pid := range []string{"pa", "pb"} {
		d, _ := s.Check(context.Background(), pid, "acc", l, now)
		if !d.Over || d.Reason != "pool_exhausted" {
			t.Fatalf("%s must be pool_exhausted: %+v", pid, d)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/quota -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/quota/service.go`：

```go
package quota

import (
	"context"
	"time"
)

type UsageReader interface {
	WindowUsed(ctx context.Context, projectID string, since time.Time) (int64, error)
	PoolUsed(ctx context.Context, accountID string, since time.Time) (int64, error)
}

type Limits struct {
	FiveHour, SevenDay             int64
	PoolFiveHour, Reserve, SafetyMargin int64
}

type Decision struct {
	Over                       bool
	Reason                     string
	FiveHourUsed, SevenDayUsed int64
}

type Service struct{ Reader UsageReader }

func (s Service) Check(ctx context.Context, projectID, accountID string, l Limits, now time.Time) (Decision, error) {
	var d Decision
	var err error
	if d.FiveHourUsed, err = s.Reader.WindowUsed(ctx, projectID, now.Add(-5*time.Hour)); err != nil {
		return d, err
	}
	if d.SevenDayUsed, err = s.Reader.WindowUsed(ctx, projectID, now.Add(-7*24*time.Hour)); err != nil {
		return d, err
	}
	switch {
	case d.FiveHourUsed >= l.FiveHour:
		d.Over, d.Reason = true, "five_hour_limit"
	case d.SevenDayUsed >= l.SevenDay:
		d.Over, d.Reason = true, "seven_day_limit"
	default:
		poolUsed, err := s.Reader.PoolUsed(ctx, accountID, now.Add(-5*time.Hour))
		if err != nil {
			return d, err
		}
		if l.PoolFiveHour-poolUsed <= l.Reserve+l.SafetyMargin {
			d.Over, d.Reason = true, "pool_exhausted"
		}
	}
	return d, nil
}

// AccountUsageProvider：spec预留接口，未来接获授权的上游用量来源做校准；第一版无实现。
type AccountUsageProvider interface {
	Snapshot(ctx context.Context, pool string) (UsageSnapshot, error)
}

type UsageSnapshot struct {
	FiveHourUsedPct float64
	SevenDayUsedPct float64
	FiveHourResetAt time.Time
	SevenDayResetAt time.Time
}
```

生产`UsageReader`（`store`包追加）：`SELECT COALESCE(SUM(weighted_units),0) FROM usage_events WHERE project_id=$1 AND occurred_at > $2`；PoolUsed按`JOIN projects ON account_id`聚合。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/quota -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: enforce independent project usage budgets"
```

### Task 10: control-api——认证、连接信息与用量门户

**Files:**
- Create: `internal/control/http.go`
- Create: `internal/control/http_test.go`
- Create: `web/templates/usage.html`
- Modify: `cmd/control-api/main.go`

**Interfaces:**
- Consumes: `project.Resolver`、`token.Mint`、`quota.Service`、`storage.Index`
- Produces（HTTP合同，CLI与门户都依赖）：
  - `POST /v1/auth/exchange`，body `{"cdk":"ccw_..."}` → `200 {"session_token":"...","project_id":"...","project_slug":"..."}`；无效CDK一律`401 {"error":"invalid_cdk"}`
  - `GET /v1/connection`（Header `Authorization: Bearer <session_token>`）→ `ConnectionResponse`（spec第4节字段：project_id、project_slug、terminal_url、sync_url、terminal_token、sync_token、disk_used/limit、five_hour_used/limit、seven_day_used/limit、over、over_reason）；`over==true`时不签发terminal_token/sync_token（空串）
  - `GET /usage`（会话令牌）→ SSR HTML，30秒`<meta http-equiv="refresh" content="30">`
  - `control.Server`结构：`New(resolver project.Resolver, key []byte, q quota.Service, idx storage.Index, limitsFor func(project.Project) quota.Limits, agentBase string) *Server`；`(*Server).Handler() http.Handler`

- [ ] **Step 1: 写失败测试**

`internal/control/http_test.go`：

```go
package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccw/internal/auth"
	"ccw/internal/project"
	"ccw/internal/quota"
	"ccw/internal/storage"
	"ccw/internal/token"
)

type fixedReader struct{ perProject map[string]int64 }

func (f fixedReader) WindowUsed(_ context.Context, pid string, _ time.Time) (int64, error) {
	return f.perProject[pid], nil
}
func (f fixedReader) PoolUsed(_ context.Context, _ string, _ time.Time) (int64, error) { return 0, nil }

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	cdkA, _ := auth.NewCDK()
	hashA, _ := auth.HashCDK(cdkA)
	resolver := project.NewMemoryResolver(map[string]project.Project{
		hashA: {ID: "pa", AccountID: "acc", Slug: "project-a", DiskLimit: 1000, FiveHourLimit: 100, SevenDayLimit: 1000},
	})
	key := make([]byte, 32)
	q := quota.Service{Reader: fixedReader{perProject: map[string]int64{"pa": 10}}}
	s := New(resolver, key, q, storage.NewMemoryIndex(),
		func(p project.Project) quota.Limits {
			return quota.Limits{FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit, PoolFiveHour: 1 << 40}
		}, "ws://agent:8081")
	return s, cdkA
}

func TestExchangeAndConnection(t *testing.T) {
	s, cdk := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/auth/exchange", "application/json",
		strings.NewReader(`{"cdk":"`+cdk+`"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("exchange failed: %v %d", err, resp.StatusCode)
	}
	var ex struct {
		SessionToken string `json:"session_token"`
		ProjectID    string `json:"project_id"`
	}
	json.NewDecoder(resp.Body).Decode(&ex)
	if ex.ProjectID != "pa" || ex.SessionToken == "" {
		t.Fatalf("bad exchange payload: %+v", ex)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/connection", nil)
	req.Header.Set("Authorization", "Bearer "+ex.SessionToken)
	resp2, _ := http.DefaultClient.Do(req)
	if resp2.StatusCode != 200 {
		t.Fatalf("connection status %d", resp2.StatusCode)
	}
	var conn ConnectionResponse
	json.NewDecoder(resp2.Body).Decode(&conn)
	if conn.TerminalToken == "" || conn.SyncToken == "" || conn.Over {
		t.Fatalf("expected tokens for non-over project: %+v", conn)
	}
	// 终端令牌audience必须是terminal，不能拿去开同步
	if _, err := token.Verify(make([]byte, 32), conn.TerminalToken, token.AudSync, timeNow()); err == nil {
		t.Fatal("terminal token must not verify as sync")
	}
}

func TestInvalidCDKUniformError(t *testing.T) {
	s, _ := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/v1/auth/exchange", "application/json",
		strings.NewReader(`{"cdk":"ccw_wrong"}`))
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestOverProjectGetsNoConnectionTokens(t *testing.T) {
	s, cdk := newTestServer(t)
	s.Quota = quota.Service{Reader: fixedReader{perProject: map[string]int64{"pa": 100}}} // 达到FiveHour=100
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/v1/auth/exchange", "application/json",
		strings.NewReader(`{"cdk":"`+cdk+`"}`))
	var ex struct {
		SessionToken string `json:"session_token"`
	}
	json.NewDecoder(resp.Body).Decode(&ex)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/connection", nil)
	req.Header.Set("Authorization", "Bearer "+ex.SessionToken)
	resp2, _ := http.DefaultClient.Do(req)
	var conn ConnectionResponse
	json.NewDecoder(resp2.Body).Decode(&conn)
	if !conn.Over || conn.OverReason != "five_hour_limit" || conn.TerminalToken != "" || conn.SyncToken != "" {
		t.Fatalf("over project must get no tokens: %+v", conn)
	}
}
```

注：测试文件顶部需要`"time"`导入与`timeNow`辅助（`func timeNow() time.Time { return time.Now() }`可直接放在测试文件里）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/control -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

`internal/control/http.go`要点（完整实现）：

```go
package control

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"time"

	"ccw/internal/project"
	"ccw/internal/quota"
	"ccw/internal/storage"
	"ccw/internal/token"
)

//go:embed templates
var templatesFS embed.FS // web/templates在构建时复制到internal/control/templates

type ConnectionResponse struct {
	ProjectID     string `json:"project_id"`
	ProjectSlug   string `json:"project_slug"`
	TerminalURL   string `json:"terminal_url"`
	SyncURL       string `json:"sync_url"`
	TerminalToken string `json:"terminal_token"`
	SyncToken     string `json:"sync_token"`
	DiskUsed      int64  `json:"disk_used"`
	DiskLimit     int64  `json:"disk_limit"`
	FiveHourUsed  int64  `json:"five_hour_used"`
	FiveHourLimit int64  `json:"five_hour_limit"`
	SevenDayUsed  int64  `json:"seven_day_used"`
	SevenDayLimit int64  `json:"seven_day_limit"`
	Over          bool   `json:"over"`
	OverReason    string `json:"over_reason,omitempty"`
}

type Server struct {
	Resolver  project.Resolver
	Key       []byte
	Quota     quota.Service
	Index     storage.Index
	LimitsFor func(project.Project) quota.Limits
	AgentBase string
	Projects  map[string]project.Project // session claims → project元数据缓存
}

func New(r project.Resolver, key []byte, q quota.Service, idx storage.Index,
	limitsFor func(project.Project) quota.Limits, agentBase string) *Server {
	return &Server{Resolver: r, Key: key, Quota: q, Index: idx,
		LimitsFor: limitsFor, AgentBase: agentBase, Projects: map[string]project.Project{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/exchange", s.exchange)
	mux.HandleFunc("GET /v1/connection", s.connection)
	mux.HandleFunc("GET /usage", s.usagePage)
	return mux
}

func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CDK string `json:"cdk"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		httpErr(w, 401, "invalid_cdk")
		return
	}
	p, err := s.Resolver.ResolveCDK(r.Context(), body.CDK)
	if err != nil {
		httpErr(w, 401, "invalid_cdk") // 统一错误：不泄露CDK是否存在/过期/禁用
		return
	}
	s.Projects[p.ID] = p
	tok, err := token.Mint(s.Key, p.ID, token.AudSession, 15*time.Minute, time.Now())
	if err != nil {
		httpErr(w, 500, "internal")
		return
	}
	json.NewEncoder(w).Encode(map[string]string{
		"session_token": tok, "project_id": p.ID, "project_slug": p.Slug,
	})
}

func (s *Server) authed(r *http.Request) (project.Project, bool) {
	h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := token.Verify(s.Key, h, token.AudSession, time.Now())
	if err != nil {
		return project.Project{}, false
	}
	p, ok := s.Projects[c.ProjectID]
	return p, ok
}

func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authed(r)
	if !ok {
		httpErr(w, 401, "unauthorized")
		return
	}
	now := time.Now()
	d, err := s.Quota.Check(r.Context(), p.ID, p.AccountID, s.LimitsFor(p), now)
	if err != nil {
		httpErr(w, 500, "internal")
		return
	}
	disk, _ := s.Index.DiskUsed(r.Context(), p.ID)
	resp := ConnectionResponse{
		ProjectID: p.ID, ProjectSlug: p.Slug,
		TerminalURL: s.AgentBase + "/v1/terminal", SyncURL: s.AgentBase + "/v1/sync",
		DiskUsed: disk, DiskLimit: p.DiskLimit,
		FiveHourUsed: d.FiveHourUsed, FiveHourLimit: p.FiveHourLimit,
		SevenDayUsed: d.SevenDayUsed, SevenDayLimit: p.SevenDayLimit,
		Over: d.Over, OverReason: d.Reason,
	}
	if disk >= p.DiskLimit && !resp.Over {
		resp.Over, resp.OverReason = true, "disk_limit"
	}
	if !resp.Over {
		resp.TerminalToken, _ = token.Mint(s.Key, p.ID, token.AudTerminal, 15*time.Minute, now)
		resp.SyncToken, _ = token.Mint(s.Key, p.ID, token.AudSync, 15*time.Minute, now)
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authed(r)
	if !ok {
		httpErr(w, 401, "unauthorized")
		return
	}
	t := template.Must(template.ParseFS(templatesFS, "templates/usage.html"))
	d, _ := s.Quota.Check(r.Context(), p.ID, p.AccountID, s.LimitsFor(p), time.Now())
	disk, _ := s.Index.DiskUsed(r.Context(), p.ID)
	t.Execute(w, map[string]any{
		"Project": p, "Decision": d, "DiskUsed": disk,
	})
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
```

`web/templates/usage.html`（同时复制到`internal/control/templates/`）：

```html
<!doctype html>
<meta charset="utf-8">
<meta http-equiv="refresh" content="30">
<title>项目用量 - {{.Project.Slug}}</title>
<h1>{{.Project.Slug}}</h1>
<p><strong>注意：以下为内部项目额度（估算单位），不是Anthropic官方订阅余额。</strong></p>
<ul>
  <li>5小时窗口：{{.Decision.FiveHourUsed}}/{{.Project.FiveHourLimit}}内部单位</li>
  <li>7天窗口：{{.Decision.SevenDayUsed}}/{{.Project.SevenDayLimit}}内部单位</li>
  <li>磁盘：{{.DiskUsed}}/{{.Project.DiskLimit}}字节（逻辑字节，GiB=1073741824字节）</li>
  {{if .Decision.Over}}<li>状态：已超额（{{.Decision.Reason}}）</li>{{else}}<li>状态：正常</li>{{end}}
</ul>
```

`cmd/control-api/main.go`改为：加载配置→`store.New`+`Migrate`→用store实现的Resolver/UsageReader/PGIndex组装`control.New(...)`→`http.ListenAndServe(cfg.ListenAddr, s.Handler())`。管理端（建项目/发CDK）以`net.Listen("unix", cfg.AdminSocketPath)`挂独立mux：`POST /admin/projects`、`POST /admin/cdks`（返回一次性明文），不注册到公网Handler。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/control -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: expose auth, connection and usage endpoints"
```

### Task 11: 本地CLI（三平台）

**Files:**
- Create: `internal/control/client.go`
- Create: `internal/control/client_test.go`
- Create: `internal/sync/client.go`
- Create: `internal/sync/client_test.go`
- Modify: `cmd/cclaude/main.go`

**Interfaces:**
- Consumes: Task 10的HTTP合同、Task 5的WebSocket终端协议、Task 6的`Diff`/`FileEntry`
- Produces:
  - `control.Client{Base string}`：`Exchange(ctx, cdk string) (ExchangeResult, error)`；`Connection(ctx, sessionToken string) (ConnectionResponse, error)`（指数退避重试：1s起倍增，上限30s）
  - `sync.LocalIndex`：项目根`.cclaude/index.json`读写本地`[]FileEntry`
  - `sync.ScanDir(root string) ([]FileEntry, error)`——与DirStore.Manifest同规则（排除`.cclaude/`与默认排除项），供watcher静默期后全量对比
  - CLI主流程：读CDK（终端交互输入，禁止argv；`CCW_CDK`环境变量仅供测试）→exchange→首次同步→fsnotify watcher（500ms静默窗口）→连接终端（raw mode+resize）→状态栏一行：`[project-a] 5h:10/100 7d:60/1000 disk:1.2MiB/1GiB net:ok`

- [ ] **Step 1: 写失败测试**

`internal/control/client_test.go`：

```go
package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestExchangeNeverLogsCDK(t *testing.T) {
	// 约定：Client所有error字符串不得包含CDK明文
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid_cdk"}`))
	}))
	defer srv.Close()
	c := Client{Base: srv.URL}
	_, err := c.Exchange(context.Background(), "ccw_secret_value")
	if err == nil {
		t.Fatal("want error")
	}
	if contains := "ccw_secret_value"; len(err.Error()) > 0 && strings.Contains(err.Error(), contains) {
		t.Fatalf("error leaks cdk: %v", err)
	}
}

func TestExchangeRetriesWithBackoff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(502) // 网关暂时不可用→重试
			return
		}
		w.Write([]byte(`{"session_token":"tok","project_id":"pa","project_slug":"project-a"}`))
	}))
	defer srv.Close()
	c := Client{Base: srv.URL, RetryBase: 0} // 测试中退避基数设0加速
	res, err := c.Exchange(context.Background(), "ccw_x")
	if err != nil || res.SessionToken != "tok" {
		t.Fatalf("retry failed: %+v %v", res, err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}
```

（顶部补`"strings"`导入。）

`internal/sync/client_test.go`：

```go
package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanDirMatchesManifestRules(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, ".cclaude"), 0o755)
	os.WriteFile(filepath.Join(root, ".cclaude", "index.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o644)
	entries, err := ScanDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "main.go" {
		t.Fatalf(".cclaude and .env must be excluded: %+v", entries)
	}
}

func TestLocalIndexRoundTrip(t *testing.T) {
	root := t.TempDir()
	idx := LocalIndex{Root: root}
	in := []FileEntry{{Path: "a.go", Size: 3, SHA256: "abc", Revision: 2}}
	if err := idx.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := idx.Load()
	if err != nil || len(out) != 1 || out[0] != in[0] {
		t.Fatalf("round trip failed: %+v %v", out, err)
	}
}

func TestUnchangedFileNotReuploaded(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("abc"), 0o644)
	local, _ := ScanDir(root)
	// 远端清单与本地一致→Diff不产生任何传输
	p := Diff(local, local)
	if len(p.Upload)+len(p.Download) != 0 {
		t.Fatalf("unchanged files must not transfer: %+v", p)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/control ./internal/sync -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

```bash
go get github.com/fsnotify/fsnotify@latest golang.org/x/term@latest
```

`internal/control/client.go`：

```go
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ExchangeResult struct {
	SessionToken string `json:"session_token"`
	ProjectID    string `json:"project_id"`
	ProjectSlug  string `json:"project_slug"`
}

type Client struct {
	Base      string
	RetryBase time.Duration // 默认1s；测试可设0
}

// doJSON带指数退避：5xx与网络错误重试（上限5次），4xx立即失败。
// 错误信息只含状态码，绝不包含请求体（防CDK泄漏）。
func (c Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	backoff := c.RetryBase
	if backoff == 0 {
		backoff = time.Second
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
				backoff = min(backoff*2, 30*time.Second)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("control: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("control: request failed (network)")
			continue
		}
		func() {
			defer resp.Body.Close()
			switch {
			case resp.StatusCode >= 500:
				lastErr = fmt.Errorf("control: server error %d", resp.StatusCode)
			case resp.StatusCode >= 400:
				lastErr = fmt.Errorf("control: rejected with status %d", resp.StatusCode)
			default:
				lastErr = json.NewDecoder(resp.Body).Decode(out)
			}
		}()
		if lastErr == nil || (lastErr != nil && bytes.Contains([]byte(lastErr.Error()), []byte("rejected"))) {
			return lastErr
		}
	}
	return lastErr
}

func (c Client) Exchange(ctx context.Context, cdk string) (ExchangeResult, error) {
	var out ExchangeResult
	err := c.doJSON(ctx, "POST", "/v1/auth/exchange", map[string]string{"cdk": cdk}, &out)
	return out, err
}

func (c Client) Connection(ctx context.Context, sessionToken string) (ConnectionResponse, error) {
	var out ConnectionResponse
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/v1/connection", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("control: request failed (network)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("control: rejected with status %d", resp.StatusCode)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
```

`internal/sync/client.go`：

```go
package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type LocalIndex struct{ Root string }

func (l LocalIndex) path() string { return filepath.Join(l.Root, ".cclaude", "index.json") }

func (l LocalIndex) Load() ([]FileEntry, error) {
	b, err := os.ReadFile(l.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []FileEntry
	return out, json.Unmarshal(b, &out)
}

func (l LocalIndex) Save(es []FileEntry) error {
	if err := os.MkdirAll(filepath.Dir(l.path()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(es, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.path(), b, 0o644)
}

func ScanDir(root string) ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(os.PathSeparator)))
		if d.IsDir() {
			if rel != "" && DefaultExcluded(rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if DefaultExcluded(rel) || strings.HasPrefix(filepath.Base(p), ".cclaude.tmp.") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return out, err
}
```

`cmd/cclaude/main.go`主流程（完整实现，约150行）：

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"golang.org/x/term"

	"ccw/internal/control"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	base := os.Getenv("CCW_API")
	if base == "" {
		base = "https://ccw.example.com"
	}
	cdk := os.Getenv("CCW_CDK") // 仅测试用
	if cdk == "" {
		fmt.Fprint(os.Stderr, "CDK: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd())) // 不回显，不进shell历史
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read cdk:", err)
			os.Exit(1)
		}
		cdk = string(b)
	}

	c := control.Client{Base: base}
	ex, err := c.Exchange(ctx, cdk)
	if err != nil {
		fmt.Fprintln(os.Stderr, err) // Client保证错误不含CDK
		os.Exit(1)
	}
	conn, err := c.Connection(ctx, ex.SessionToken)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if conn.Over {
		fmt.Fprintf(os.Stderr, "项目额度受限（%s），无法连接。\n", conn.OverReason)
		os.Exit(2)
	}
	fmt.Printf("[%s] 5h:%d/%d 7d:%d/%d disk:%d/%d\n", conn.ProjectSlug,
		conn.FiveHourUsed, conn.FiveHourLimit, conn.SevenDayUsed, conn.SevenDayLimit,
		conn.DiskUsed, conn.DiskLimit)

	cwd, _ := os.Getwd()
	go runSyncLoop(ctx, cwd, conn)            // 首次全量+watcher增量，断线退避重连
	if err := runTerminal(ctx, conn); err != nil { // raw mode+WebSocket+resize；断线退避重连
		fmt.Fprintln(os.Stderr, err)
	}
}
```

`runSyncLoop`与`runTerminal`在同文件实现：`runTerminal`用`gorilla/websocket`连`conn.TerminalURL+"?token="+conn.TerminalToken`，`term.MakeRaw(stdin)`进raw mode（退出时恢复），goroutine双向拷贝，监听`SIGWINCH`（Windows上轮询`term.GetSize`每2秒）发resize Text帧；`runSyncLoop`调用`ScanDir`+`LocalIndex`+`Diff`，通过同步WebSocket端点执行Plan（上传走`WriteTemp`语义的消息，冲突时按`ConflictName`落地并打印提示），fsnotify事件进入500ms定时器去抖后重扫。两个循环断线后均以1s起倍增退避重连（上限30s），并用会话令牌重新`Connection`换新的短期令牌。

- [ ] **Step 4: 运行测试与三平台交叉编译**

Run: `go test ./internal/control ./internal/sync -v`
Expected: PASS

Run: `GOOS=windows GOARCH=amd64 go build ./cmd/cclaude && GOOS=darwin GOARCH=arm64 go build ./cmd/cclaude && GOOS=linux GOARCH=amd64 go build ./cmd/cclaude`
Expected: 三平台全部编译通过（Windows路径规则已由`SafeRelPath`反斜杠测试覆盖）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: add cross-platform remote workspace cli"
```

### Task 12: 同步端点接线、e2e验收与部署

**Files:**
- Create: `internal/sync/ws.go`（worker-agent侧同步WebSocket端点：验AudSync令牌→按消息调DirStore+PGIndex+Gate）
- Modify: `cmd/worker-agent/main.go`（挂`/v1/sync`路由、启动每30秒的usage.Collector循环、容器名改为store查询）
- Create: `tests/e2e/two_projects_test.go`
- Create: `deploy/control-api.service`
- Create: `deploy/worker-agent.service`
- Create: `deploy/Dockerfile.claude`（含官方Claude Code、git、tmux、非root用户claude的项目容器镜像）
- Create: `docs/runbook.md`

**Interfaces:**
- Consumes: 前面全部任务的产出
- Produces: 可部署系统与e2e证据

同步WebSocket消息协议（JSON Text帧+文件内容Binary帧交替）：

```json
{"op":"hello","project_id":"...","device":"laptop"}
{"op":"manifest"}                          → 服务端回{"op":"manifest","entries":[FileEntry...]}
{"op":"put","entry":FileEntry}             → 紧随一个Binary帧为文件内容；服务端校验SHA与Gate后回{"op":"ack","path":"...","revision":N}或{"op":"reject","path":"...","reason":"disk_full|sha_mismatch|unsafe_path"}
{"op":"get","path":"..."}                  → 服务端回{"op":"file","entry":FileEntry}+Binary帧
{"op":"delete","entry":FileEntry}          → tombstone；回ack
```

- [ ] **Step 1: 写e2e测试（在目标VPS或有Docker的环境运行；无Docker自动skip）**

`tests/e2e/two_projects_test.go`骨架必须覆盖spec第13节全部场景，每个场景一个子测试：

```go
package e2e

import (
	"os/exec"
	"testing"
)

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available; e2e runs on target VPS")
	}
}

func TestTwoProjectsEndToEnd(t *testing.T) {
	requireDocker(t)
	// 子测试按序执行，共享docker compose环境：
	t.Run("bootstrap", testBootstrap)                   // compose起postgres，迁移，建A/B项目与CDK，起两容器
	t.Run("isolation_volumes", testVolumeIsolation)     // docker inspect：A容器挂载列表无B卷
	t.Run("isolation_cdk", testCDKIsolation)            // CDK-A取connection只能拿到A；伪造B的project_id被拒
	t.Run("sync_roundtrip", testSyncRoundtrip)          // 本地→云端→云端改文件→回本地
	t.Run("sync_conflict", testSyncConflict)            // 双端同改→conflict副本，无覆盖
	t.Run("disk_quota", testDiskQuota)                  // 超限上传被reject，删除仍允许，页面数值与SUM一致
	t.Run("terminal_reconnect", testTerminalReconnect)  // 断开WebSocket重连后capture-pane可见断开前标记
	t.Run("quota_a_over_b_free", testQuotaIsolation)    // 灌A的usage_events到超限：A拒连，B正常
	t.Run("rebuild_keeps_volumes", testRebuildKeepsData) // docker rm A容器→EnsureProjectRuntime重建→数据仍在
	t.Run("no_secrets_in_logs", testNoSecretLeak)       // 全部服务日志grep无ccw_前缀明文与OAuth token
}
```

每个子测试函数在同文件实现，调用`docker`、`curl`（或Go http）与`tmux capture-pane`完成断言；`testBootstrap`失败则`t.Fatal`终止后续。

- [ ] **Step 2: 运行确认skip/失败路径**

Run: `go test ./tests/e2e -v`（开发机）
Expected: SKIP（docker not available）——测试结构编译通过

- [ ] **Step 3: 实现`internal/sync/ws.go`与worker-agent接线**

`ws.go`按上方协议实现：每个`put`在事务中执行`Gate.Allow(used, oldSize, newSize)`→`WriteTemp`→SHA比对`entry.SHA256`→`Promote`→`Index.Upsert`；任何一步失败回`reject`并删tmp。`cmd/worker-agent/main.go`补：`store.New`、每项目一个`usage.Collector`每30秒`Scan`、`mux.HandleFunc("GET /v1/sync", ...)`；终端与同步连接建立/断开时写`sessions`表（`connected_at`/`last_seen_at`/`state`），供门户显示会话状态。

`deploy/control-api.service`：

```ini
[Unit]
Description=ccw control-api
After=network-online.target postgresql.service

[Service]
User=ccw
EnvironmentFile=/etc/ccw/control-api.env
ExecStart=/usr/local/bin/control-api
Restart=always
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/run/ccw

[Install]
WantedBy=multi-user.target
```

`deploy/worker-agent.service`同构（User=ccw加docker组，ReadWritePaths含workspace根）。

`deploy/Dockerfile.claude`：

```dockerfile
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    git tmux curl ca-certificates nodejs npm && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
RUN useradd -m -s /bin/bash claude
USER claude
WORKDIR /workspace
```

`docs/runbook.md`：安装Docker与PostgreSQL→建库→跑迁移→systemd安装→管理socket建A/B项目与CDK→按`docs/admin-login-runbook.md`完成两容器登录→备份（每日`pg_dump`+`docker run --rm -v <vol>:/v alpine tar`快照）→VPS重启恢复流程→R2的24小时双登录验证记录表。

- [ ] **Step 4: 在目标VPS运行全量验收**

Run（VPS）: `go test ./... && go test -race ./... && go test ./tests/e2e -v`
Expected: 全部PASS，无SKIP（VPS有Docker）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "test: verify isolated dual-project remote workspaces"
```

---

## 验收对照（spec第14节12条）

| 验收条 | 覆盖任务 |
|---|---|
| CDK-A永不能连Project B | Task 2、10、12（isolation_cdk） |
| A的Claude读不到B路径 | Task 4、12（isolation_volumes） |
| Claude HOME/历史/tmux完全分开 | Task 4、5、12 |
| 断网不删项目不杀tmux | Task 5、12（terminal_reconnect） |
| 容器重建不丢三卷 | Task 4、12（rebuild_keeps_volumes） |
| 同步逻辑字节与页面精确一致 | Task 7、12（disk_quota） |
| A超额停、B预留可用 | Task 9、12（quota_a_over_b_free） |
| 池不足时统一保护 | Task 9（pool_exhausted） |
| CDK无SSH/Docker权限 | Task 4、10（管理socket分离） |
| 无CDK明文/OAuth凭据泄漏 | Task 2、11、12（no_secrets_in_logs） |
| macOS与Win/Linux同等验收 | Task 11（交叉编译+路径测试）+VPS手工验收 |
| 用量采集无重复无遗漏 | Task 8（真实样例）、12 |
