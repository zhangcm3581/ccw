# 实施状态与缺口清单

**最后核对：**2026-07-26（对照分支`v2`；本次含N1+N2用量接线的改动）
**核对方式：**通读`cmd/`与`internal/`全部Go源码 + `deploy/`编排 + 跑`go build ./...`与`go test ./...`

> 实施计划`docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md`已于2026-07-26**归档**（71个checkbox一个都没勾，且Task 13等内容已被推翻）。本文件是进度的唯一可信来源；改动代码后请同步更新。

> **系统图谱**：`docs/diagrams/` 有七张图（全景、登录时序、终端附着、同步三方判断、额度闸门、卷布局、纳管流水线），SVG/PNG 与 Mermaid 源码一起提交，浏览器打开 `docs/diagrams/index.html` 可一次看全。**图注上的验证强度标记与本文件同步维护**——改代码后请一并更新，别让图比代码更乐观。

## 一句话现状

Task 1–13的代码都写过一遍，`go test ./...`与`-race`全绿。2026-07-26接线了用量采集与cleanup模式（N1+N2），**额度闸门在代码层面首次成为闭环**；但**全部只有单测证据、没有在真实部署上跑过**，且e2e十条断言仍是skip，因此额度相关的验收标准尚不能记为成立。

## 按任务

| Task | 内容 | 状态 |
|---|---|---|
| 0 | 架构阻断验证 | **部分**：只有Step 3（`docs/phase1-evidence/jsonl-semantics.md`）。Step 1双登录24小时验证未做（被共享凭据方案绕过，见`design-deviations.md`）；Step 2 tmux容器原型无记录 |
| 1 | 骨架与配置加载 | 完成 |
| 2 | 迁移、CDK认证、项目绑定 | 完成（`store`首批测试已补，需真实PG，CI有Postgres集成job） |
| 3 | HMAC短期连接令牌 | 完成 |
| 4 | 容器运行时与持久卷 | 完成（假Docker API单测；PID 1为`sleep infinity`、非root、资源上限齐全） |
| 5 | tmux会话与WS终端 | 完成（`-it`双TTY、断开不kill session、resize/ping/pong/上限齐全） |
| 6 | 清单同步与冲突保护 | 完成（三方判断、CAS、冲突副本、tombstone） |
| 7 | 磁盘配额与文件索引记账 | 完成（项目级锁+预留/提交） |
| 8 | JSONL用量采集器 | **已接线**（2026-07-26），未真机验证，见P0-1 |
| 9 | 额度窗口与双层闸门 | **双层均已可用**：项目级5h/7d + 账号池（002迁移补上池上限存储，此前写死`1<<62`故池闸门从未生效），未真机验证 |
| 10 | control-api | 完成（exchange限速、cleanup模式令牌、`/usage`门户） |
| 11 | 本地CLI | 完成（三平台编译；超额不退出、令牌走header、CDK缓存0600） |
| 12 | 同步接线、e2e、部署 | **部分**：compose/Caddy/同步端点已接线并可跑；e2e十条断言全是`t.Skip`；反代路径合同无自动化测试；备份恢复完全没做 |
| 13 | 文件系统硬配额 | **不采纳**（2026-07-26定）：脚本创建的卷名与compose不兼容，执行了也不生效；已决定不改卷布局，改走替代防线——见下方「已知取舍」 |

## 缺口清单

### ~~P0-1 用量采集链断开，额度闸门空转~~（2026-07-26接线，**2026-08-02 真机验证通过**）

**整条链路已在真机上跑通并实际触发**：采集 → usage_events → 30秒判定 →
超额降级 cleanup → 客户端显示「项目受限（five_hour_limit）」。
第 1 条缺口可以划掉。

同一次验证暴露了第 2 条（系数未校准）的具体形态：Claude 账号 5 小时窗口
才用到 **11%**，我们的估算已经报 **105%** 并把项目降级。差约一个数量级，
原因是 `CCW_USAGE_WEIGHTS=1,5,1,1` 把**缓存读按输入的全价**计——而缓存读
实际只有输入的 1/10。长上下文会话每轮重读十几万 token 缓存，几十轮就吃光了
限额，账号那边几乎没动。

已按公开定价比例重标为 `10,50,1,13`（输入1·输出5·缓存读0.1·缓存写1.25，
整数化乘10），默认限额同步放大。`internal/config` 加了一条测试守住
"缓存读必须远低于输入"这个比例关系——数值可以随数据调，这个数量级差不该被
随手改回去。**仍需按 /admin/usage 的真实分布做二次校准**，尤其多人共用时。

**已做（N1+N2，见`plans/2026-07-26-usage-wiring-plan.md`）：**

- `internal/store/usage.go`：`usage.Sink`（`ON CONFLICT ... GREATEST`幂等）与`usage.OffsetStore`的PG实现
- `cmd/worker-agent/usage.go`：每30秒对**全部项目**（不是仅活跃项目）跑一轮采集，单项目panic隔离，目录缺失显式报错
- `internal/config`：`CCW_USAGE_ROOT`与`CCW_USAGE_WEIGHTS`；worker启动时`RequireUsage()`硬校验，全零权重也拒绝
- `deploy/compose.yaml`：`<slug>-claude-projects`只读挂进worker-agent——**没有这一步采集器会安静地扫空目录**
- `internal/usage/identity_linux.go`：`fileIdentity`取dev:inode，同路径重建不再沿用旧游标
- `002_account_pool_limits.sql`：`accounts`加池上限两列；worker不再写死`1<<62`，账号级闸门首次真正可用
- `cmd/worker-agent/quota.go`：`modeFor`实时查额度，超额降级cleanup，**查询失败按超额处理**（fail closed）
- `internal/quota`的`Assemble`：**control-api与worker-agent共用同一个限额组装函数与同一个数据源**。此前control-api读`CCW_POOL_5H`/`CCW_POOL_7D`环境变量、worker读`accounts`表，会出现"门户显示未超额、同步却已降级"；那两个环境变量已废弃，安全余量改由`CCW_POOL_RESERVE`/`CCW_POOL_SAFETY_MARGIN`经`config`统一注入两侧

**测试：**21条新单测（`cmd/worker-agent` 14条 + `internal/config` 7条）全绿；`internal/store`首批测试与CI的Postgres集成job覆盖幂等键与GREATEST语义。

**未验证：**以上全部只在本机单测层面成立。**没有在真实部署上跑过**——JSONL是否确实被扫到、用量是否真的进库、超额是否真的关终端，都还没有实测证据。

**仍待办：**加权系数处于"先记账、后校准"的第一阶段，取值是估算起点；跑够真实数据后必须校准，否则闸门看着已完成、实际从未拦过任何人。校准触发条件见计划§8。

### ~~P0-2 共享Claude HOME使按项目计量在原理上不成立~~（2026-07-26已解除结构性障碍）

原问题：`deploy/compose.yaml`把`claude-shared:/home/claude`同时挂给两个项目，会话JSONL混在同一个卷里，无法把usage归属到A还是B。

**已改：**新增`<slug>-claude-projects`嵌套挂载到`/home/claude/.claude/projects`，JSONL按项目分卷；凭据文件`.claude/.credentials.json`是`projects/`的兄弟节点，仍在共享卷里，**账号只授权一次的性质不变**。`Dockerfile.claude`同步预建该目录并chown，避免命名卷以root初始化。详见`design-deviations.md`的D1与设计文档§7.3。

**未验证：**改动只做了`docker compose config`语法校验，**没有在真实部署上跑过**——JSONL是否确实落进新卷、容器重建后ownership是否保持，都还没有实测证据。

**端到端计量：**采集器已于同日接线（见P0-1），归属由挂载关系保证。但**两端都缺真机证据**——结构（JSONL是否落进各自卷）与采集（是否被扫到并入库）都只有单测层面的成立。

### ~~P1-1 cleanup模式服务端未实施~~（2026-07-26已实施，**未真机验证**）

`modeFor`现在每次接受连接都实时查库（`cmd/worker-agent/quota.go`的`syncModeFor`），超额返回`cleanup`。**不信任令牌里的模式**——连接令牌2分钟有效且允许重连，只看令牌会让刚超额的项目在窗口内继续上传。**查询失败返回`cleanup`而不是`rw`**：额度状态未知时按可能已超额处理。

**未验证：**验收21（超额仍能下载/删除/缩小）与27（cleanup端到端可达）需要真实部署，尚未跑过。

### ~~P1-2 路径安全未达到生产边界~~（2026-07-26已实现）

spec §8要求的`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`已实现（`internal/sync/fsops_linux.go`，Linux构建标签）：整段路径内核原子解析、最终分量经父目录fd操作，无TOCTOU窗口；内核不支持时硬失败不降级。非Linux平台保留`EvalSymlinks`实现（`fsops_other.go`）仅作测试替代——spec允许的唯一用途。顺带收紧：叶子符号链接读取两平台一律拒绝、`Manifest`/`ScanDir`只入账普通文件（**符号链接不再参与同步**）。

**证据强度：**逃逸单测在macOS与Linux容器（kernel 6.10，真实openat2）均通过，CI ubuntu持续覆盖；未在生产节点跑过。详见`design-deviations.md`的D4（已标记解决）。

### P1-3 e2e全部skip

`tests/e2e/two_projects_test.go`的十个子测试（bootstrap、CDK隔离、双向同步、冲突副本、逻辑配额、硬配额隔离、终端重连、超额关终端、秘密泄漏）全是`t.Skip`。**没有任何一条真实VPS验收跑过。**（用skip而不是空断言是对的，避免把"没验证"报成"通过"。）

### P1-4 Phase 1证据缺两份

`docs/phase1-evidence/`只有`jsonl-semantics.md`。计划要求的`dual-login-24h.md`（24小时双登录）与`tmux-prototype.md`（容器内tmux断连重连原型）都不存在。前者已被共享凭据方案绕过，但绕过的结论没有正式记录进spec。

### P2 工程与运维

- `deploy/versions.lock`原本缺失，现已补上，但**镜像仍按tag拉取、未按digest固定**；`CLAUDE_CODE_VERSION`默认为空＝装最新，与"禁止`@latest`"冲突
- **备份/恢复完全没有**：无脚本、无演练runbook（验收24）
- **反代路径合同无自动化测试**（验收25）：`deploy/Caddyfile`有路由，但没有逐条命中的集成测试
- **云端watcher不是spec §8的形态**：现在是客户端请求manifest时顺带`reconcileCloud`全量扫目录，不是fsnotify + 500ms静默窗口 + 前后双哈希入账；`fsnotify`根本不在`go.mod`里。大目录下每2秒一轮全量扫描开销明显
- ~~无CI~~：已加`.github/workflows/ci.yml`（build/vet/test/-race/gofmt + 一个带Postgres service的store集成job）。~~`internal/store`零测试~~：已补首批，需真实PG，本机无库时skip
- **三平台真实冒烟未做**（spec §13明确"交叉编译通过不算数"）

### Console层进展（console-fleet-design，2026-07-26开始实施）

已实施（同日，均只有单测+本机冒烟证据，未在真实Console主机部署过）：

- **C1骨架**：`ccw-console`二进制（默认serve，回环8090）、`config.LoadConsole`（缺失即硬失败）、`internal/consolestore`独立库与迁移`001_console_initial.sql`（设计§10全量schema照录）。CLAUDE.md迁移规则已按设计§10要求改写为"每库一份源"
- **C20官网与发布**：落地页/下载页/quickstart（html/template+embed，零外部CDN，深浅色），`/dist`只发已发布版本登记过的文件（半成品不可达），`SHA256SUMS`从库生成；`scripts/build-release.sh`交叉编译六目标+版本注入（`cclaude --version`，A3）、`ccw-console register-release [--publish]`入库。**本机端到端冒烟通过**：构建→登记→发布→下载→`shasum -c OK`（A2的本机版）
- **C18查询页**：`/connect`浏览器本地切分CDK、只POST public-id；服务端收到含`.`立即400且不记录请求体（有日志捕获测试+突变检查）、格式错/未知/已撤销统一`not_found`、每IP每分钟10次限速。解析链cdk_issues→node_projects→nodes→node_domains有PG集成测试
- 落地页措辞有测试守卫：出现"官方订阅/保证不超过"等越界措辞即失败

- **C21部署物（部分）**：`deploy/Dockerfile.console`（distroless）、`deploy/console/{compose.yaml,Caddyfile,.env.example}`（按§8.3双域名分流+管理域名IP白名单404）、部署手册（并入`DEPLOY.md`的B部分）。Console的Postgres数据bind到data-root之外（对齐N4第2项）。`docker compose config`本机与CI均通过；**未在真实Console主机部署过**，备份恢复演练（A33）未做
- **C2信封加密**：`internal/secretbox`（AES-256-GCM，AAD绑定用途标签防止密文跨列搬运）；密文不可预测、篡改/错密钥/错上下文均检出、错误信息不含内容——各有单测
- **C5日志脱敏**：`internal/redact`覆盖设计§5.4要求的五类（CDK明文、私钥块、令牌密钥、密码各写法、AWS凭据）+ 连接串密码与`sudo -S`管道；同时有"正常输出不被误伤"的反向测试。**目前只有登录/登出两处调用方**，流水线推流接入随C7
- **C3管理员认证**：Argon2id密码 + TOTP（`internal/totp`自实现，RFC 6238官方向量验证）+ 服务端会话表（撤销立即生效、12h绝对/30min空闲超时、禁用用户即时失效）+ 每IP与每用户名双维限速 + 应用层独立IP白名单（白名单外404）+ double-submit CSRF + 审计（写入失败则动作失败）。统一错误经"三种失败响应逐字节相同"的测试守卫（A4）。`ccw-console create-admin`生成账号与两步验证密钥（密码不做命令行参数，有源码级守卫）
- **端到端冒烟（本机真实PG）**：create-admin→登录页→错密码401→正确登录302→带会话访问总览→白名单外404，全部实测通过

- **C4 SSH执行层**：TOFU host key（不符即中止，A25）、流式输出、**在最靠近数据源处脱敏**、超时与1 MiB截断。内存SSH server做单测；AST守卫禁止`InsecureIgnoreHostKey`
- **C7 流水线引擎**：precheck跳过、失败即停（后续保持pending）、断点续跑、记账失败即中止、步骤panic隔离。11条引擎单测 + 接真实PG跑"失败→续跑→成功"（A9在真实库上成立）
- **C6 凭据生命周期**：ed25519生成→注入authorized_keys（追加不覆盖、幂等）→**用新私钥重新拨号验证**→才加密落库；密码只活在调用栈里，不落库不进日志（有测试守卫）
- **C8 DNS**：Provider接口 + manual实现（双解析器交叉校验）+ 子域名分配器（序号单调递增、永不回收、保留名单）+ CAA预检（用x/net的dnsmessage自实现，标准库无CAA支持）
- **C11 bootstrap 12步**：probe（发行版白名单、磁盘核算）→harden→install-docker→dns-allocate→push-source（**推完整源码包**，节点靠它构建镜像）→push-artifacts（渲染的compose.yaml，sha256 precheck）→render-env（**密钥节点本地生成**）→compose-up→cert-wait→healthcheck→init-projects→disk-guard。硬顺序dns-allocate在compose-up之前有测试守卫；**CDK明文只经回调一次、绝不进日志**（有测试守卫）
- **C15 后台UI**：机队总览、新增节点向导、节点详情（含待添加的DNS记录提示）、运行详情与**SSE实时日志**；跨节点访问运行会404；日志落盘+推流前再脱敏一次；慢订阅者丢行不阻塞流水线
- **后台外壳**（2026-07-27）：改成传统的左侧栏 + 内容区操作台（`admin_layout.html`），全部管理页共用；导航标出当前位置、主题三态（自动/浅/深，登录页共用同一份偏好）。总览与节点页是「左栏做事、右栏看状态」的两栏工作区，表单限宽1080。**只面向PC，不做移动端**（取舍T2）。外壳契约有测试守卫（`adminshell_test.go`：新增页面忘了走`renderAdmin`会红）。**只有本机浏览器截图证据**
- **本机端到端冒烟**：向导提交→纳管goroutine启动→日志落盘→节点入库→超上限当场拒绝，全部实测

- **C16 域名页 + C17 CDK页 + C19 审计页**（2026-07-27）：
  - **项目与CDK镜像补上了写入方**——`init-projects`回调改为带回完整项目信息（`OnProject`），Console写`node_projects`/`cdk_issues`。**`/connect`因此才真正可用**（此前查不到任何数据）
  - **CDK明文有了交付路径**：签发后经内存中转在页面上显示一次、**取走即清**（10分钟TTL兜底），不落库不进日志；此前只能从节点侧输出人工取回
  - CDK页可在浏览器里签发/轮换（宽限或立即撤销）/从节点同步状态——都是远程调用节点上的`ccwadmin`，**判定规则仍只有节点那一份**；每台节点的项目数/上限3也在页面上
  - 域名页：zone创建、子域名分配总览、**待添加的A记录直接给出原文**；证书状态明确标注未实施（不假装）
  - 审计页：按动作与结果过滤、分页。审计此前只有写没有读

- **解除纳管 / 禁用CDK / 后台授权Claude**（2026-07-30）：
  - 解除纳管：**只清Console的账，不碰远端机器**（容器与数据都还在那台服务器上跑）。
    域名行保留、置released_at，序号不回收；其余从表由外键CASCADE。
    要求把节点名原样打一遍才执行，**审计先于删除**（删完就没有节点行可归属了）
  - 禁用CDK：远程调节点上的`ccwadmin disable-cdk`，镜像随之标撤销
  - **后台直接授权Claude**：Console在容器里起一个跑`claude`的tmux会话，
    把终端画面原样取回来显示，管理员粘贴授权码，Console经stdin送进会话
    （**不走命令行**，那在节点上人人可见）。**不解析Claude的输出**——
    登录提示与URL形态会随版本变，写死解析等于把后台绑死在某个客户端版本上

- **节点诊断与维护**（2026-07-30）：把两份运行手册里要登机敲的命令搬进后台。
  一次SSH跑完 `docker ps` / 每个项目的 `claude auth status` / 凭据文件属主 /
  磁盘与 data-root，输出**原文照显**（只对登录状态取一个布尔用于染色，
  判不出来就留空——把"没解析出来"显示成"未登录"会让人白折腾一轮）。
  另有「重建容器」（验证凭据随卷持久）与「从节点同步项目」
  （镜像为空时补齐，不必为一行记录重跑部署）。
  **现在还需要登机的只剩**：更换域名、轮换托管密钥（DEPLOY.md 的 A6）。
- **重置节点**（2026-07-30）：把机器擦回「装了 Docker 的干净机器」，供反复重来的
  测试循环用。销毁 ccw 的全部容器与命名卷（含 Claude 凭据）与源码树；
  **刻意保留** Docker（precheck 会跳过，每轮省几分钟）、authorized_keys 里的托管密钥
  （删了 Console 就再也连不上）、Console 库里的节点与域名分配（不用重填 IP、
  不用再等 DNS）。与「解除纳管」正交：前者擦远端留账，后者清账不碰机器。
  清理靠 compose 项目标签而不是 `cd` 进源码树——树可能已经被上一次擦除删了
  （**已实测**：compose 建的卷确实带 `com.docker.compose.project` 标签，
  且源码树不存在时 `docker compose -p <name> down -v` 仍能纯靠标签拆干净）。
  擦除成功后 Console 侧同步两件事：节点状态回「待部署」，
  **该节点的全部 CDK 标为已撤销**——哈希在被删的库里，不撤的话 `/connect`
  仍会解析出接入域名，使用者拿到域名却在 exchange 时吃 invalid_cdk。
  `rm -rf` 的目标过 `safeWipeRoot` 守卫（RepoRoot 现在写死，但接到配置上时
  空值或 `/` 就是一台机器）。**远端擦除已在真机上跑通**（2026-07-30，Node-NY-02）。
- **`cclaude -d` 与 tmux 额度状态栏**（2026-08-01）：
  `-d` 以 Bypass Permissions 模式启动云端 Claude（`--dangerously-skip-permissions`，
  已查官方 CLI 文档确认，等价于 `--permission-mode bypassPermissions`）。官方说这个
  模式只应在可随时恢复的沙箱容器里用——本项目的容器正是那种（独立容器、独立卷、
  独立配额，坏了重建即可）。**只在新建会话时生效**：会话是持久的，已经在跑的
  claude 进程改不了模式，客户端会如实提示这一点与怎么结束会话。
  客户端报上来的值经白名单收敛（`ParsePermMode`），认不出退回默认——
  它会成为容器里命令行的一部分，透传等于让客户端往 claude 的参数里塞任意东西。
  额度显示走 **Claude Code 的 statusLine**，渲染在它自己 footer 的上一行。
  **数据源改成 Claude 自己的 `rate_limits`**（2026-08-01 第二次改）：那是上游账号的
  真实额度与真实重置时间，比本仓库那套未校准的内部估算有用得多，而且不经过
  worker-agent——少一条要维护的链路，也少 30 秒的滞后。本仓库的内部额度闸门
  **仍在服务端执行**，只是不在这行显示。
  **上一版有个 Dockerfile bug 导致整次部署失败**（2026-08-01 真机）：加构建阶段时
  把 `ARG UBUNTU_TAG=24.04` 挤到了第一个 `FROM` 之后，而只有第一个 `FROM` 之前的
  ARG 才是全局的——于是 `FROM ubuntu:${UBUNTU_TAG}` 解析成 `ubuntu:`，
  报 "failed to parse stage name"。**不构建根本看不出来**，
  `internal/deploy/dockerfile_test.go` 现在扫全部 Dockerfile 守住这条。
  **账号级与项目级必须分清**（2026-08-02）：状态行的 5h/7d 来自 Claude 的
  `rate_limits`，那是**整个账号**的用量（同节点全部项目共用一个上游账号），
  所以标签是「账号5h/账号7d」。而真正会关掉终端的是**本项目**的内部额度闸门，
  两者可以完全不一致（账号还剩 80%，项目却已到顶）。worker-agent 每 30 秒把
  本项目的受限状态写进容器的 `/tmp/ccw-project-quota`，状态行读它——
  **只在受限时多出一段**（`本项目5h已满`/`本项目7d已满`/`账号池已满`），
  平时不占宽度；读不到文件一律当未受限，宁可不提示也不凭空说人受限。
  渲染器是 `cmd/ccw-statusline`（静态二进制，编进项目镜像）：四段——模型 /
  context 已用 / 5h 剩余 + 倒计时 / 7d 剩余 + 倒计时，1/8 格精度的渐进进度条。
  **写成 Go 而不是 shell**：这几段逻辑（子格填充、倒计时格式、缺数据降级）
  用 shell 写既难读也难验，也省掉往镜像里装 jq。
  **配 managed-settings 而不是 `~/.claude/settings.json`**：后者在 claude-shared
  卷里，卷初始化过之后镜像里的同名文件根本不会出现；`/etc` 不是卷，重建即生效。
  代价是 managed 优先级最高、用户无法覆盖自己的 statusLine——额度是这套系统的
  硬约束，让它始终可见是有意的取舍。
  落点用 `/tmp` 而不是任何卷：它天然每容器一份（＝每项目一份），
  而 `/home/claude` 是全部项目共享的卷，写在那里会串项目。
  两处只有执行才发现的问题：文件不存在时 `cat` 退出码为 1（**新会话的头 30 秒
  一定走这条路**），命令补了 `|| true`；写入不带尾部换行，否则 statusLine
  会多渲染一条空行把界面往上顶。
  **新连接后最多 30 秒才出现**（等下一次循环），这是已知的粗糙处。
  code review 修了两处：① 状态栏原来用**项目限额**算百分比，而 `Over` 可能是
  **账号池**耗尽——那时项目自己用量很低，状态栏会同时显示"剩90%"与"受限"，
  自相矛盾。现在显示受限原因（`受限·账号池` / `受限·5h` / `受限·7d`），
  并改用 `Assemble` 组装好的限额（顺带省掉一次重复查库）。
  ② `-d` 会话已存在时的提示原来由客户端打 stderr，而终端随即进 raw mode、
  Claude 的 alt screen 把整屏清掉——**那句话活不过一秒**。改由服务端走
  tmux `display-message`，显示在状态行上，由 tmux 自己维持。
- **云端副本显示可读名**（2026-08-01）：`og-vault-94137d17` → `og-vault`。
  哈希是给机器用的（区分 `~/a/code` 与 `~/b/code`），摆在界面上只是噪音。
  **撞名时保留哈希**——这块界面用来选删除对象，两行长得一模一样等于让人
  蒙着眼睛删。删除用的仍是真实键，显示与操作对象错开就会删错东西（有测试）。
  能对上本地目录的还会显示路径：决定"删哪个"时它比名字更能说明问题。
- **三个界面的重绘记账重写**（2026-08-01，code review 找到）：选择器、目录浏览、
  云端管理原来各自记账"打了 N 行、往回移 N 行"，而在**行数会变**的时候必错，
  且三处都真的错了：① 按 `d` 移出一项后新一帧少一行，旧的最后一行没人擦；
  ② 按 `n`/`t` 后输入提示自己又打了几行，循环末尾仍按旧行数往回移，光标停到
  比起点更高的位置，下一帧直接盖掉 shell 提示符与更早的输出；③ 目录浏览的 `p`
  分支清了两次，同款错位。**正是用户刚抱怨过的那类"旧内容留在屏幕上"，
  只不过这次是自己造的。**现在统一走 `screen`：只记上一帧实际写了多少行，
  重绘时移回去再 `ESC[J` 擦到屏幕末尾。测试直接验行数记账并做了变异验证。
- **管理云端副本**（2026-08-01）：每个本地目录在云端各有一份副本，换过机器、
  试过几个目录之后会攒下一堆再也用不到的，把项目那 15 GiB 配额慢慢吃光
  ——而在此之前用户完全看不见它们。选择器按 `c` 进入：列出全部副本与各自大小、
  显示配额占用、空格标记、Enter 删除所选。
  **删除是硬删除而不是墓碑**：普通 delete 写 tombstone 是为了把"这个文件被删了"
  同步给其他设备；而删云端副本要的恰恰相反——本地原样保留，下次连上来重新上传。
  写墓碑会把删除传播出去，把用户其他机器上的文件也删掉。
  协议加了 `workspaces` 与 `purge` 两个 op，都在会话已绑定的 projectID 范围内，
  看不到也删不掉别的项目。`purge` 在 cleanup 模式下照样允许——它只减不增，
  正是额度用尽时该能做的事。
  **最要紧的一行是 ws 过 ValidWorkspace**：它会被拼进文件系统路径，放行 `..`
  就是任意目录删除；有测试专门盯着，变异验证过。
- **同步目录与项目选择器**（2026-07-31）：此前同步的是"你碰巧所在的那个目录"，
  "在哪跑"成了必须记住的隐式状态。现在桌面上有一个固定的「cclaude 同步目录」
  （安装脚本创建，客户端每次启动也会补建——用户删掉它是正常操作）；
  裸跑 `cclaude` 弹项目选择器（↑/↓/Enter/n 新建/t 其他位置/d 移出/Esc 退出）。
  **服务端零改动**：工作区键本来就按本地绝对路径算，同步目录下的每个子文件夹
  天然是一个独立工作区。
  外部工程**就地登记，不搬文件**（2026-07-31 用户确认）：搬走会打断 IDE 工程、
  终端历史、快捷方式与其他工具的配置，而"统一放在同步目录下"只是界面上的
  整理需求，不该用移动用户文件来实现。列表里外部项目显示真实路径、
  同步目录内的不显示。
  几处安全边界：`--dir` 跳过选择器（脚本/CI）；非 TTY 回退到当前目录（旧行为）；
  **盘符根、家目录、同步目录自身不提示「登记当前目录」**——在那些地方回车一下
  会把几十 GB 无关文件推上云端；`d` 只移出列表、绝不删文件。
  路径比较解符号链接（macOS 的 /var、Windows junction、OneDrive 重定向），
  否则"明明在自己的工程里，每次还是弹选择器"。
  `t 其他位置` 是**可浏览的目录选择器**（↑/↓ 移动、Enter/→ 进入、←/Backspace
  上一级、s 选定当前目录、p 直接输入、Esc 取消），Windows 上在盘符根再上一级
  会回到盘符列表。手打 `C:\TestProjects\SyntheticProject` 既慢又容易打错，
  而打错只得到一句"不是一个存在的目录"。读不了的目录列一行原因、仍可返回上级，
  不会把人困住。
- **额度档位与池上限自动校准**（2026-08-02）：项目限额不再是写死的绝对值，
  而是「档位比例 × 账号池上限」推导出来的。默认三档 `2x/5x/7x` = `10%/25%/33%`
  （合计 68%，刻意留余量；有测试记录该意图）。项目没挂档位时沿用绝对限额——
  迁移前建的项目行为一点不变。比例存万分之一（整数），因为限额要参与 `>=` 比较，
  浮点会带来"到底算不算超"的边界抖动。
  **池上限由真实账号用量自动校准**：官方 CLI 没有 usage 命令，Claude 的 rate_limits
  只随会话的 statusline JSON 送进来——状态行把它写成快照，worker-agent 反推
  `池上限 ≈ 池内累计 ÷ 真实百分比`。三条防呆：百分比<5%不校准（窗口刚开始时
  分母噪声被放大）、累计量<1000不校准、有值时每次只朝估计值移动20%
  （单次异常快照不该把所有档位的分母一把改掉）。快照超 10 分钟即作废。
  Console 的 `/admin/usage` 上可改档位百分比、给项目指派档位（CSRF + 审计），
  改完闸门下一轮（约 30 秒）按新值判。同页还有 **Claude 账号卡**：登录状态、
  `claude auth status` 原文，以及账号级真实用量——后者由活跃会话的状态行回传，
  页面标明"数据截至"，因为官方 CLI 没有查用量的接口，没人在用时它会停在
  最后一次会话的时刻。
  **客户端显示的是分配给本项目的那一份**（`本项目5h/7d`），不是账号级：
  账号百分比是全机共用的数，看的人分不出哪部分是自己的，也解释不了
  "账号还剩 80% 而我被关了"。拿不到本项目额度时退回账号级，标签如实写成「账号」。
- **用量页**（2026-08-02）：`/admin/usage` 显示各项目的**真实 token 消耗**
  （按模型拆分：输入/输出/缓存读/缓存写，逐条来自 Claude 写的会话 JSONL）
  与**内部额度单位**占比。两者在页面上明确分开——前者是真实的，
  后者是 `CCW_USAGE_WEIGHTS` 折算出来的估算，spec §10 明令不得标成官方订阅百分比。
  数据经 `ccwadmin usage --json` 走 SSH 从各节点取，不落 Console 库
  （节点库才是权威，两个库 schema 无交集）。
  **「最近无采集」标记是判断采集链路死没死的唯一线索**——链路断掉
  （最常见是 compose 里 `<slug>-claude-projects` 没只读挂进 worker-agent）的表现
  不是报错，而是"一切正常、表永远是空的"。阈值取 24 小时，免得正常的周末空闲被误报。
  **计量按项目，不按 CDK**：用量数据源是容器里的会话 JSONL，它不知道这次连接
  用的是哪张卡；同项目的全部 CDK 共用一个容器与一份凭据，令牌里也只有
  ProjectID。要按人分开算就一人一个项目（单节点上限 3 个）。
- **左侧残影的根因是 ConPTY，用官方变量修**（2026-07-31）：官方文档
  code.claude.com/docs/en/fullscreen 的「Stale or misplaced text」写明：
  fullscreen 渲染只发送变化的单元格，而 **Windows Terminal 等 ConPTY 宿主会错误
  合并这些定位写入**，把上一帧片段留在屏幕上。修复是
  `CLAUDE_CODE_ALT_SCREEN_FULL_REPAINT=1`（每帧重画所有单元格）。
  连同 `CLAUDE_CODE_NO_FLICKER=1` 一起**写进 compose 的项目服务环境**，
  而不是远端的 `~/.claude/settings.json`——那份文件在 claude-shared 卷里，
  一次「重置节点」就没了，且要 SSH 上去改。契约 I9 守着这两个变量。
  代价：全量重绘的字节数高于增量，而这条链路是远程 WebSocket；先要正确。
  用户仍可 `/tui default` 切回经典渲染。
  **同一批 tmux 改动里，`set -g mouse on` 是官方文档明确要求的前置条件**
  （没有它滚轮事件被 tmux 吃掉，不会送到 Claude）；而当时给 `escape-time 0`
  写的"能修残影"是推断且推错了方向，注释已更正。
- **容器里没有 tmux 配置、TERM 也没传**（2026-07-31，真机渲染残影后查出）：
  两条默认值在这套用法下是错的。① `docker exec -it` **写死 `TERM=xterm`**
  （已实测，与调用方环境无关），只宣告 8 色；② tmux 的 `default-terminal` 默认
  `screen`，于是容器里的 Claude Code 看到的是 `TERM=screen`。一个重绘频繁的 TUI
  在能力被低估时会留下重绘残影。③ tmux `mouse` 默认 off，滚轮不归 tmux 管。
  现在：新增 `deploy/tmux.conf`（`default-terminal=tmux-256color`、真彩色透传、
  `mouse on`、`escape-time 0`、`history-limit 20000`）装到 `/etc/tmux.conf`
  （实测 tmux 3.4 会自动加载；`tmux-256color` 这份 terminfo 在 ubuntu:24.04
  基础镜像里就有，无需 ncurses-term）；客户端经 `X-CCW-Term` 上报自己的 TERM，
  服务端校验后用 `-e TERM=` 传进容器，非法或缺失退回 `xterm-256color`
  （**不拒绝连接**——这只影响显示，为它断掉终端不成比例）。
  端到端实测：tmux 内部的 TERM 从 `screen` 变成 `tmux-256color`。
  **注意**：`mouse on` 之后拖选归 tmux 管，要用终端原生选择需按住 Shift。
- **Windows 上终端尺寸从不上报**（2026-07-30，真机暴露）：客户端用
  `term.GetSize(os.Stdin.Fd())` 轮询尺寸，而 Windows 上它走
  `GetConsoleScreenBufferInfo`——那个 API 只接受**屏幕缓冲区**句柄（stdout/stderr），
  传 stdin 必然失败，于是 resize 帧一次都没发出去，PTY 永远停在服务端建会话时的
  默认尺寸，界面只占窗口左上角一块。Unix 的 `TIOCGWINSZ` 对 stdin 有效，
  所以这条在 macOS/Linux 上从来看不见。
  现在按 stdout → stderr → stdin 依次试，并且**连上立刻发一次**（等第一个
  500ms tick 意味着 Claude 的欢迎界面按默认尺寸画完了再重画）。
  探测顺序提成变量并单独断言——真正会被改回去的是顺序，不是探测函数本身。
- **同步落盘的文件归容器里的 claude(1001)**（2026-07-30，真机暴露）：
  worker-agent 以 root 写盘（它持 docker.sock），项目容器以 1001 运行同一个卷，
  于是同步上去的文件是 `root:root 0600`——容器里 `cat` 直接 Permission denied，
  Claude 读不了也改不了。目录同样要 chown：`root:root 0755` 能进能读，
  但没法在里面建文件。
  `DirStore` 只在服务端构造（客户端下行走 `writeLocal`，用当前用户身份），
  所以加 chown 不影响本地文件。判定用 `uid > 0` 而不是 `>= 0`——uid 0 就是 root，
  当成 chown 目标毫无意义，这样零值（单元测试）天然表示"不改属主"。
  **UID 不做成配置项**：它由 `deploy/Dockerfile.claude` 的 `useradd -u 1001` 定死，
  加个 env 只是多一个能让两处悄悄漂移的地方（漂了不报错，只是"容器里读不了"）；
  改成常量 + 一条读 Dockerfile 比对的测试。
  历史遗留的 root 文件由 `repairOwnerOnce` 修回来（每进程每目录一次——
  客户端每 2 秒重连，不能每次遍历整棵树）；修不了也不阻断同步。
  三条都在 Linux 容器里以 root 实测，并对两个 chown 点各做了变异验证。
- **安装脚本支持重装/升级**（2026-07-30）：重装是常态（客户端升级就得重装）。
  两个脚本都会先探测旧版本并打印「已从 v0.1.0 升级到 v0.1.1」；
  **PATH 遮挡**会告警——别处有一个更靠前的 `cclaude` 时，装了新的也还是跑旧的，
  表现是"明明升级过、行为还是老的"，极难自己看出来。
  install.ps1 另外接住"exe 正在运行被锁住"，提示先关窗口，而不是抛一段 .NET 异常。
  两个脚本都**实际执行验证过**（install.sh 起本地 HTTP 服务真装了一遍，
  install.ps1 的版本解析与遮挡判断在 PowerShell 容器里跑过）；
  执行时抓到一个 `$VERSION（` 被 sh 吃进变量名的 unbound variable
  ——与 76472b2 同一个坑，现在有测试守着。
- **握手被拒时带出服务端原因**（2026-07-30，真机暴露）：旧版客户端连新部署的节点，
  服务端在 upgrade 之前以 400 `workspace required` 拒掉（工作区隔离去掉了老客户端
  兼容分支），而 gorilla 只给 `websocket: bad handshake`——用户看到这一句，
  既不知道是版本错配、也不知道 CDK 其实是好的（额度行已经打出来了），
  于是反复重输 CDK。现在读 HTTP 握手响应，把状态码与原因带出来，
  400+workspace 直接说"这个客户端比节点旧，重跑一次安装命令；CDK 不用换"。
  终端与同步两侧都改了。
  **提醒**：`dist/` 里的客户端产物不随 Console 更新而重建——节点侧改了协议就必须
  重新 `scripts/build-release.sh` + 登记发布，否则新装的用户拿到的仍是旧二进制。
- **/quickstart 并入 /connect**（2026-07-30）：公开站原有两个上手页，
  而 quickstart 只能给占位符命令（`cclaude --api https://api-01.example.com`
  外加一句"换成你的API域名"）——有 CDK 就能查出真域名，那个页面没有存在必要。
  现在 /connect 是唯一上手页：粘 CDK → 直接给出填好域名的两条命令；
  结果区在查询前完全不显示（没 CDK 时摆占位符只会让人照着抄错）。
  命令块里只放**能直接跑的那一行**——`cd 你的项目目录` 移到标题里，
  掺进命令块的话复制走就跑不起来，与占位符域名是同一类问题。
  常见问题一节删除。`/quickstart` 301 到 `/connect`（下载页与外链都指过它），有测试守着。
- **install.ps1 装完当场可用**（2026-07-30，真机 PowerShell 7.6.4 暴露）：
  原先只写用户级 PATH，而 `SetEnvironmentVariable(...,'User')` 只对将来启动的
  进程生效——`irm ... | iex` 就跑在当前会话里，于是装完立刻敲 `cclaude` 必然是
  "不是可识别的命令"，而脚本那句"请新开一个终端"只是把缺陷写成了提示语。
  现在同时更新 `$env:PATH`。顺带修掉一个更糟的：PATH 去重原来用
  `-notlike "*$dest*"`，已有 `...\cclaude2` 会被当成"已安装"而**跳过写入**,
  那样连开新终端都救不回来。改成按分号切段精确比对。
  `tests/e2e/install_ps1_test.go` 用 PowerShell 容器**真的执行**这套 PATH 逻辑
  （无 Docker 自动 skip）——此前 install 脚本的测试只断言文本、从没执行，
  正是那样让 `tar xzf` 对着裸 exe 的 bug 混过了全部测试。
- **healthcheck 经回环探测**（2026-07-30，真机暴露）：客户端从公网拿到 401
  （说明 Caddy 与 control-api 都健康），而节点自己 curl 同一个域名得到 000。
  这一步要验的是「Caddy → control-api 接对了」，不是「云厂商的 hairpin NAT 通不通」
  ——后者与栈的健康无关，却足以让整次部署失败。改用
  `curl --resolve "$fq:443:127.0.0.1"`：只改连哪个 IP，SNI 与证书仍按真实域名校验
  （实测：指对 IP 得 200、指错 IP 连不上），覆盖面没缩小。失败时会再走一次公网路径，
  用来佐证"是栈坏了还是只是绕不回来"。
- **客户端错误可照着做**（2026-07-30，真机暴露）：`control: rejected with status 401`
  改成说清下一步。401 同时覆盖"卡打错了"与"卡已不存在"，服务端刻意不区分
  （不泄露存在性），客户端同样不猜，但把该做什么说清楚。测试同时守着
  「不得指向不存在的命令」——初稿写的 `cclaude --reset` 并不存在（实际是
  `cclaude logout`），而认证失败时客户端本就会自动清缓存，压根不需要额外命令。
- **healthcheck 会自证失败原因**（2026-07-30，真机暴露）：原先探测用 `curl -s`，
  失败原因被吞掉，错误只剩一个 `000`——连不上、TLS 校验失败、超时全是 000，
  管理员无从下手。现在：探测重试 30×5s（`ps` 说 running 不等于在听端口，
  control-api 启动要跑迁移，重置后是空库更久）；改用 `-sS` 并带出 curl 退出码；
  最终失败时打印域名解析、80 端口响应、caddy 与 control-api 的近期日志——
  502 说明 Caddy 好而后端没起来，000 说明连 Caddy 都没碰到，排查方向完全不同。
  现场收集不 `cd` 进源码树（重置后可能已不存在），纯用 `docker compose -p`。
- **续跑参数可从库里重建**（2026-07-30，真机暴露）：「继续 / 重新部署」原先只认
  内存里的 `lastInput`，而**更新 Console 就是重建镜像＋重启**——重启后按钮必然报
  「本次 Console 启动后没有该节点的部署参数」，「重置 → 重新部署」这个循环一过
  更新就断。现在内存缺失时从 Console 库重建：slug 与配额来自 `node_projects`
  镜像、zone 来自域名分配，首登密码不需要（续跑用库里的托管密钥）。
  取不到 slug 时**不猜**——那意味着这台机器从未跑到 init-projects，
  用空列表跑会渲染出一个没有任何项目的 compose。
- **后台授权流程已按真机实测修正**（2026-07-30，ubuntu:24.04 + Claude Code v2.1.220，
  本机 Docker 里跑通）。实测纠正了三处此前靠推断写错的地方：
  ①首次运行的第一屏是**主题选择器**、第二屏是登录方式，之后才到粘贴码界面——
  前两屏要方向键与回车，只给文本框的话管理员会卡死在第一屏；
  ②URL 约 400 字符，pane 开 200 列时 Claude 的 TUI 会**自己**折成三行，
  `capture-pane -J` 只能合并终端折行、救不回来，改为 600 列后完整成一行；
  ③`capture-pane` 必须带 `-J`（另一层的终端折行）。
  **仍未验证**：真实节点上的端到端（本机验的是镜像与流程形态，不是 Console→SSH→节点 这条链）。

**未实施**：C9（Route 53自动化）、C10（证书预算记账与到期告警）、C19剩余（节点巡检）、C22（真实VPS纳管验收）、C21剩余（备份恢复演练）。

**2026-07-26代码审查修掉的两个阻断项**：①此前只推4个编排文件到扁平目录，而compose用`context: ..`+`dockerfile: deploy/X`且Dockerfile从Go源码构建——compose-up必然失败；现改为推完整源码包（`push-source`步骤+`scripts/build-node-src.sh`+Console镜像内置），缺源码包时机队管理直接不启用。②`LogHub`的cancel会close通道，与Append并发时`panic: send on closed channel`（已复现），触发路径是部署中关掉日志页；现改为只注销不close，并给运行详情页加只读的`History()`。同批还修了：Console日志目录未挂卷（重建即丢）、install-docker注释声称配了data-root实际没有、凭据交接不进provision_steps、CSRF每次渲染换token导致多标签403、CAA查询固定query ID、日志缓冲map无上限。

**未验证（诚实表述）**：**纳管全流程从未在真实VPS上跑过**——A7（全新Ubuntu走完bootstrap）、A8（浏览器实时日志）、A9（断点续跑）、A25（host key变更中止）都只有单测与本机冒烟证据。2026-07-27补的项目/CDK入库与后台三页同理：**只有单测与浏览器截图**，`/connect`能否真解析到域名、浏览器里签发CDK能否真在节点上生效，都还没在真机上跑过。「继续部署」的参数存在Console内存里，重启后失效。

### P3 工具与管理面

- ~~compose硬编码双项目~~：**C13已实施**（2026-07-26）——`ccwadmin render-compose`渲染任意1–3个项目的compose，`deploy/compose.yaml`已改为渲染产物（文件头有再生成命令，勿手工编辑）；契约I1–I8有单测，输出经`docker compose config`与原手写文件比对语义相同。**B9（真实节点加第3个项目、现有客户tmux现场完好）未真机验证。**
- ~~`ccwadmin`只有`init-project`~~：**C12已实施**（2026-07-26）——新增`issue-cdk`/`rotate-cdk`（默认24h宽限、`--revoke-now`应急，设计§11.1.1）/`disable-cdk`/`list-cdks`/`list-projects`/`status`，全部支持`--json`；`init-project`幂等化（已存在返回现有信息）并支持flag形式。轮换/禁用按§11.1.1返回统一错误。003迁移给`cdks`补`created_at`。**证据强度：**store层PG集成测试（本机真实PG + CI Postgres job）+ 子命令层单测 + 本机真实PG全链路冒烟；未在生产节点跑过。仍缺：删除项目、清理tombstone
- ~~单节点上限未强制~~：**已在两个强制点生效**（2026-07-26）——`render-compose`拒第4个slug与`--disk-gib`>15；`ccwadmin init-project`拒第4个项目与`disk_gib`>15（A34/A35的节点侧部分），默认值已从20改为15 GiB。slug校验两处共用`internal/deploy.ValidateSlug`。Console前后端双校验待Console实施
- 门户只有`/usage`单页，认证复用CDK session token，没有独立管理员登录（spec §11决定是localhost+SSH隧道，Caddy已按此对公网404，与设计一致）
- ~~客户端写死默认域名`ccw.example.com`~~：**C14已实施**（2026-07-26，设计§6.7/§11.2）——寻址优先级`--api` > `CCW_API` > `~/.ccw/config.json`(0600) > 交互提示（先域名后CDK）；裸域名自动补`/api`前缀（路径合同），显式路径保留；旧`~/.ccw/cdk`自动迁移；新增`logout`；CDK仍不做命令行参数（A24有源码级守卫测试）。**A22/A23由单测覆盖；真实三平台冒烟仍未做（P2既有欠账）**
- CLI同步循环每2秒重新Dial一次WebSocket，无本地fsnotify去抖

## 2026-07-29 的三处改动

- **workspace 按本地目录隔离**（修真机上的数据污染）：此前一个项目的云端 workspace 是
  平铺目录，客户端在任意本地目录跑都映射到同一处——在 `~/code` 用过一次再进 `~/work`，
  云端会把 code 的全部文件同步下来。现在客户端按本地绝对路径算工作区键
  （`目录名-哈希8位`，见 `internal/sync/workspace.go`），云端按键分层：
  文件落 `<root>/<slug>/<ws>/`，索引路径加 `<ws>/` 前缀（**不需要改表结构**），
  tmux 会话名与工作目录也跟着走。协议上 `hello` 必须带 `ws`，
  **老客户端会被明确拒绝而不是退回平铺目录**——静默兼容等于继续制造污染且没人发现。
  五条单测守着，含"两个工作区互不可见"与"同名文件不互相覆盖"。
  **破坏性**：已有部署里 `<root>/<slug>/` 下的旧文件不在任何工作区内，
  升级后客户端看不到它们；需要手动 `mv` 进对应的工作区子目录，或重新上传。
- **客户端一键安装**：`curl -fsSL <站点>/install.sh | sh`（macOS/Linux）与
  `irm <站点>/install.ps1 | iex`（Windows）。脚本由 Console 按当前已发布版本渲染，
  **校验和内嵌**（脚本与产物同源，再去同源取 SHA256SUMS 校验不了任何东西），
  装完 `cclaude` 为全局命令。下载页仍保留手动下载。
- **节点镜像改用官方安装脚本装 Claude Code**：`curl -fsSL https://claude.ai/install.sh | bash`，
  不再走 npm，因此镜像里不再需要 Node 与 nodesource 源。

## 已知取舍（不是缺口，不要当待办）

本节记录**已明确决定不做**的事。它们看起来像缺口，但改动方向已被否决，请勿重新提为待办。

### T1 文件系统硬配额不启用，同机客户磁盘互相影响（2026-07-26定）

**事实：**`deploy/quota-setup.sh`创建的卷名是`<slug>-workspace`，compose实际使用的卷带项目前缀（`docker compose config`实测为`deploy_project-a-workspace`）。**两者不是同一个卷**，因此脚本执行了也不生效，且没有任何报错——它会正常退出并打印"capped at NN GiB"。

**决定：**不改卷布局（渲染计划§4.4，用户2026-07-26定）。因此硬配额不启用，脚本保留但已在文件头标注"勿接入流程"，也已从bootstrap流水线中删除（设计§5.3步骤12）。

**承担的后果：**逻辑配额（`internal/storage`）只统计走同步接口的文件。客户在终端里`npm install`、堆构建缓存或直接`dd`可以突破15 GiB上限，撑爆后**同机全部客户一并受影响**。这通常不需要恶意即可触发。

**替代防线**（设计§12.1的N4，尚未实施）：Docker`data-root`指向独立分区 + Postgres数据移出data-root + 磁盘水位告警。这些只把后果从"整台机器死亡且救不回来"降到"客户一起降级、管理员能登机处理"，**不消除客户间的互相影响**。

**对外表述边界：**15 GiB只能称"配额"，**不得**表述为"保证不超过"（设计§7.6）。

**若将来要重开：**回到`docs/superpowers/plans/2026-07-26-compose-render-plan.md`§4.2重新选型，代价是渲染器加`--workspace-mode`、bootstrap多一条硬顺序约束、已有部署迁卷。

**证据强度：**静态代码审查 + `docker compose config`本机输出；**未在真实部署上复现**。将来若重开该方向，先`docker volume ls`比对确认。

### T2 管理后台只面向PC，不做移动端适配（2026-07-27定）

**事实：**`/admin/*`是纳管服务器用的操作台——12步流水线的步骤表、实时日志流、SSH参数表单、`sha256`指纹核对，没有一项适合在手机上操作。曾做过一版窄屏抽屉，但它只是让页面"能打开"，并不能让人真的在手机上纳管一台机器。

**决定：**撤掉窄屏适配（用户2026-07-27定）。`.shell`加`min-width:1080px`，窗口比这窄就整页横滚——布局形态始终只有一种，不在小屏上折成另一副样子。

**承担的后果：**手机与窄窗口上后台只能横向拖动查看，体验差。公开站点（`layout.html`那套：落地页/下载/`/connect`）**不受影响**，它仍是自适应的——那才是终端用户会用手机打开的部分。

**若将来要重开：**改动集中在`admin_layout.html`一个文件（外壳CSS+两处workspace网格），不影响任何handler与视图模型。

## 建议推进顺序

1. ~~定`claude-shared`的去留（P0-2）~~——已于2026-07-26解决（`4093e3d`）
2. ~~接线用量采集（P0-1）~~与~~`modeFor`查额度（P1-1）~~——已于2026-07-26完成，**但只有单测证据**
3. **在真实部署上验证N1+N2**（当前最高优先级）：确认JSONL被扫到、`usage_events`非空、超额真的关终端与降级cleanup。**在此之前不得把额度闸门称为"可用"**
4. ~~`openat2`路径解析（P1-2）~~——已于2026-07-26实现（macOS+Linux容器验证，CI覆盖）
5. 镜像digest固定，然后在真实VPS上把e2e十条skip逐条填实（P1-3）
6. 加权系数校准（计划§8的开放问题1）——数据够了之后做，否则闸门永远不会真正拦人
