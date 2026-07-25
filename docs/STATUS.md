# 实施状态与缺口清单

**最后核对：**2026-07-25（对照分支`v2`，commit `4bbd4c0`）
**核对方式：**通读`cmd/`与`internal/`全部Go源码 + `deploy/`编排 + 跑`go build ./...`与`go test ./...`

> 实施计划`docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md`里的71个checkbox**一个都没勾**，不反映真实进度。本文件是进度的唯一可信来源；改动代码后请同步更新。

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
| 13 | 文件系统硬配额 | **部分**：`deploy/quota-setup.sh`写好了，但没有任何文档或流程调用它，compose直接建普通命名卷 |

## 缺口清单

### P0-1 用量采集链断开，额度闸门空转

`internal/usage/collector.go`（184行，含offset持久化、半行拼接、requestId最终值语义）**没有任何文件import**：

- `worker-agent`里没有采集goroutine
- `internal/store`里没有`usage_events`的INSERT，也没有`usage_offsets`表的`OffsetStore`实现
- 加权系数`usage.Weights`只出现在测试里，生产无配置入口

后果链：`usage_events`永远为空 → `WindowUsed`恒为0 → `quota.Check`永远`Over=false` → worker的30秒主动执行循环永远不关终端 → `/usage`门户与CLI状态栏永远显示0。

**影响验收：**12（采集无重复无遗漏）、16（半行补全）、17（同requestId语义）、20（超额关闭已连终端）。

### P0-2 共享Claude HOME使按项目计量在原理上不成立

`deploy/compose.yaml`把`claude-shared:/home/claude`同时挂给`project-a`与`project-b`。两个项目的会话JSONL因此写在同一个卷里，即使把P0-1接上，也无法把usage归属到A还是B。详见`design-deviations.md`。

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

- `ccwadmin`只有`init-project`：没有CDK轮换/禁用、删除项目、查用量、清理tombstone等命令
- 门户只有`/usage`单页，认证复用CDK session token，没有独立管理员登录（spec §11决定是localhost+SSH隧道，Caddy已按此对公网404，与设计一致）
- CLI同步循环每2秒重新Dial一次WebSocket，无本地fsnotify去抖

## 建议推进顺序

1. 定`claude-shared`的去留（P0-2）——它决定计量能不能做，是其他工作的前提
2. 接线用量采集（P0-1）：worker起collector goroutine + `usage_events`/`usage_offsets`的PG实现 + 权重配置化
3. `modeFor`查额度（P1-1）：改动量小，但直接影响两条验收
4. `openat2`路径解析（P1-2）：Linux生产必需
5. 补CI与digest固定，然后在真实VPS上把e2e十条skip逐条填实（P1-3）
