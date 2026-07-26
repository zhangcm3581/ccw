# 实施状态与缺口清单

**最后核对：**2026-07-26（对照分支`v2`，commit `feedb06`；P0-2改动已于`4093e3d`提交）
**核对方式：**通读`cmd/`与`internal/`全部Go源码 + `deploy/`编排 + 跑`go build ./...`与`go test ./...`

> 实施计划`docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md`已于2026-07-26**归档**（71个checkbox一个都没勾，且Task 13等内容已被推翻）。本文件是进度的唯一可信来源；改动代码后请同步更新。

## 一句话现状

Task 1–13的代码都写过一遍，13个包全部有单测且`go test ./...`全绿（约4.5k行Go）；但**用量采集链未接线、cleanup模式服务端未实施、e2e全部skip**，因此额度与配额相关的验收标准实际尚未成立。

## 按任务

| Task | 内容 | 状态 |
|---|---|---|
| 0 | 架构阻断验证 | **部分**：只有Step 3（`docs/phase1-evidence/jsonl-semantics.md`）。Step 1双登录24小时验证未做（被共享凭据方案绕过，见`design-deviations.md`）；Step 2 tmux容器原型无记录 |
| 1 | 骨架与配置加载 | 完成 |
| 2 | 迁移、CDK认证、项目绑定 | 完成（`store`包本身零测试） |
| 3 | HMAC短期连接令牌 | 完成 |
| 4 | 容器运行时与持久卷 | 完成（假Docker API单测；PID 1为`sleep infinity`、非root、资源上限齐全） |
| 5 | tmux会话与WS终端 | 完成（`-it`双TTY、断开不kill session、resize/ping/pong/上限齐全） |
| 6 | 清单同步与冲突保护 | 完成（三方判断、CAS、冲突副本、tombstone） |
| 7 | 磁盘配额与文件索引记账 | 完成（项目级锁+预留/提交） |
| 8 | JSONL用量采集器 | **代码完成但未接线**，见P0-1 |
| 9 | 额度窗口与双层闸门 | 逻辑完成（5h/7d + 池双窗口），但输入恒为空，见P0-1 |
| 10 | control-api | 完成（exchange限速、cleanup模式令牌、`/usage`门户） |
| 11 | 本地CLI | 完成（三平台编译；超额不退出、令牌走header、CDK缓存0600） |
| 12 | 同步接线、e2e、部署 | **部分**：compose/Caddy/同步端点已接线并可跑；e2e十条断言全是`t.Skip`；反代路径合同无自动化测试；备份恢复完全没做 |
| 13 | 文件系统硬配额 | **不采纳**（2026-07-26定）：脚本创建的卷名与compose不兼容，执行了也不生效；已决定不改卷布局，改走替代防线——见下方「已知取舍」 |

## 缺口清单

### P0-1 用量采集链断开，额度闸门空转

`internal/usage/collector.go`（184行，含offset持久化、半行拼接、requestId最终值语义）**没有任何文件import**：

- `worker-agent`里没有采集goroutine
- `internal/store`里没有`usage_events`的INSERT，也没有`usage_offsets`表的`OffsetStore`实现
- 加权系数`usage.Weights`只出现在测试里，生产无配置入口

后果链：`usage_events`永远为空 → `WindowUsed`恒为0 → `quota.Check`永远`Over=false` → worker的30秒主动执行循环永远不关终端 → `/usage`门户与CLI状态栏永远显示0。

**影响验收：**12（采集无重复无遗漏）、16（半行补全）、17（同requestId语义）、20（超额关闭已连终端）。

### ~~P0-2 共享Claude HOME使按项目计量在原理上不成立~~（2026-07-26已解除结构性障碍）

原问题：`deploy/compose.yaml`把`claude-shared:/home/claude`同时挂给两个项目，会话JSONL混在同一个卷里，无法把usage归属到A还是B。

**已改：**新增`<slug>-claude-projects`嵌套挂载到`/home/claude/.claude/projects`，JSONL按项目分卷；凭据文件`.claude/.credentials.json`是`projects/`的兄弟节点，仍在共享卷里，**账号只授权一次的性质不变**。`Dockerfile.claude`同步预建该目录并chown，避免命名卷以root初始化。详见`design-deviations.md`的D1与设计文档§7.3。

**未验证：**改动只做了`docker compose config`语法校验，**没有在真实部署上跑过**——JSONL是否确实落进新卷、容器重建后ownership是否保持，都还没有实测证据。

**仍不成立的部分：**端到端的按项目计量依赖P0-1（采集器接线），后者未做，所以`usage_events`仍为空。P0-2解除的只是结构性障碍，不是计量能力本身。

### P1-1 cleanup模式服务端未实施

`cmd/worker-agent/main.go`的`modeFor`恒返回`"rw"`（代码里就标着`TODO(12-4)`）。`control-api`已经会在超额时签发cleanup模式的sync令牌，但worker不据此降级，超额状态下仍接受上传。

**影响验收：**21（超额仍能下载/删除/缩小）、27（cleanup端到端可达）。

### P1-2 路径安全未达到生产边界

`internal/sync/server.go`的`DirStore.resolve`用`filepath.EvalSymlinks`，注释自己写明"存在检查与写入之间的TOCTOU窗口，只适合本机/非Linux测试"。spec §8要求Linux生产**必须**用`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`或逐级目录fd的等价实现——未实现。

### P1-3 e2e全部skip

`tests/e2e/two_projects_test.go`的十个子测试（bootstrap、CDK隔离、双向同步、冲突副本、逻辑配额、硬配额隔离、终端重连、超额关终端、秘密泄漏）全是`t.Skip`。**没有任何一条真实VPS验收跑过。**（用skip而不是空断言是对的，避免把"没验证"报成"通过"。）

### P1-4 Phase 1证据缺两份

`docs/phase1-evidence/`只有`jsonl-semantics.md`。计划要求的`dual-login-24h.md`（24小时双登录）与`tmux-prototype.md`（容器内tmux断连重连原型）都不存在。前者已被共享凭据方案绕过，但绕过的结论没有正式记录进spec。

### P2 工程与运维

- `deploy/versions.lock`原本缺失，现已补上，但**镜像仍按tag拉取、未按digest固定**；`CLAUDE_CODE_VERSION`默认为空＝装最新，与"禁止`@latest`"冲突
- **备份/恢复完全没有**：无脚本、无演练runbook（验收24）
- **反代路径合同无自动化测试**（验收25）：`deploy/Caddyfile`有路由，但没有逐条命中的集成测试
- **云端watcher不是spec §8的形态**：现在是客户端请求manifest时顺带`reconcileCloud`全量扫目录，不是fsnotify + 500ms静默窗口 + 前后双哈希入账；`fsnotify`根本不在`go.mod`里。大目录下每2秒一轮全量扫描开销明显
- **无CI**（没有`.github/`）、`internal/store`零测试
- **三平台真实冒烟未做**（spec §13明确"交叉编译通过不算数"）

### P3 工具与管理面

- `ccwadmin`只有`init-project`：没有CDK轮换/禁用、删除项目、查用量、清理tombstone等命令。
  **补充（2026-07-26核实）：CDK轮换的读路径已完备，只缺写入端。**`cdks`表有`disabled_at`/`expires_at`，`internal/store/postgres.go:92`的`ResolveCDK`已校验二者并用数据库`now()`，客户端认证失败时也已会清缓存（`cmd/cclaude/main.go:44`）。缺的只是**写`disabled_at`的代码**——全仓对该列仅有那一处读引用。实现量为`store`加一个方法 + CLI子命令，设计见console-fleet-design §11.1.1
- **单节点上限未强制**：产品规则定为最多3个项目容器、单项目磁盘配额上限与默认值均为15 GiB（节点规格80 GB盘），但`ccwadmin init-project`既不限项目数也不限`disk_gib`，且`cmd/ccwadmin/main.go:27`的默认值仍是旧的20 GiB。设计见console-fleet-design §7.6
- 门户只有`/usage`单页，认证复用CDK session token，没有独立管理员登录（spec §11决定是localhost+SSH隧道，Caddy已按此对公网404，与设计一致）
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

1. ~~定`claude-shared`的去留（P0-2）~~——已于2026-07-26解决：JSONL按项目分卷，凭据仍共享（`4093e3d`）。**但未在真实部署上验证**
2. 接线用量采集（P0-1）：worker起collector goroutine + `usage_events`/`usage_offsets`的PG实现 + 权重配置化。**注意：设计§12.1把N1标为依赖C13，该依赖已随`4093e3d`解除，现在即可开工**
3. `modeFor`查额度（P1-1）：改动量小，但直接影响两条验收
4. `openat2`路径解析（P1-2）：Linux生产必需
5. 补CI与digest固定，然后在真实VPS上把e2e十条skip逐条填实（P1-3）
