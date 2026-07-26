# 双项目远程Claude工作空间Implementation Plan（v3）

> # ⚠️ 已归档，不再维护，勿按此实施
>
> **归档于2026-07-26。**本文件保留为历史记录（Task 0–13当初是怎么规划的），**不是待办清单**。
>
> - **71个checkbox一个都没勾，不反映真实进度。**进度的唯一可信来源是`docs/STATUS.md`与git log
> - **部分内容已被明确推翻**，按此实施会做错事，最典型的三条：
>   - **Task 13（文件系统硬配额）已决定不做**——脚本与compose卷名不兼容，执行了也不生效；见`docs/design-deviations.md`的D5与`docs/STATUS.md`的T1。**不要实现它,也不要执行`deploy/quota-setup.sh`**
>   - **Task 0 Step 1（24小时双登录验证）已被绕过**——改为共享Claude HOME，原验证目标不再适用；见D1
>   - **"双项目"已过时**——单节点上限改为3个项目容器、单项目配额15 GiB；见console-fleet-design §7.6
> - **当前工作的依据**是`docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md`（v2 Console设计）与`docs/superpowers/plans/2026-07-26-compose-render-plan.md`
>
> 读本文件前先读`docs/STATUS.md`（含「已知取舍」一节）与`docs/design-deviations.md`。

> **v3修订说明：**本计划已按[v2审查与v3调整要求](../specs/2026-07-19-remote-claude-workspace-v2-review-adjustments.md)（最高权威）与[审计与修订说明](../specs/2026-07-19-remote-claude-workspace-audit-corrections.md)完成修订；配套设计为[Design Spec v3](../specs/2026-07-19-remote-claude-workspace-design.md)。
>
> **执行状态（2026-07-19）：**v3与调整后顺序已获用户批准，编码开始。24小时双登录验证消耗真实Claude额度，运行前仍需用户单独同意。
>
> **v2→v3主要变更：**①终端附着改`docker exec -it`（真实TTY）；②公网/后端路径合同+Caddy前缀重写与集成测试；③"单用途令牌"更名为"2分钟短期连接令牌"（可重连，worker接入时实时复查额度）；④同步改三方判断（base_revision+base_sha256+current_sha256+本地状态）；⑤同步路径生产安全边界改`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`，tmp随机独占+上限加一字节判超；⑥新增Task 13文件系统硬配额（默认每项目loop文件系统）；⑦JSONL采集器按OffsetStore+半行拼接+同requestId最终值语义重写；⑧CLI超额不退出，cleanup模式端到端可达，删除`?token=`残留；⑨依赖与镜像版本全部固定（deploy/versions.lock）；⑩门户定案为localhost+SSH隧道；⑪服务默认只监听127.0.0.1；⑫描述性段落不再自称"完整实现"，改为接口/状态机/测试先行的工程步骤。

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
- 仓库根：`/root/code1/remote-claude-workspace/`；所有路径相对仓库根。仓库内`remote-claude-workspace/`子目录是用户本地机器的FUSE同步挂载点，已gitignore，任何任务不得写入或删除该目录。
- migration文件只保留`internal/store/migrations/`一份源，用`schema_migrations`表保证每个迁移只执行一次。
- terminal/sync连接令牌TTL为2分钟（**短期令牌，非单用途**：有效期内允许重连，worker每次接入实时复查额度）；令牌只放`Authorization`头或首个认证帧，全计划禁止出现`?token=`。
- 整体池保护必须同时计算5小时与7天两个窗口。
- control-api与worker-agent默认只监听`127.0.0.1:8080`/`127.0.0.1:8081`；公网只有反向代理的443，路径按spec第3节合同重写。
- **版本固定：**任何实施步骤禁止`@latest`或未固定版本安装；本计划指定的版本在Task 1锁入go.mod/go.sum，镜像与工具版本记录于`deploy/versions.lock`。
- **诚实表述：**计划中给出代码的步骤按代码执行；只给接口/状态机/规则的步骤是"实现规格"，编码时测试先行补全，不得跳过其中任何列出的行为点。

## 阶段映射（调整版，2026-07-19经用户批准）

**调整原则：**纯逻辑先行（本机单元测试+假Docker API，零外部依赖）；零成本验证穿插；需要VPS/账号的验证后置为**集成/上线闸门**而非编码闸门。Task 0据此拆分：

| Task 0拆分项 | 何时做 | 依赖 |
|---|---|---|
| Step 3脱敏JSONL样例与requestId语义 | **立即**（编码Task 8前完成） | 仅本机现成`~/.claude`会话数据，零成本 |
| Step 2 tmux容器原型 | Task 4/5编码前后皆可，尽早 | 任一有Docker的机器，容器内用`sh`代替`claude`，不消耗额度 |
| Step 1 24小时双登录验证 | **上线闸门**：引入第二项目并行（Phase 4）之前完成 | VPS/Docker机+用户Claude账号+**用户单独同意**（消耗额度） |

| 执行序 | 内容 | 对应任务 | 依赖 |
|---|---|---|---|
| ① | 仓库与骨架 | Task 1 | 本机（装Go） |
| ② | 纯逻辑层：CDK/令牌/同步/配额/采集/闸门 | Task 2、3、6、7、8、9 | 本机单元测试；Task 8前完成Task 0 Step 3 |
| ③ | 容器与终端逻辑（假Docker API+本机tmux） | Task 4、5 | 本机；穿插Task 0 Step 2原型 |
| ④ | control-api与CLI | Task 10、11 | 本机（三平台冒烟可后置到⑤前） |
| ⑤ | 真实集成：e2e/硬配额/部署/备份 | Task 12、13 | **目标VPS**；此前完成Task 0 Step 1 |

---

### Task 0: 架构阻断验证（Phase 1，先于全部编码）

**Files:**
- Create: `docs/phase1-evidence/dual-login-24h.md`（双登录验证记录）
- Create: `docs/phase1-evidence/tmux-prototype.md`（tmux原型记录）
- Create: `internal/usage/testdata/session-sample.jsonl`（**脱敏**真实样例，替换Task 8中的手工样例）

**Interfaces:**
- Produces: 三份证据文件与一份脱敏JSONL样例；Task 8的解析测试必须以该样例为准。

- [ ] **Step 1: 双Claude HOME同账号登录24小时验证**（**消耗真实Claude额度，须用户明确同意后才运行**）——在任一有Docker的Linux机器上起两个容器，各自独立Claude HOME卷，分别官方登录；每小时各发起一次正常对话（可用`claude -p "ping"`）；24小时后检查两边凭据均未失效。结果（通过/失败、时间线、Claude Code版本）写入`dual-login-24h.md`。**失败→停止，回改设计为分时使用或官方API接入。**
- [ ] **Step 2: tmux容器原型**——用`sleep infinity`作PID 1起最小容器，执行审计§4.1的三步会话准备流程（has-session→new-session -d→attach），验证：断开attach后会话存活、重连可见此前输出、容器stop/start后按`claude --continue`策略恢复。记录到`tmux-prototype.md`。
- [ ] **Step 3: 脱敏JSONL样例与requestId语义**——从真实Claude HOME提取一段多轮（含工具调用）会话JSONL，把文本内容替换为占位符、只保留结构与usage字段；确认同一requestId出现多条记录时哪条代表最终用量（最终/最大计数规则），把结论写进样例文件头部注释与Task 8的解析规则。
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "docs: record phase 1 architecture-blocking validation evidence"
```

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

- [ ] **Step 0: 安装Go工具链（开发机当前没有Go；不使用破坏性命令）**

```bash
test -d /usr/local/go && echo "Go already installed, skip" || {
  curl -fsSL https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -o "$HOME/go1.22.5.tgz"
  tar -C /usr/local -xzf "$HOME/go1.22.5.tgz" && rm "$HOME/go1.22.5.tgz"
}
export PATH=$PATH:/usr/local/go/bin && go version
```

Expected: `go version go1.22.5 linux/amd64`（如无外网，使用系统包管理器`apt install golang-go`并确认≥1.22；若`/usr/local/go`已存在旧版本，人工确认后再决定是否替换，不得脚本内`rm -rf`）

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
	// 默认只监听回环地址（审查§3.1）：绑定所有网卡的":8080"形式是错误答案
	if c.ListenAddr != "127.0.0.1:8080" || c.AgentListenAddr != "127.0.0.1:8081" {
		t.Fatalf("defaults must bind loopback only: %+v", c)
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
		ListenAddr:      or(getenv("CCW_LISTEN_ADDR"), "127.0.0.1:8080"),
		AgentListenAddr: or(getenv("CCW_AGENT_LISTEN_ADDR"), "127.0.0.1:8081"),
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
- Create: `internal/store/migrations/001_initial.sql`（唯一一份migration源，embed使用，禁止复制第二份）
- Create: `internal/auth/cdk.go`
- Create: `internal/auth/cdk_test.go`
- Create: `internal/store/postgres.go`
- Create: `internal/project/service.go`
- Create: `internal/project/service_test.go`

**Interfaces:**
- Consumes: `config.Config`
- Produces:
  - `auth.NewCDK() (plain, publicID string, err error)`——CDK格式`ccw_<public-id>.<random-secret>`，public-id为8字节hex（O(1)检索键），secret为32字节hex
  - `auth.SplitCDK(plain string) (publicID, secret string, err error)`
  - `auth.HashSecret(secret string) (string, error)`（编码为`argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>`）
  - `auth.VerifySecret(secret, encoded string) bool`
  - `store.New(ctx, dsn string) (*Store, error)`、`(*Store).Migrate(ctx) error`（`schema_migrations`表，每个迁移只执行一次）、`(*Store).GetProjectByID(ctx, id string) (project.Project, error)`
  - `project.Project{ID, AccountID, Slug, ContainerName string; DiskLimit, FiveHourLimit, SevenDayLimit int64}`
  - `project.Resolver`接口：`ResolveCDK(ctx, plain string) (Project, error)`——按public-id做O(1)查询后验证secret；错误恒为`project.ErrInvalidCDK`（不区分不存在/禁用/过期）；限速在Task 10的HTTP层实现（IP与public-id双维度）

- [ ] **Step 1: 写失败测试**

`internal/auth/cdk_test.go`：

```go
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
	cdkA, pubA, _ := auth.NewCDK()
	cdkB, pubB, _ := auth.NewCDK()
	_, secA, _ := auth.SplitCDK(cdkA)
	_, secB, _ := auth.SplitCDK(cdkB)
	hashA, _ := auth.HashSecret(secA)
	hashB, _ := auth.HashSecret(secB)
	r := NewMemoryResolver(map[string]Entry{
		pubA: {SecretHash: hashA, Project: Project{ID: "pa", Slug: "project-a"}},
		pubB: {SecretHash: hashB, Project: Project{ID: "pb", Slug: "project-b"}},
	})
	p, err := r.ResolveCDK(context.Background(), cdkA)
	if err != nil || p.ID != "pa" {
		t.Fatalf("cdkA must resolve to project A only, got %+v err=%v", p, err)
	}
	// 正确public-id+错误secret也必须失败
	if _, err := r.ResolveCDK(context.Background(), "ccw_"+pubA+".wrongsecret"); err != ErrInvalidCDK {
		t.Fatalf("wrong secret must return ErrInvalidCDK, got %v", err)
	}
	if _, err := r.ResolveCDK(context.Background(), "ccw_unknown.zzz"); err != ErrInvalidCDK {
		t.Fatalf("unknown cdk must return ErrInvalidCDK, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/auth ./internal/project -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

```bash
go get golang.org/x/crypto@v0.31.0 github.com/jackc/pgx/v5@v5.7.1 github.com/google/uuid@v1.6.0
```

（版本为编写本计划时的已验证稳定版；如执行时需调整，先更新本行与`deploy/versions.lock`再安装，禁止`@latest`。）

`internal/auth/cdk.go`：

```go
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

// Entry：以public-id为键的CDK记录（与数据库行同构）。
type Entry struct {
	SecretHash string
	Project    Project
}

// MemoryResolver：单元测试与后续HTTP测试共用；生产实现在store包。
// 与生产实现一致：先按public-id做O(1)查找，再验证secret哈希。
type MemoryResolver struct{ byPublicID map[string]Entry }

func NewMemoryResolver(byPublicID map[string]Entry) *MemoryResolver {
	return &MemoryResolver{byPublicID: byPublicID}
}

func (r *MemoryResolver) ResolveCDK(_ context.Context, plain string) (Project, error) {
	pub, secret, err := auth.SplitCDK(plain)
	if err != nil {
		return Project{}, ErrInvalidCDK
	}
	e, ok := r.byPublicID[pub]
	if !ok || !auth.VerifySecret(secret, e.SecretHash) {
		return Project{}, ErrInvalidCDK
	}
	return e.Project, nil
}
```

`internal/store/migrations/001_initial.sql`（spec第4节的表结构，此处完整落盘；`schema_migrations`由Migrate自身维护）：

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
  name TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
  public_id TEXT NOT NULL UNIQUE,
  secret_hash TEXT NOT NULL,
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
  source_event_id TEXT NOT NULL,
  UNIQUE (project_id, source_event_id)
);
CREATE INDEX usage_events_window ON usage_events (project_id, occurred_at);

CREATE TABLE file_index (
  project_id UUID NOT NULL REFERENCES projects(id),
  path TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  sha256 TEXT NOT NULL,
  server_revision BIGINT NOT NULL,
  deleted BOOLEAN NOT NULL DEFAULT FALSE,
  updated_by_device TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, path)
);

CREATE TABLE sessions (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  tmux_name TEXT NOT NULL,
  connected_at TIMESTAMPTZ,
  last_seen_at TIMESTAMPTZ NOT NULL,
  state TEXT NOT NULL,
  UNIQUE (project_id, tmux_name)
);

CREATE TABLE usage_offsets (
  project_id UUID NOT NULL REFERENCES projects(id),
  file_identity TEXT NOT NULL,
  path TEXT NOT NULL,
  committed_offset BIGINT NOT NULL DEFAULT 0,
  partial_line TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, file_identity)
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
var migrationsFS embed.FS // migration唯一源就在本包的migrations/目录，禁止在仓库其他位置复制第二份

type Store struct{ Pool *pgxpool.Pool }

// New连接数据库并立即Ping；失败返回错误，调用方（main）必须以非零码退出。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Migrate：schema_migrations记录已执行迁移，每个迁移文件只执行一次。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
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
		var done bool
		if err := s.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, n).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + n)
		if err != nil {
			return err
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("store: migrate %s: %w", n, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, n); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ResolveCDK实现project.Resolver：按public-id做O(1)查询，再验证secret哈希。
func (s *Store) ResolveCDK(ctx context.Context, plain string) (project.Project, error) {
	pub, secret, err := auth.SplitCDK(plain)
	if err != nil {
		return project.Project{}, project.ErrInvalidCDK
	}
	var hash string
	var p project.Project
	err = s.Pool.QueryRow(ctx, `
		SELECT c.secret_hash, p.id, p.account_id, p.slug, p.container_name,
		       p.disk_limit_bytes, p.five_hour_limit, p.seven_day_limit
		FROM cdks c JOIN projects p ON p.id = c.project_id
		WHERE c.public_id = $1
		  AND c.disabled_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > now())`, pub).
		Scan(&hash, &p.ID, &p.AccountID, &p.Slug, &p.ContainerName,
			&p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit)
	if err != nil || !auth.VerifySecret(secret, hash) {
		return project.Project{}, project.ErrInvalidCDK
	}
	return p, nil
}

// GetProjectByID：session claims里的project ID一律经此查库（control-api不保存进程内会话状态）。
func (s *Store) GetProjectByID(ctx context.Context, id string) (project.Project, error) {
	var p project.Project
	err := s.Pool.QueryRow(ctx, `
		SELECT id, account_id, slug, container_name, disk_limit_bytes, five_hour_limit, seven_day_limit
		FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.AccountID, &p.Slug, &p.ContainerName, &p.DiskLimit, &p.FiveHourLimit, &p.SevenDayLimit)
	if err != nil {
		return project.Project{}, fmt.Errorf("store: project %s not found", id)
	}
	return p, nil
}
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
  - TTL约定（由签发方传入）：AudSession=15分钟；AudTerminal/AudSync=**2分钟短期连接令牌**——无状态HMAC无法保证单次使用，故不声称单用途；有效期内允许重连，worker每次接入实时复查额度（审查§2.3方案1）；令牌只经`Authorization`头或首个认证帧传递，禁止URL查询参数

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
  - **容器PID 1不是tmux**（审计§4.1：无TTY会立即退出）：容器命令固定为`sleep infinity`；tmux会话由worker-agent在容器运行后经`docker exec`准备（见Task 5的`EnsureSessionCmds`）

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
	// PID 1必须是sleep infinity而不是tmux（无TTY下tmux前台会立即退出）
	if got := strings.Join(c.Cmd, " "); got != "sleep infinity" {
		t.Fatalf("pid1 must be sleep infinity, got %q", got)
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
		// PID 1不能是tmux（无TTY立即退出）；tmux由worker经docker exec准备（Task 5）
		Cmd: []string{"sleep", "infinity"},
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
  - `terminal.EnsureSessionCmds(containerName, projectID string) [][]string`——审计§4.1流程：先`has-session -t main`，不存在则`new-session -d -s main -c /workspace claude`（都经`docker exec`）
  - `terminal.AttachCmd(containerName, projectID string) []string`——`["docker","exec","-it",container,"tmux","-L",projectID,"attach-session","-t","main"]`（**必须`-it`**：`-t`给容器内分配TTY，宿主机侧TTY由creack/pty提供；真实容器附着/断开/重连集成测试在Task 12覆盖）
  - `terminal.Serve(w http.ResponseWriter, r *http.Request, key []byte, start func(projectID string) (io.ReadWriteCloser, error))`：WebSocket升级+字节转发+resize控制消息`{"type":"resize","rows":N,"cols":N}`；令牌从`Authorization: Bearer`头读取（2分钟，AudTerminal），禁止URL查询参数；设置最大消息大小、读写deadline、ping/pong与连接数上限
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
	// 审查§2.1：必须为容器内分配TTY，否则tmux attach失败
	if !strings.Contains(joined, "-it") {
		t.Fatalf("attach must allocate a container tty (-it): %q", joined)
	}
}

func TestEnsureSessionCmdsOrder(t *testing.T) {
	cmds := EnsureSessionCmds("ccw-project-a", "pid-1")
	if len(cmds) != 2 {
		t.Fatalf("want has-session then new-session, got %d cmds", len(cmds))
	}
	if !strings.Contains(strings.Join(cmds[0], " "), "has-session") {
		t.Fatalf("first cmd must probe session: %v", cmds[0])
	}
	joined := strings.Join(cmds[1], " ")
	if !strings.Contains(joined, "new-session -d") || strings.Contains(joined, "-A") {
		t.Fatalf("second cmd must create detached session without -A: %v", cmds[1])
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
go get github.com/gorilla/websocket@v1.5.3 github.com/creack/pty@v1.1.24
```

`internal/terminal/session.go`：

```go
package terminal

func Names(projectID string) (socket, session string) {
	return projectID, "main"
}

// EnsureSessionCmds：附着前必须依次执行的命令（第一条失败时执行第二条）。
// 不使用new-session -A的前台形式：PID 1不是tmux，会话一律detached创建。
func EnsureSessionCmds(containerName, projectID string) [][]string {
	return [][]string{
		{"docker", "exec", containerName, "tmux", "-L", projectID, "has-session", "-t", "main"},
		{"docker", "exec", containerName, "tmux", "-L", projectID, "new-session", "-d", "-s", "main", "-c", "/workspace", "claude"},
	}
}

// AttachCmd必须带-t（审查§2.1）：容器内不分配TTY时tmux attach会直接失败；
// 宿主机侧的TTY由creack/pty提供给docker CLI进程，两者缺一不可。
func AttachCmd(containerName, projectID string) []string {
	return []string{"docker", "exec", "-it", containerName,
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
	"strings"
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
	// 令牌只从Authorization头读取（2分钟短期令牌，可重连）；URL查询参数会进代理日志，禁止使用
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := token.Verify(key, raw, token.AudTerminal, time.Now())
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
			container := containerFor(projectID)
			// 附着前先准备会话：has-session失败才new-session -d（审计§4.1）
			cmds := terminal.EnsureSessionCmds(container, projectID)
			if err := exec.Command(cmds[0][0], cmds[0][1:]...).Run(); err != nil {
				if err := exec.Command(cmds[1][0], cmds[1][1:]...).Run(); err != nil {
					return nil, err
				}
			}
			args := terminal.AttachCmd(container, projectID)
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
  - `sync.FileEntry{Path string; Size int64; SHA256 string; Revision int64; Deleted bool}`——**服务端**清单条目，`Revision`即`server_revision`
  - `sync.LocalEntry{Path string; Size int64; BaseRevision int64; BaseSHA256 string; CurrentSHA256 string; State LocalState}`与`type LocalState string`（`StateClean`/`StateModified`/`StateDeleted`）——**本地**索引条目，保存三方判断所需基线（审查§2.4）；`sync.BuildLocal(scanned []FileEntry, base []LocalEntry) []LocalEntry`由当前扫描结果+上次基线推导State
  - `sync.Diff(local []LocalEntry, remote []FileEntry) Plan`；`Plan{Upload []LocalEntry; Download []FileEntry; Conflicts []Conflict; DeleteToRemote []LocalEntry; DeleteToLocal []FileEntry}`；`Conflict{Path, LocalSHA, RemoteSHA string}`。**三方规则（不再是"revision大者胜"）：**clean+服务端已变→下载；modified+服务端未变→CAS上传；modified+服务端已变→冲突副本；deleted+服务端未变→CAS删除；deleted+服务端已变→保留服务端版本并提示冲突
  - `sync.SafeRelPath(p string) (string, error)`——校验并规范为forward-slash相对路径；错误`ErrUnsafePath`
  - `sync.DefaultExcluded(path string) bool`——`.env`、`.cclaude/`、`.ssh/`、`.aws/`、`.claude/`前缀
  - `sync.ConflictName(path, device string, at time.Time) string`——`<path>.conflict-<device>-<20060102T150405Z>`；远端副本落地时device恒为`remote`（审计§6.4的`.conflict-remote-<UTC>`）
  - **权威裁决在服务端（审计§6）：**客户端`Diff`只计算候选传输集；冲突以Task 12协议中`put`携带的`base_revision`与服务端`server_revision`的CAS比较为准，不得静默用"revision更大一端"覆盖。`DirStore.Manifest`仅用于灾后重建校验，正式Manifest来自`file_index`（含未过保留期tombstone）
  - `sync.Store`接口（服务端落盘）：`WriteTemp(path string, r io.Reader, maxBytes int64) (tmpID, sha string, size int64, err error)`（超限返回`ErrTooLarge`，用"上限+1字节"的LimitReader判定，不信任声明大小）；`Promote(path, tmpID string, revision int64) error`；`Discard(tmpID string)`（任何失败路径删除临时文件并释放空间预留）；`Delete(path string, revision int64) error`；`Manifest() ([]FileEntry, error)`
  - `sync.NewDirStore(root string) Store`——临时文件**随机命名+`O_EXCL`独占创建**，SHA校验+原子rename。**安全边界（审查§2.5）：**Linux生产实现必须用`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`（`golang.org/x/sys/unix.Openat2`，构建标签`//go:build linux`）解析父目录与目标；`EvalSymlinks`版本仅作非Linux平台测试替代，不得作为生产边界——`MkdirAll`与写入之间的符号链接替换是真实TOCTOU竞态，测试须覆盖"检查后替换符号链接"场景

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

func srv(path, sha string, rev int64, del bool) FileEntry {
	return FileEntry{Path: path, SHA256: sha, Revision: rev, Deleted: del, Size: 10}
}

func loc(path string, baseRev int64, baseSHA, curSHA string, st LocalState) LocalEntry {
	return LocalEntry{Path: path, BaseRevision: baseRev, BaseSHA256: baseSHA, CurrentSHA256: curSHA, State: st, Size: 10}
}

func TestDiffCleanFollowsServer(t *testing.T) {
	// 本地未修改（current==base），服务端已前进 → 下载，绝不上传
	local := []LocalEntry{loc("a.go", 1, "s1", "s1", StateClean)}
	remote := []FileEntry{srv("a.go", "s9", 5, false)}
	p := Diff(local, remote)
	if len(p.Download) != 1 || p.Download[0].SHA256 != "s9" || len(p.Upload)+len(p.Conflicts) != 0 {
		t.Fatalf("clean local must follow server: %+v", p)
	}
}

func TestDiffModifiedOnCurrentBaseUploads(t *testing.T) {
	// 本地已修改，服务端仍在同一基线 → CAS上传
	local := []LocalEntry{loc("a.go", 3, "s1", "s2", StateModified), loc("new.go", 0, "", "n1", StateModified)}
	remote := []FileEntry{srv("a.go", "s1", 3, false)}
	p := Diff(local, remote)
	if len(p.Upload) != 2 || len(p.Conflicts) != 0 {
		t.Fatalf("want 2 uploads no conflicts, got %+v", p)
	}
}

func TestDiffBothModifiedIsConflict(t *testing.T) {
	// 本地基于rev2修改，服务端已到rev4且内容不同 → 冲突，禁止任何静默传输
	local := []LocalEntry{loc("a.go", 2, "s1", "local-sha", StateModified)}
	remote := []FileEntry{srv("a.go", "remote-sha", 4, false)}
	p := Diff(local, remote)
	if len(p.Conflicts) != 1 || p.Conflicts[0].Path != "a.go" {
		t.Fatalf("want conflict on a.go, got %+v", p)
	}
	if len(p.Upload)+len(p.Download) != 0 {
		t.Fatal("conflict must not silently transfer")
	}
}

func TestDiffStaleLocalIsNotAWin(t *testing.T) {
	// 关键回归（审查§2.4）：本地是未修改的旧版本，不能因为"看起来不同"而上传覆盖服务端
	local := []LocalEntry{loc("a.go", 2, "s-old", "s-old", StateClean)}
	remote := []FileEntry{srv("a.go", "s-new", 7, false)}
	p := Diff(local, remote)
	if len(p.Upload) != 0 || len(p.Download) != 1 {
		t.Fatalf("stale clean copy must download, never upload: %+v", p)
	}
}

func TestDiffDelete(t *testing.T) {
	local := []LocalEntry{loc("gone.go", 3, "s", "", StateDeleted)}
	remote := []FileEntry{srv("gone.go", "s", 3, false)}
	p := Diff(local, remote)
	if len(p.DeleteToRemote) != 1 {
		t.Fatalf("deletion on current base must propagate: %+v", p)
	}
	// 删除遇到服务端新版本 → 冲突（保留服务端）
	remote2 := []FileEntry{srv("gone.go", "s-new", 5, false)}
	p2 := Diff(local, remote2)
	if len(p2.DeleteToRemote) != 0 || len(p2.Conflicts) != 1 {
		t.Fatalf("delete vs newer server must conflict: %+v", p2)
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
	tmpID, sha, size, err := s.WriteTemp("src/main.go", strings.NewReader("package main\n"), 1<<20)
	if err != nil || size != 13 {
		t.Fatalf("write: sha=%s size=%d err=%v", sha, size, err)
	}
	// promote前目录里只有tmp文件，不可见于清单
	m, _ := s.Manifest()
	if len(m) != 0 {
		t.Fatalf("tmp file must not appear in manifest: %+v", m)
	}
	if err := s.Promote("src/main.go", tmpID, 1); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Manifest()
	if len(m) != 1 || m[0].Path != "src/main.go" || m[0].SHA256 != sha {
		t.Fatalf("manifest wrong: %+v", m)
	}
}

func TestDirStoreTooLarge(t *testing.T) {
	// 审查§2.5/§15.3：真实字节上限用"上限+1"判定，超限必须失败且tmp被清理
	root := t.TempDir()
	s := NewDirStore(root)
	if _, _, _, err := s.WriteTemp("big.bin", strings.NewReader("0123456789"), 9); err != ErrTooLarge {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("failed write must leave no tmp files: %v", entries)
	}
}

func TestDirStoreRejectsEscape(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	if _, _, _, err := s.WriteTemp("../evil", strings.NewReader("x"), 8); err == nil {
		t.Fatal("path escape must be rejected")
	}
	// 符号链接逃逸：workspace内建一个指向外部的链接目录
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "link"))
	if _, _, _, err := s.WriteTemp("link/evil.txt", strings.NewReader("x"), 8); err == nil {
		t.Fatal("symlink escape must be rejected")
	}
}

func TestDirStoreDeleteTombstone(t *testing.T) {
	root := t.TempDir()
	s := NewDirStore(root)
	tmpID, _, _, _ := s.WriteTemp("a.txt", strings.NewReader("hello"), 8)
	s.Promote("a.txt", tmpID, 1)
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
	Revision int64  `json:"revision"` // server_revision，只由服务端分配
	Deleted  bool   `json:"deleted"`
}

type LocalState string

const (
	StateClean    LocalState = "clean"
	StateModified LocalState = "modified"
	StateDeleted  LocalState = "deleted"
)

// LocalEntry保存三方判断的基线（审查§2.4）：只有同时知道
// "上次确认的服务端版本"和"当前本地内容"，才能区分旧副本与新修改。
type LocalEntry struct {
	Path          string     `json:"path"`
	Size          int64      `json:"size"`
	BaseRevision  int64      `json:"base_revision"`
	BaseSHA256    string     `json:"base_sha256"`
	CurrentSHA256 string     `json:"current_sha256"`
	State         LocalState `json:"state"`
}

type Conflict struct{ Path, LocalSHA, RemoteSHA string }

type Plan struct {
	Upload         []LocalEntry
	Download       []FileEntry
	Conflicts      []Conflict
	DeleteToRemote []LocalEntry
	DeleteToLocal  []FileEntry
}

// Diff三方规则（服务端CAS是最终裁决，这里只产生候选集）：
//   clean    + 服务端已变 → 下载（本地旧副本永远不是"赢家"）；
//   modified + 服务端未变 → CAS上传；
//   modified + 服务端已变 → 冲突副本；
//   deleted  + 服务端未变 → CAS删除；
//   deleted  + 服务端已变 → 冲突（保留服务端版本）。
func Diff(local []LocalEntry, remote []FileEntry) Plan {
	ri := make(map[string]FileEntry, len(remote))
	for _, r := range remote {
		ri[r.Path] = r
	}
	seen := make(map[string]bool, len(local))
	var p Plan
	for _, l := range local {
		seen[l.Path] = true
		r, ok := ri[l.Path]
		serverAdvanced := ok && (r.Revision != l.BaseRevision || r.Deleted)
		switch l.State {
		case StateClean:
			if serverAdvanced {
				if r.Deleted {
					p.DeleteToLocal = append(p.DeleteToLocal, r)
				} else {
					p.Download = append(p.Download, r)
				}
			}
		case StateModified:
			switch {
			case !ok || !serverAdvanced:
				p.Upload = append(p.Upload, l)
			case r.SHA256 == l.CurrentSHA256:
				// 内容碰巧一致：只需更新基线，无需传输
			default:
				p.Conflicts = append(p.Conflicts, Conflict{Path: l.Path, LocalSHA: l.CurrentSHA256, RemoteSHA: r.SHA256})
			}
		case StateDeleted:
			if !ok || !serverAdvanced {
				p.DeleteToRemote = append(p.DeleteToRemote, l)
			} else if !r.Deleted {
				p.Conflicts = append(p.Conflicts, Conflict{Path: l.Path, LocalSHA: "", RemoteSHA: r.SHA256})
			}
		}
	}
	for path, r := range ri {
		if !seen[path] && !r.Deleted {
			p.Download = append(p.Download, r)
		}
	}
	return p
}

// BuildLocal由当前目录扫描结果与上次保存的基线推导每条路径的State。
func BuildLocal(scanned []FileEntry, base []LocalEntry) []LocalEntry {
	bi := make(map[string]LocalEntry, len(base))
	for _, b := range base {
		bi[b.Path] = b
	}
	seen := make(map[string]bool, len(scanned))
	var out []LocalEntry
	for _, s := range scanned {
		seen[s.Path] = true
		b, ok := bi[s.Path]
		e := LocalEntry{Path: s.Path, Size: s.Size, CurrentSHA256: s.SHA256}
		if ok {
			e.BaseRevision, e.BaseSHA256 = b.BaseRevision, b.BaseSHA256
		}
		if ok && s.SHA256 == b.BaseSHA256 {
			e.State = StateClean
		} else {
			e.State = StateModified // 新文件的BaseRevision=0也走CAS上传
		}
		out = append(out, e)
	}
	for _, b := range base {
		if !seen[b.Path] && b.State != StateDeleted {
			out = append(out, LocalEntry{Path: b.Path, BaseRevision: b.BaseRevision,
				BaseSHA256: b.BaseSHA256, State: StateDeleted})
		}
	}
	return out
}

func ConflictName(path, device string, at time.Time) string {
	return fmt.Sprintf("%s.conflict-%s-%s", path, device, at.UTC().Format("20060102T150405Z"))
}
```

`internal/sync/server.go`（DirStore；WebSocket端点在Task 7接好配额后一并挂到worker-agent）：

```go
package sync

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrTooLarge = errors.New("sync: content exceeds size limit")

type Store interface {
	WriteTemp(path string, r io.Reader, maxBytes int64) (tmpID, sha string, size int64, err error)
	Promote(path, tmpID string, revision int64) error
	Discard(tmpID string)
	Delete(path string, revision int64) error
	Manifest() ([]FileEntry, error)
}

type DirStore struct{ root string }

func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// resolve把相对路径映射到root下。
//
// 安全边界（审查§2.5）：本函数的EvalSymlinks实现存在检查与写入之间的
// TOCTOU窗口，只允许用于非Linux平台的测试。Linux生产构建必须提供
// resolve的openat2版本（//go:build linux文件中用unix.Openat2带
// RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS逐级打开父目录并在fd上操作），
// 并有"检查后替换符号链接"的竞态测试证明其不可绕过。
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

// WriteTemp：随机命名+O_EXCL独占创建；LimitReader读"上限+1"字节判定超限；
// 任何失败路径都删除临时文件（审查§2.5）。
func (d *DirStore) WriteTemp(rel string, r io.Reader, maxBytes int64) (string, string, int64, error) {
	abs, err := d.resolve(rel)
	if err != nil {
		return "", "", 0, err
	}
	rnd := make([]byte, 8)
	if _, err := rand.Read(rnd); err != nil {
		return "", "", 0, err
	}
	tmpID := fmt.Sprintf(".cclaude.tmp.%s", hex.EncodeToString(rnd))
	tmpPath := filepath.Join(filepath.Dir(abs), tmpID)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
	f.Close()
	if err != nil || n > maxBytes {
		os.Remove(tmpPath)
		if err == nil {
			err = ErrTooLarge
		}
		return "", "", 0, err
	}
	return tmpID, hex.EncodeToString(h.Sum(nil)), n, nil
}

func (d *DirStore) Promote(rel, tmpID string, rev int64) error {
	abs, err := d.resolve(rel)
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(filepath.Dir(abs), tmpID), abs)
}

func (d *DirStore) Discard(tmpID string) {
	// tmpID不含路径分隔符，只可能位于root子树内；遍历删除同名tmp
	filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && filepath.Base(p) == tmpID {
			os.Remove(p)
		}
		return nil
	})
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
  - `usage.OffsetStore`接口：`Load(ctx, projectID, fileIdentity string) (offset int64, partial string, err error)`；`Save(ctx, projectID, fileIdentity, path string, offset int64, partial string) error`——生产实现写`usage_offsets`表（审计§8.2：worker重启从持久offset恢复；找不到则从头重扫靠幂等去重）
  - `usage.Collector{Dir string; ProjectID string; Sink Sink; Weights Weights; Offsets OffsetStore}`：`Scan(ctx) error`——只在读到**完整换行**后推进offset；末尾半行存`partial_line`下轮拼接；文件截断/轮转时按file identity（inode+首行哈希）重识别；Scanner错误与超长行记指标不静默丢弃
  - 数据来源：worker以**只读方式挂载**各项目claude卷（或受控docker exec读取），不依赖`/var/lib/docker/volumes/.../_data`内部布局（审计§8.1）
  - 测试样例：`testdata/session-sample.jsonl`以Task 0产出的**脱敏真实样例**为准（含requestId多条记录场景）；下方Step 1给出的是最小结构示例，Task 0完成后必须替换并按确认的最终计量语义调整断言

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

func TestPartialLineNotLostAcrossScans(t *testing.T) {
	// 审查§2.7.3/§15.4：末尾半行不解析、不丢失，补全后被准确采集
	dir := t.TempDir()
	os.MkdirAll(dir+"/projects/x", 0o755)
	path := dir + "/projects/x/s.jsonl"
	full := `{"type":"assistant","requestId":"req_E","timestamp":"2026-07-19T09:02:00.000Z","message":{"model":"m","usage":{"input_tokens":3,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	os.WriteFile(path, []byte(full[:60]), 0o644) // 只写前60字节，无换行
	sink := &memSink{}
	c := &Collector{Dir: dir, ProjectID: "pa", Sink: sink}
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.ids["req_E"] {
		t.Fatal("half line must not be parsed yet")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(full[60:])
	f.Close()
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sink.ids["req_E"] {
		t.Fatal("completed line must be collected exactly once")
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

// parseLine解析单行。返回(event, isEvent, isBad)：非用量行两者皆false。
func parseLine(line string) (Event, bool, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Event{}, false, false
	}
	var rl rawLine
	if err := json.Unmarshal([]byte(line), &rl); err != nil {
		return Event{}, false, true
	}
	if rl.Type != "assistant" || rl.Message.Usage == nil || rl.RequestID == "" {
		return Event{}, false, false // 非用量行，不算坏行
	}
	ts, err := time.Parse(time.RFC3339Nano, rl.Timestamp)
	if err != nil {
		return Event{}, false, true
	}
	u := rl.Message.Usage
	return Event{
		SourceEventID: rl.RequestID, OccurredAt: ts.UTC(), Model: rl.Message.Model,
		Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
	}, true, false
}

func ParseLines(r io.Reader) ([]Event, int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20) // 长行容忍到8MB
	var out []Event
	bad := 0
	for sc.Scan() {
		if e, ok, isBad := parseLine(sc.Text()); ok {
			out = append(out, e)
		} else if isBad {
			bad++
		}
	}
	return out, bad
}

type Weights struct{ Input, Output, CacheRead, CacheWrite int64 }

func Weighted(e Event, w Weights) int64 {
	return e.Input*w.Input + e.Output*w.Output + e.CacheRead*w.CacheRead + e.CacheWrite*w.CacheWrite
}

type Sink interface {
	// Insert幂等键为(projectID, e.SourceEventID)；同一requestId再次出现时
	// 必须按Task 0确认的语义更新为最终记录/各字段最大值，不能简单保留第一条。
	Insert(ctx context.Context, projectID string, e Event, weighted int64) error
}

// OffsetStore持久化每文件读取游标（审查§2.7）；生产实现写usage_offsets表。
type OffsetStore interface {
	Load(ctx context.Context, projectID, fileIdentity string) (offset int64, partial string, err error)
	Save(ctx context.Context, projectID, fileIdentity, path string, offset int64, partial string) error
}

type fileCursor struct {
	offset  int64  // committed_offset：最后一个完整行末尾的位置
	partial string // 已读到但未见换行的尾部半行
}

type Collector struct {
	Dir       string
	ProjectID string
	Sink      Sink
	Weights   Weights
	Offsets   OffsetStore // 生产必须注入；为nil时退化为进程内存游标（仅单元测试）
	mem       map[string]fileCursor
	BadLines  int64 // 指标：坏行/超长行/读取错误累计，由worker暴露，不静默丢弃
}

// fileIdentity在Linux上取dev:inode（处理轮转/重建）；退化实现取路径。
// 生产版在collector_linux.go以syscall.Stat_t实现，此处默认用路径。
func fileIdentity(path string, fi os.FileInfo) string { return path }

func (c *Collector) load(ctx context.Context, id string) (fileCursor, error) {
	if c.Offsets == nil {
		if c.mem == nil {
			c.mem = map[string]fileCursor{}
		}
		return c.mem[id], nil
	}
	off, partial, err := c.Offsets.Load(ctx, c.ProjectID, id)
	return fileCursor{offset: off, partial: partial}, err
}

func (c *Collector) save(ctx context.Context, id, path string, cur fileCursor) error {
	if c.Offsets == nil {
		c.mem[id] = cur
		return nil
	}
	return c.Offsets.Save(ctx, c.ProjectID, id, path, cur.offset, cur.partial)
}

func (c *Collector) Scan(ctx context.Context) error {
	return filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil // 单文件失败不中断整体采集
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil
		}
		id := fileIdentity(p, fi)
		cur, err := c.load(ctx, id)
		if err != nil {
			return nil // 游标读不到：跳过本轮，不前进
		}
		resume := cur.offset + int64(len(cur.partial))
		if fi.Size() < resume {
			cur = fileCursor{} // 截断/轮转：从头重扫，幂等写入兜底
			resume = 0
		}
		if _, err := f.Seek(resume, io.SeekStart); err != nil {
			return nil
		}
		br := bufio.NewReaderSize(f, 64<<10)
		for {
			chunk, rerr := br.ReadString('\n')
			if rerr == nil {
				line := cur.partial + chunk
				cur.partial = ""
				if e, ok, isBad := parseLine(line); ok {
					if err := c.Sink.Insert(ctx, c.ProjectID, e, Weighted(e, c.Weights)); err != nil {
						return err // Sink失败：游标不保存，下轮重试
					}
				} else if isBad {
					c.BadLines++
				}
				cur.offset += int64(len(line)) // 只在完整行后推进committed_offset
				continue
			}
			if rerr == io.EOF {
				cur.partial += chunk // 半行暂存，下一轮补全
				break
			}
			c.BadLines++ // 读取错误：记指标，游标停在最后完整行
			break
		}
		c.save(ctx, id, p, cur)
		return nil
	})
}
```

生产Sink（`store`包追加方法，审查§2.7.7）：

```sql
INSERT INTO usage_events (project_id, occurred_at, model, input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens, weighted_units, source_event_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (project_id, source_event_id) DO UPDATE SET
  input_tokens      = GREATEST(usage_events.input_tokens, EXCLUDED.input_tokens),
  output_tokens     = GREATEST(usage_events.output_tokens, EXCLUDED.output_tokens),
  cache_read_tokens = GREATEST(usage_events.cache_read_tokens, EXCLUDED.cache_read_tokens),
  cache_write_tokens= GREATEST(usage_events.cache_write_tokens, EXCLUDED.cache_write_tokens),
  weighted_units    = GREATEST(usage_events.weighted_units, EXCLUDED.weighted_units);
```

（"各字段最大值"是默认语义；若Task 0确认应取"最终记录"，改为按`occurred_at`较新者覆盖——二选一以Task 0结论为准，禁止简单保留第一条。）生产OffsetStore：`usage_offsets`表的`SELECT/UPSERT`。默认权重（可由环境变量覆盖，比例参考官方计费）：`Input=10, Output=50, CacheRead=1, CacheWrite=12`。

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
  - `quota.Limits{FiveHour, SevenDay, PoolFiveHour, PoolSevenDay, Reserve, SafetyMargin int64}`——池保护**同时**计算5小时与7天两个窗口（审计§9.3）
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
	d, err := s.Check(context.Background(), "pa", "acc", Limits{FiveHour: 1000, SevenDay: 1000, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}, now)
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
	l := Limits{FiveHour: 1000, SevenDay: 100000, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}
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
	l := Limits{FiveHour: 100000, SevenDay: 100000, PoolFiveHour: 10000, PoolSevenDay: 1 << 40, Reserve: 100, SafetyMargin: 200}
	// 5小时池剩余=10000-9800=200，不大于Reserve+SafetyMargin=300 → 双双拒绝
	for _, pid := range []string{"pa", "pb"} {
		d, _ := s.Check(context.Background(), pid, "acc", l, now)
		if !d.Over || d.Reason != "pool_exhausted" {
			t.Fatalf("%s must be pool_exhausted: %+v", pid, d)
		}
	}
	// 7天池同样受保护：5小时充裕但7天耗尽时也必须拒绝
	l2 := Limits{FiveHour: 100000, SevenDay: 100000, PoolFiveHour: 1 << 40, PoolSevenDay: 10000, Reserve: 100, SafetyMargin: 200}
	d, _ := s.Check(context.Background(), "pa", "acc", l2, now)
	if !d.Over || d.Reason != "pool_exhausted" {
		t.Fatalf("7d pool must also be protected: %+v", d)
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
	FiveHour, SevenDay                        int64
	PoolFiveHour, PoolSevenDay                int64
	Reserve, SafetyMargin                     int64
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
		// 池保护同时看5小时与7天窗口（审计§9.3：只看5小时不足以保护周额度）
		pool5h, err := s.Reader.PoolUsed(ctx, accountID, now.Add(-5*time.Hour))
		if err != nil {
			return d, err
		}
		pool7d, err := s.Reader.PoolUsed(ctx, accountID, now.Add(-7*24*time.Hour))
		if err != nil {
			return d, err
		}
		if l.PoolFiveHour-pool5h <= l.Reserve+l.SafetyMargin ||
			l.PoolSevenDay-pool7d <= l.Reserve+l.SafetyMargin {
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
  - `GET /v1/connection`（Header `Authorization: Bearer <session_token>`）→ `ConnectionResponse`（字段：project_id、project_slug、terminal_url、sync_url、terminal_token、sync_token、sync_mode、disk_used/limit、five_hour_used/limit、seven_day_used/limit、over、over_reason）；terminal/sync令牌TTL为**2分钟**；`over==true`时terminal_token为空串，但**sync_token仍签发**且`sync_mode="cleanup"`（只允许下载/删除/缩小，由Task 12的sync端点强制执行；审计§7）；正常时`sync_mode="rw"`
  - exchange端点按IP与public-id双维度做简单令牌桶限速（内存实现即可），超阈值返回429
  - `GET /usage`（会话令牌）→ SSR HTML，30秒`<meta http-equiv="refresh" content="30">`
  - `control.Server`结构：`New(resolver project.Resolver, getProject func(context.Context, string) (project.Project, error), key []byte, q quota.Service, idx storage.Index, limitsFor func(project.Project) quota.Limits, agentBase string) *Server`；`(*Server).Handler() http.Handler`。**无进程内会话状态**（审计§5.4）：session claims里的project ID一律经`getProject`查库（生产传`store.GetProjectByID`），control-api重启后未过期会话仍有效

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
	cdkA, pubA, _ := auth.NewCDK()
	_, secA, _ := auth.SplitCDK(cdkA)
	hashA, _ := auth.HashSecret(secA)
	pa := project.Project{ID: "pa", AccountID: "acc", Slug: "project-a", DiskLimit: 1000, FiveHourLimit: 100, SevenDayLimit: 1000}
	resolver := project.NewMemoryResolver(map[string]project.Entry{
		pubA: {SecretHash: hashA, Project: pa},
	})
	getProject := func(_ context.Context, id string) (project.Project, error) {
		if id == pa.ID {
			return pa, nil
		}
		return project.Project{}, project.ErrInvalidCDK
	}
	key := make([]byte, 32)
	q := quota.Service{Reader: fixedReader{perProject: map[string]int64{"pa": 10}}}
	s := New(resolver, getProject, key, q, storage.NewMemoryIndex(),
		func(p project.Project) quota.Limits {
			return quota.Limits{FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit, PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}
		}, "wss://ccw.example.com/ws")
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
	if !conn.Over || conn.OverReason != "five_hour_limit" || conn.TerminalToken != "" {
		t.Fatalf("over project must get no terminal token: %+v", conn)
	}
	// 超额仍必须能清理：sync token照发，但模式为cleanup（审计§7）
	if conn.SyncToken == "" || conn.SyncMode != "cleanup" {
		t.Fatalf("over project must still get cleanup-mode sync token: %+v", conn)
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
	"context"
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
	SyncMode      string `json:"sync_mode"` // "rw"或"cleanup"（超额/磁盘满时只许下载、删除、缩小）
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
	Resolver   project.Resolver
	GetProject func(context.Context, string) (project.Project, error) // 一律查库，无进程内会话状态（审计§5.4）
	Key        []byte
	Quota      quota.Service
	Index      storage.Index
	LimitsFor  func(project.Project) quota.Limits
	AgentBase  string
}

func New(r project.Resolver, getProject func(context.Context, string) (project.Project, error),
	key []byte, q quota.Service, idx storage.Index,
	limitsFor func(project.Project) quota.Limits, agentBase string) *Server {
	return &Server{Resolver: r, GetProject: getProject, Key: key, Quota: q, Index: idx,
		LimitsFor: limitsFor, AgentBase: agentBase}
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
	p, err := s.GetProject(r.Context(), c.ProjectID)
	return p, err == nil
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
	// 超额/磁盘满：不发终端令牌，但sync降级为cleanup模式照发（仍能下载/删除/缩小，审计§7）
	resp.SyncMode = "rw"
	if resp.Over {
		resp.SyncMode = "cleanup"
	} else {
		resp.TerminalToken, _ = token.Mint(s.Key, p.ID, token.AudTerminal, 2*time.Minute, now)
	}
	resp.SyncToken, _ = token.Mint(s.Key, p.ID, token.AudSync, 2*time.Minute, now)
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

`cmd/control-api/main.go`改为：加载配置→`store.New`+`Migrate`（连接或Ping失败打印错误并`os.Exit(1)`）→用store实现的Resolver/UsageReader/PGIndex与`store.GetProjectByID`组装`control.New(...)`→用`http.Server{ReadTimeout: 10s, ReadHeaderTimeout: 5s, WriteTimeout: 30s, IdleTimeout: 60s}`启动并检查`ListenAndServe`返回错误（不得忽略）。**只监听内网/localhost**，公网由Caddy/Nginx的443反代（Task 12的Caddyfile）。exchange端点套IP+public-id令牌桶限速中间件。管理端（建项目/发CDK）以`net.Listen("unix", cfg.AdminSocketPath)`挂独立mux：`POST /admin/projects`、`POST /admin/cdks`（返回一次性明文），socket权限`0660`并校验调用者；不注册到公网Handler。门户`/usage`认证定案（审查§3.3，不再二选一）：**仅监听localhost，经SSH隧道访问**；`/portal/*`公网路由不启用；CDK会话仅能查自己项目的JSON接口，无管理员能力。

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
  - `sync.LocalIndex`：项目根`.cclaude/index.json`读写本地`[]LocalEntry`（含base_revision/base_sha256基线，审查§2.4）
  - `sync.ScanDir(root string) ([]FileEntry, error)`——当前目录状态扫描（排除`.cclaude/`与默认排除项）；与`LocalIndex`基线经`BuildLocal`合成`[]LocalEntry`后才能进`Diff`
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
	in := []LocalEntry{{Path: "a.go", Size: 3, BaseRevision: 2, BaseSHA256: "abc", CurrentSHA256: "abc", State: StateClean}}
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
	scanned, _ := ScanDir(root)
	sha := scanned[0].SHA256
	// 基线=服务端rev3同内容 → BuildLocal判定clean → Diff零传输
	base := []LocalEntry{{Path: "a.go", BaseRevision: 3, BaseSHA256: sha, State: StateClean}}
	local := BuildLocal(scanned, base)
	remote := []FileEntry{{Path: "a.go", SHA256: sha, Revision: 3, Size: 3}}
	p := Diff(local, remote)
	if len(p.Upload)+len(p.Download)+len(p.Conflicts) != 0 {
		t.Fatalf("unchanged files must not transfer: %+v", p)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/control ./internal/sync -v`
Expected: FAIL（未定义符号）

- [ ] **Step 3: 实现**

```bash
go get github.com/fsnotify/fsnotify@v1.8.0 golang.org/x/term@v0.27.0
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

func (l LocalIndex) Load() ([]LocalEntry, error) {
	b, err := os.ReadFile(l.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LocalEntry
	return out, json.Unmarshal(b, &out)
}

func (l LocalIndex) Save(es []LocalEntry) error {
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
	fmt.Printf("[%s] 5h:%d/%d 7d:%d/%d disk:%d/%d\n", conn.ProjectSlug,
		conn.FiveHourUsed, conn.FiveHourLimit, conn.SevenDayUsed, conn.SevenDayLimit,
		conn.DiskUsed, conn.DiskLimit)
	cwd, _ := os.Getwd()

	if conn.Over {
		// 审查§2.8：超额/磁盘满绝不直接退出——否则cleanup模式永远不可达。
		// 不开Claude终端；以cleanup模式建立同步（服务端只允许get/delete/缩小），
		// 周期性重新Connection探测窗口恢复，恢复后自动切回正常流程。
		fmt.Fprintf(os.Stderr, "项目受限（%s）：进入cleanup模式，可下载、删除、缩小文件；额度窗口恢复后自动回到正常模式。\n", conn.OverReason)
		runSyncLoop(ctx, cwd, conn) // 阻塞运行；内部每60秒重新Connection，Over解除即返回
		return
	}

	go runSyncLoop(ctx, cwd, conn)            // 首次全量+watcher增量，断线退避重连
	if err := runTerminal(ctx, conn); err != nil { // raw mode+WebSocket+resize；断线退避重连
		fmt.Fprintln(os.Stderr, err)
	}
}
```

`runSyncLoop`与`runTerminal`在同文件实现（实现规格，编码时测试先行补全）：`runTerminal`用`gorilla/websocket`的`Dialer.DialContext`连`conn.TerminalURL`，令牌放`http.Header{"Authorization": {"Bearer " + conn.TerminalToken}}`（**全代码库不得出现`?token=`**），`term.MakeRaw(stdin)`进raw mode（退出时恢复），goroutine双向拷贝，监听`SIGWINCH`（Windows上轮询`term.GetSize`每2秒）发resize Text帧；`runSyncLoop`调用`ScanDir`+`LocalIndex`+`BuildLocal`+`Diff`，通过同步WebSocket端点执行Plan（首帧发auth，上传带`base_revision`走CAS，收到`reject:conflict`时下载远端版本按`ConflictName(path,"remote",now)`落地并提示；ack后把该路径基线更新为新revision与新SHA），fsnotify事件进入500ms定时器去抖后重扫。两个循环断线后均以1s起倍增退避重连（上限30s），并用会话令牌重新`Connection`换新的2分钟连接令牌；session token过期（401）时用**内存中保留的CDK**自动重新`Exchange`（审计§5.3），CDK只存进程内存、不落盘不入日志。连接WebSocket时令牌放`Authorization`头。`sync_mode=="cleanup"`时CLI只执行下载/删除/缩小并提示用户当前为清理模式。

- [ ] **Step 4: 运行测试与三平台交叉编译**

Run: `go test ./internal/control ./internal/sync -v`
Expected: PASS

Run: `GOOS=windows GOARCH=amd64 go build ./cmd/cclaude && GOOS=darwin GOARCH=arm64 go build ./cmd/cclaude && GOOS=linux GOARCH=amd64 go build ./cmd/cclaude`
Expected: 三平台全部编译通过（Windows路径规则已由`SafeRelPath`反斜杠测试覆盖）

- [ ] **Step 4b: 三平台真实冒烟（审查§3.5：交叉编译不等于可用）**——在Windows、macOS、Linux三台真机（或Windows实机+macOS实机+Linux VPS）各执行一次：启动`cclaude`、输入CDK、完成一次同步、附着终端输入一条命令、断网30秒验证自动重连。每平台记录结果到`docs/phase1-evidence/cli-smoke-<os>.md`；任一平台失败视为本任务未完成。

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: add cross-platform remote workspace cli"
```

### Task 12: 同步端点接线、e2e验收与部署

**Files:**
- Create: `internal/sync/ws.go`（worker-agent侧同步WebSocket端点：验AudSync令牌→按消息调DirStore+PGIndex+Gate）
- Modify: `cmd/worker-agent/main.go`（挂`/v1/sync`路由、启动每30秒的usage.Collector循环、容器名改为store查询）
- Create: `tests/e2e/two_projects_test.go`
- Create: `deploy/Caddyfile`
- Create: `deploy/versions.lock`
- Create: `deploy/backup.sh`
- Create: `deploy/restore.sh`
- Create: `deploy/control-api.service`
- Create: `deploy/worker-agent.service`
- Create: `deploy/Dockerfile.claude`（含官方Claude Code、git、tmux、非root用户claude的项目容器镜像）
- Create: `docs/runbook.md`

**Interfaces:**
- Consumes: 前面全部任务的产出
- Produces: 可部署系统与e2e证据

同步WebSocket消息协议（JSON Text帧+文件内容Binary帧交替；连接建立后第一帧为认证帧，令牌不进URL）：

```json
{"op":"auth","token":"<sync-token>"}       → 验证AudSync（2分钟）；服务端回{"op":"auth_ok","mode":"rw|cleanup"}
{"op":"hello","project_id":"...","device":"laptop","cursor":N}
{"op":"manifest"}                          → 服务端回{"op":"manifest","entries":[FileEntry...]}——来自file_index，含未过保留期tombstone，不是磁盘扫描
{"op":"put","entry":FileEntry,"base_revision":N,"declared_size":M}
                                           → 紧随一个Binary帧为文件内容；服务端在项目级锁/事务中：CAS比较base_revision与当前server_revision→限制实际读取字节数（不信任declared_size）→写tmp并算真实SHA/大小→Gate.Allow预留→原子替换→revision+1→回{"op":"ack","path":"...","revision":N+1}；失败回{"op":"reject","path":"...","reason":"conflict|disk_full|sha_mismatch|unsafe_path|readonly_mode|too_large"}
{"op":"get","path":"..."}                  → 服务端回{"op":"file","entry":FileEntry}+Binary帧
{"op":"delete","entry":FileEntry,"base_revision":N} → CAS通过后写持久tombstone；回ack
```

收到`reject:conflict`时客户端下载远端版本存为`ConflictName(path, "remote", now)`并提示用户，不覆盖本地。`mode=="cleanup"`时服务端拒绝一切新增/扩大的`put`（`readonly_mode`），只允许get/delete/缩小。WebSocket设最大消息大小、读写deadline、ping/pong与连接数上限。

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
	t.Run("proxy_path_contract", testProxyPathContract) // spec第3节路径合同逐条打通：/api/v1/*→/v1/*，/ws/*→/v1/*（审查§2.2）
	t.Run("terminal_tty_attach", testTerminalTTYAttach) // 真实容器内tmux经-it附着/断开/重连（审查§2.1）
	t.Run("isolation_volumes", testVolumeIsolation)     // docker inspect：A容器挂载列表无B卷
	t.Run("isolation_cdk", testCDKIsolation)            // CDK-A取connection只能拿到A；伪造B的project_id被拒
	t.Run("sync_roundtrip", testSyncRoundtrip)          // 本地→云端→云端改文件→回本地
	t.Run("sync_conflict", testSyncConflict)            // 双端同改→conflict副本，无覆盖
	t.Run("disk_quota", testDiskQuota)                  // 超限上传被reject，删除仍允许，页面数值与SUM一致
	t.Run("terminal_reconnect", testTerminalReconnect)  // 断开WebSocket重连后capture-pane可见断开前标记
	t.Run("quota_a_over_b_free", testQuotaIsolation)    // 灌A的usage_events到超限：A拒连，B正常
	t.Run("quota_enforce_active_conn", testQuotaClosesActiveTerminals) // A超额时已连接终端被关闭输入（审计§9.3）
	t.Run("concurrent_uploads_quota", testConcurrentUploadsQuota) // 并发上传不能各读旧用量后同时突破限额（审计§15.2）
	t.Run("cleanup_mode_when_full", testCleanupModeStillWorks)    // 磁盘满/超额时仍能下载、删除、缩小（审计§15.9）
	t.Run("api_restart_keeps_sessions", testAPIRestartKeepsSessions) // 重启control-api后未过期会话仍可解析项目（审计§15.1）
	t.Run("cloud_edit_syncs_back", testCloudEditSyncsBack)        // 云端直接改/删文件→revision/tombstone→同步回本地（审计§15.10）
	t.Run("rebuild_keeps_volumes", testRebuildKeepsData) // docker rm A容器→EnsureProjectRuntime重建→数据仍在
	t.Run("backup_restore", testBackupRestore)           // 加密备份恢复到空环境后A/B、数据库与Claude HOME可用（审计§15.12）
	t.Run("no_secrets_in_logs", testNoSecretLeak)       // 服务与反代日志grep无ccw_前缀明文、令牌与OAuth token
}
```

每个子测试函数在同文件实现，调用`docker`、`curl`（或Go http）与`tmux capture-pane`完成断言；`testBootstrap`失败则`t.Fatal`终止后续。

- [ ] **Step 2: 运行确认skip/失败路径**

Run: `go test ./tests/e2e -v`（开发机）
Expected: SKIP（docker not available）——测试结构编译通过

- [ ] **Step 3: 实现`internal/sync/ws.go`与worker-agent接线**

`ws.go`（实现规格，编码时测试先行补全，含全部消息错误路径的状态机测试）：每个`put`在**项目级advisory lock+事务**中执行CAS（`base_revision`不匹配→`conflict`）→`Gate.Allow(used, oldSize, newSize)`预留→`WriteTemp(path, r, maxBytes)`（上限+1字节判超）→SHA比对`entry.SHA256`→`Promote(path, tmpID, rev)`→`Index.Upsert`（带期望revision的CAS更新）；任何一步失败回`reject`、`Discard(tmpID)`并释放预留；`mode=="cleanup"`时新增/扩大一律`readonly_mode`。

`cmd/worker-agent/main.go`补齐五件事（实现规格，编码时测试先行补全；服务用`http.Server`带四类超时并检查`ListenAndServe`错误）：

0. **terminal/sync接入时实时复查（审查§3.1）：**每次WebSocket升级通过令牌验证后、建立会话前，先`quota.Check`+磁盘检查——2分钟前签发的令牌不豁免其后发生的超额；

1. `store.New`+每项目一个`usage.Collector`（注入`usage_offsets`表的OffsetStore）每30秒`Scan`；
2. `mux.HandleFunc("GET /v1/sync", ...)`；终端与同步连接建立/断开时写`sessions`表（`connected_at`/`last_seen_at`/`state`）；
3. **云端workspace watcher（审计§6.3）：**监控各项目workspace（只读挂载+fsnotify），500ms静默窗口→前后双哈希一致才入账→与file_index比较→为Claude的新增/修改/删除分配新server revision或持久tombstone；
4. **额度主动执行循环（审计§9.3）：**每30秒及每次用量入库后`quota.Check`；项目超额→关闭该项目所有终端输入WebSocket→60秒宽限期→仍在产生新用量则`docker exec <container> pkill -INT -f claude`→保留tmux/workspace/Claude HOME→sync连接切cleanup模式→窗口恢复后允许重连。门户注明存在最后一个请求的计量延迟。

`deploy/Caddyfile`（唯一公网入口；**必须做前缀剥离/重写**，否则`/api/v1/...`原样打到只认`/v1/...`的后端全部404——审查§2.2；与spec第3节路径合同逐条对应，e2e含反代路径合同测试）：

```text
ccw.example.com {
    handle_path /api/* {
        reverse_proxy 127.0.0.1:8080
    }
    handle /ws/terminal {
        rewrite * /v1/terminal
        reverse_proxy 127.0.0.1:8081
    }
    handle /ws/sync {
        rewrite * /v1/sync
        reverse_proxy 127.0.0.1:8081
    }
}
```

（`handle_path /api/*`会剥掉`/api`前缀，`/api/v1/auth/exchange`→后端`/v1/auth/exchange`；`/portal/*`不代理——门户仅localhost+SSH隧道。）另建`deploy/versions.lock`：记录Go、全部Go模块、PostgreSQL、Ubuntu镜像tag+digest、Node.js、Claude Code、Caddy的精确版本；镜像构建与部署脚本都从此文件读取。

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
# 版本一律取自deploy/versions.lock（审查§3.2）：构建脚本以--build-arg注入，
# 并把实际使用的ubuntu digest与各版本写入镜像label。
ARG UBUNTU_TAG=24.04
FROM ubuntu:${UBUNTU_TAG}
ARG NODE_MAJOR=20
ARG CLAUDE_CODE_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    git tmux curl ca-certificates gnupg && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install -y nodejs && rm -rf /var/lib/apt/lists/*
RUN test -n "${CLAUDE_CODE_VERSION}" \
    && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}
RUN useradd -m -s /bin/bash claude
USER claude
WORKDIR /workspace
```

（`CLAUDE_CODE_VERSION`必须显式传入且记录于`deploy/versions.lock`；升级前先用脱敏JSONL样例与tmux恢复测试验证。）

`docs/runbook.md`：安装Docker与PostgreSQL（版本按`deploy/versions.lock`）→建库→跑迁移→systemd安装→管理socket建A/B项目与CDK→按`docs/admin-login-runbook.md`完成两容器登录→**备份流程（审查§3.4，直接tar运行中的卷不算备份）**：暂停写入或文件系统一致性快照→`pg_dump`一致性备份→workspace/Claude HOME/同步状态卷备份→`age`或GPG加密→复制到VPS之外→保留周期与失败告警→定期空服务器恢复演练（`deploy/backup.sh`与`deploy/restore.sh`落地）→VPS重启恢复流程→R2的24小时双登录验证记录表。

- [ ] **Step 4: 在目标VPS运行全量验收**

Run（VPS）: `go test ./... && go test -race ./... && go test ./tests/e2e -v`
Expected: 全部PASS，无SKIP（VPS有Docker）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "test: verify isolated dual-project remote workspaces"
```

### Task 13: 文件系统硬配额（审查§2.6，独立任务）

**Files:**
- Create: `deploy/quota-setup.sh`
- Create: `tests/e2e/hard_quota_test.go`
- Modify: `docs/runbook.md`（硬配额段落）

**Interfaces:**
- Consumes: `runtime.VolumeNames`的卷命名约定
- Produces: 每项目workspace卷带文件系统级容量上限；技术选型**默认每项目固定大小loop文件系统**（VPS无关）；目标VPS为XFS+`prjquota`挂载时可改用XFS project quota（runbook二选一记录，不留待实现时再定）

- [ ] **Step 1: 写e2e测试（无Docker自动skip）**——`hard_quota_test.go`：在容器内绕过同步接口直接`dd`一个超过A限额的大文件，断言：写入在配额边界失败；Project B的卷仍可写入其预留量；宿主机根分区剩余空间未被侵占（前后`df`对比误差在阈值内）。
- [ ] **Step 2: 实现`deploy/quota-setup.sh`（loop方案）**——为每项目：`truncate -s <limit> /srv/ccw/<slug>-workspace.img`→`mkfs.ext4`→`losetup`→挂载到固定目录→以`docker volume create --driver local --opt device=... --opt type=none --opt o=bind`把该目录建为命名卷；写入systemd mount单元保证重启自动恢复；脚本幂等（已存在则跳过）。
- [ ] **Step 3: runbook记录选型与容量调整流程**（扩容=新建更大img+rsync+切换，不支持在线缩小）。
- [ ] **Step 4: 在目标VPS运行**`go test ./tests/e2e -run HardQuota -v`，Expected: PASS。
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: enforce per-project filesystem hard quotas"
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
| 用量采集无重复无遗漏 | Task 8（脱敏样例）、12 |

## 验收对照补充（spec §14第13–24条，来自审计§15）

| 验收条 | 覆盖任务 |
|---|---|
| control-api重启后未过期会话仍可解析项目 | Task 10（无状态验签+查库）、12（api_restart_keeps_sessions） |
| 并发上传不能突破硬盘限额 | Task 12（advisory lock+预留；concurrent_uploads_quota） |
| 伪报文件大小不能写爆临时目录 | Task 12（io.LimitReader+too_large） |
| JSONL半行补全后准确采集 | Task 8（OffsetStore+partial_line） |
| 同一requestId多条记录按确认语义计量 | Task 0（语义确认）、Task 8 |
| session token过期后CLI自动重新exchange | Task 11 |
| 日志与错误信息无任何令牌 | Task 10、11、12（no_secrets_in_logs） |
| A超额时已连接终端不能继续提交 | Task 12（执行循环；quota_enforce_active_conn） |
| 超额/磁盘满仍能下载、删除、缩小 | Task 10（cleanup模式sync token）、12（cleanup_mode_when_full） |
| 服务端改/删文件产生revision/tombstone并回同步 | Task 12（云端watcher；cloud_edit_syncs_back） |
| worker-agent不暴露公网，流量走WSS/TLS | Task 12（Caddyfile+localhost监听） |
| 备份恢复到空服务器可用 | Task 12（backup.sh/restore.sh；backup_restore） |

## 验收对照补充二（spec §14第25–27条，来自审查§2/§3）

| 验收条 | 覆盖任务 |
|---|---|
| 反向代理路径合同逐条命中 | Task 12（Caddyfile重写；proxy_path_contract） |
| 硬配额：绕过同步写大文件不越界、不占B预留、不写满宿主机 | Task 13（quota-setup.sh；hard_quota_test） |
| cleanup模式CLI与服务端端到端可运行 | Task 10（cleanup令牌）、11（超额不退出）、12（cleanup_mode_when_full） |
| 真实容器TTY附着/断开/重连 | Task 5（-it）、12（terminal_tty_attach） |
| 三平台真实冒烟通过 | Task 11 Step 4b（cli-smoke-<os>.md证据） |
