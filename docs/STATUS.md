# 实施状态与缺口清单

**最后核对：**2026-07-26（对照分支`v2`；本次含N1+N2用量接线的改动）
**核对方式：**通读`cmd/`与`internal/`全部Go源码 + `deploy/`编排 + 跑`go build ./...`与`go test ./...`

> 实施计划`docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md`已于2026-07-26**归档**（71个checkbox一个都没勾，且Task 13等内容已被推翻）。本文件是进度的唯一可信来源；改动代码后请同步更新。

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
