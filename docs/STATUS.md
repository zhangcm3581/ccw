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

### ~~P0-1 用量采集链断开，额度闸门空转~~（2026-07-26已接线，**未真机验证**）

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
- **后台外壳**（2026-07-27）：改成传统的左侧栏 + 内容区操作台（`admin_layout.html`），全部管理页共用；导航标出当前位置、窄屏收成抽屉、主题三态（自动/浅/深，登录页共用同一份偏好）。外壳契约有测试守卫（`adminshell_test.go`：新增页面忘了走`renderAdmin`会红）。**只有本机浏览器截图证据**
- **本机端到端冒烟**：向导提交→纳管goroutine启动→日志落盘→节点入库→超上限当场拒绝，全部实测

**未实施**：C9（Route 53自动化）、C10（证书预算记账）、C16（域名管理UI）、C17（后台CDK签发/轮换、超卖水位）、C19（审计页与节点巡检）、C22（真实VPS纳管验收）、C21剩余（备份恢复演练）、解除纳管流水线。

**2026-07-26代码审查修掉的两个阻断项**：①此前只推4个编排文件到扁平目录，而compose用`context: ..`+`dockerfile: deploy/X`且Dockerfile从Go源码构建——compose-up必然失败；现改为推完整源码包（`push-source`步骤+`scripts/build-node-src.sh`+Console镜像内置），缺源码包时机队管理直接不启用。②`LogHub`的cancel会close通道，与Append并发时`panic: send on closed channel`（已复现），触发路径是部署中关掉日志页；现改为只注销不close，并给运行详情页加只读的`History()`。同批还修了：Console日志目录未挂卷（重建即丢）、install-docker注释声称配了data-root实际没有、凭据交接不进provision_steps、CSRF每次渲染换token导致多标签403、CAA查询固定query ID、日志缓冲map无上限。

**未验证（诚实表述）**：**纳管全流程从未在真实VPS上跑过**——A7（全新Ubuntu走完bootstrap）、A8（浏览器实时日志）、A9（断点续跑）、A25（host key变更中止）都只有单测与本机冒烟证据。`node_projects`/`cdk_issues`仍无写入方，`/connect`因此仍查不到数据。「继续部署」的参数存在Console内存里，重启后失效。

### P3 工具与管理面

- ~~compose硬编码双项目~~：**C13已实施**（2026-07-26）——`ccwadmin render-compose`渲染任意1–3个项目的compose，`deploy/compose.yaml`已改为渲染产物（文件头有再生成命令，勿手工编辑）；契约I1–I8有单测，输出经`docker compose config`与原手写文件比对语义相同。**B9（真实节点加第3个项目、现有客户tmux现场完好）未真机验证。**
- ~~`ccwadmin`只有`init-project`~~：**C12已实施**（2026-07-26）——新增`issue-cdk`/`rotate-cdk`（默认24h宽限、`--revoke-now`应急，设计§11.1.1）/`disable-cdk`/`list-cdks`/`list-projects`/`status`，全部支持`--json`；`init-project`幂等化（已存在返回现有信息）并支持flag形式。轮换/禁用按§11.1.1返回统一错误。003迁移给`cdks`补`created_at`。**证据强度：**store层PG集成测试（本机真实PG + CI Postgres job）+ 子命令层单测 + 本机真实PG全链路冒烟；未在生产节点跑过。仍缺：删除项目、清理tombstone
- ~~单节点上限未强制~~：**已在两个强制点生效**（2026-07-26）——`render-compose`拒第4个slug与`--disk-gib`>15；`ccwadmin init-project`拒第4个项目与`disk_gib`>15（A34/A35的节点侧部分），默认值已从20改为15 GiB。slug校验两处共用`internal/deploy.ValidateSlug`。Console前后端双校验待Console实施
- 门户只有`/usage`单页，认证复用CDK session token，没有独立管理员登录（spec §11决定是localhost+SSH隧道，Caddy已按此对公网404，与设计一致）
- ~~客户端写死默认域名`ccw.example.com`~~：**C14已实施**（2026-07-26，设计§6.7/§11.2）——寻址优先级`--api` > `CCW_API` > `~/.ccw/config.json`(0600) > 交互提示（先域名后CDK）；裸域名自动补`/api`前缀（路径合同），显式路径保留；旧`~/.ccw/cdk`自动迁移；新增`logout`；CDK仍不做命令行参数（A24有源码级守卫测试）。**A22/A23由单测覆盖；真实三平台冒烟仍未做（P2既有欠账）**
- CLI同步循环每2秒重新Dial一次WebSocket，无本地fsnotify去抖

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

## 建议推进顺序

1. ~~定`claude-shared`的去留（P0-2）~~——已于2026-07-26解决（`4093e3d`）
2. ~~接线用量采集（P0-1）~~与~~`modeFor`查额度（P1-1）~~——已于2026-07-26完成，**但只有单测证据**
3. **在真实部署上验证N1+N2**（当前最高优先级）：确认JSONL被扫到、`usage_events`非空、超额真的关终端与降级cleanup。**在此之前不得把额度闸门称为"可用"**
4. ~~`openat2`路径解析（P1-2）~~——已于2026-07-26实现（macOS+Linux容器验证，CI覆盖）
5. 镜像digest固定，然后在真实VPS上把e2e十条skip逐条填实（P1-3）
6. 加权系数校准（计划§8的开放问题1）——数据够了之后做，否则闸门永远不会真正拦人
