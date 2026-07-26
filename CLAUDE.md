# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

`ccw`（remote-claude-workspace）是**远程Claude Code工作空间**：一台Linux VPS上跑多个隔离的Claude Code容器（单节点上限3个），本地用`cclaude` CLI凭CDK登录，附着云端tmux会话（断线不中断），本地目录与云端workspace双向同步，并对每个项目施加磁盘配额与5小时/7天内部额度闸门。

**使用形态（2026-07-26定）：**一台节点只授权一次Claude账号，该节点上的全部项目共用这一个上游账号的额度；项目可以全归管理员自己，也可以分配给他人使用。**因此内部额度闸门不是锦上添花而是必需品**——它是唯一能阻止某个项目吃光全机额度的机制，而它当前空转（见「当前已知缺口」第1条）。此前文档写的"仅供本人两个项目使用"已作废，见`docs/design-deviations.md`的D6。

个人版、单管理员。**非目标**：多租户（多个互不信任的付费客户共用一套控制面与计费）、LLM Gateway计费代理、分块增量同步、Kubernetes、浏览器终端。

机队管理（多节点纳管、Provisioning流水线、域名分配）由Console负责，见`docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md`——它**不引入多租户**，仍是单管理员、每个节点独立自治。单节点上限为**3个项目容器**、单项目磁盘配额上限与默认值均为**15 GiB**（该设计§7.6，产品规则，不可由代码或配置绕过）。`deploy/compose.yaml`当前仍是硬编码的双项目，模板化见`docs/superpowers/plans/2026-07-26-compose-render-plan.md`。

## 权威文档链

改动前先读，冲突时**靠前者优先**：

1. `docs/superpowers/specs/2026-07-19-remote-claude-workspace-v2-review-adjustments.md`（最高权威）
2. `docs/superpowers/specs/2026-07-19-remote-claude-workspace-audit-corrections.md`
3. `docs/superpowers/specs/2026-07-19-remote-claude-workspace-design.md`（Design Spec v3，已批准）
4. `docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md`（v2 Console层设计，未实施；含产品硬性上限§7.6）
5. `docs/superpowers/plans/2026-07-26-compose-render-plan.md`（C13 compose渲染，未实施）

`docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md`（Task 0–13）已于2026-07-26**归档**——保留为历史记录，**不再作为实施依据**，其中Task 13等内容已被明确推翻。文件顶部有归档横幅。

计划里的checkbox**全部未勾**，不能当进度用——实际进度看`docs/STATUS.md`与git log。代码与spec的已知偏离记录在`docs/design-deviations.md`，**已明确决定不做的事**记在`docs/STATUS.md`的「已知取舍」一节（别把它们重新提为待办）；改动涉及这些点时必须先读。

## 常用命令

```bash
go build ./...                                    # 编译三个二进制 + ccwadmin
go test ./...                                     # 全部单测（无外部依赖，秒级）
go test -race ./...                               # 提交前跑一次
go test ./internal/sync -run TestSafeRelPath -v   # 跑单个测试
gofmt -l .                                        # 格式检查（应无输出）
```

部署与运维在`DEPLOY.md`（全新VPS一键部署）与`UPDATE.md`（更新已部署实例）。本机跑服务需要PostgreSQL与`CCW_TOKEN_KEY`等环境变量，见`deploy/.env.example`；`config.Load`对缺失变量一律硬失败，没有默认值可依赖。

## 架构

三个长驻进程 + 一个管理CLI，源码在`cmd/`，逻辑全在`internal/`：

| 二进制 | 监听 | 职责 |
|---|---|---|
| `cclaude` | — | 本地CLI（Win/mac/Linux）：exchange CDK → 后台同步 + 前台终端 |
| `control-api` | `127.0.0.1:8080` | CDK验证、签发短期令牌、额度查询、`/usage`门户、跑数据库迁移 |
| `worker-agent` | `127.0.0.1:8081` | Docker编排、WS终端、WS同步、额度主动执行；**唯一持docker.sock的进程** |
| `ccwadmin` | — | `init-project <slug>`：建项目并打印一次性CDK |

唯一公网入口是Caddy的443。**公开路径→后端路径是合同**（`deploy/Caddyfile`与spec §3必须同步改，否则客户端连不上）：

```
/api/*        → control-api  剥前缀   /api/v1/auth/exchange → /v1/auth/exchange
/ws/terminal  → worker-agent rewrite  /v1/terminal
/ws/sync      → worker-agent rewrite  /v1/sync
/portal, /usage                        不经公网，Caddy直接404；门户走SSH隧道访问localhost
```

### 关键数据流

**登录：**CLI提交CDK → `control-api`按`public_id`做O(1)查库、Argon2id验`secret` → 签15分钟session token（HMAC，不落库）→ CLI用它请求`/v1/connection`，拿到2分钟的terminal/sync连接令牌 + 当前额度快照。

**终端：**CLI用连接令牌连`/ws/terminal` → worker `docker exec` 到项目容器：先`tmux has-session`，不存在才`new-session -d`，再`attach-session`附着；宿主机侧TTY由`creack/pty`提供，容器侧TTY靠`docker exec -it`的`-t`，**两者缺一不可**。断开只关PTY与WebSocket，绝不`kill-session`。

**同步：**客户端每2秒连一次`/ws/sync`，拉服务端manifest → 与本地`.cclaude/index.json`基线做**三方判断**（`base_revision` + `base_sha256` + `current_sha256` + 本地状态）→ 上传走CAS（`base_revision`不匹配即conflict，绝不静默覆盖，冲突时生成`.conflict-remote-<UTC>`副本）。服务端在`HandleManifest`时顺带`reconcileCloud`，把容器内Claude直接改的文件扫进`file_index`并分配新revision。

**额度：**`internal/usage`解析Claude HOME里的会话JSONL → `usage_events`（唯一键`(project_id, source_event_id)`去重）→ `quota.Service`同时算项目5h/7d与账号级池的双窗口安全余量 → 超额时worker的30秒循环关闭该项目全部终端连接，同步降级为cleanup（只许下载/删除/缩小）。**注意：采集器目前未接线**，见`docs/STATUS.md`。

## 不可违反的规则

这些来自spec与审查，违反会破坏安全模型或验收标准：

- **令牌只走`Authorization`头或首个认证帧。全仓禁止出现`?token=`。**
- CDK明文、session token、连接令牌、OAuth凭据**永远不进日志与错误信息**；数据库只存Argon2id哈希。
- CDK认证失败一律返回`invalid_cdk`，不区分"不存在/已禁用/已过期"。
- 连接令牌是**2分钟短期令牌，不是单用途**：无状态HMAC保证不了只用一次，因此有效期内允许重连，worker**每次接受连接时实时复查额度**，不能只信令牌。
- 服务默认只监听回环地址；`worker-agent`持有docker.sock等同宿主机root，绝不对公网暴露。
- 同步路径一律UTF-8 forward-slash相对路径；拒绝绝对路径、任何`..`段、NUL。`internal/sync/paths.go`的排除名单是安全边界的一部分（`.env*`/`.ssh/`/`.aws/`/`.claude/`等凭据文件必须排除）。
- 所有时间窗口用数据库`now()`与UTC。
- 迁移只有`internal/store/migrations/`一份源（embed），靠`schema_migrations`表保证只执行一次；**禁止在仓库别处复制第二份**。
- 用量对外一律称"内部额度单位"，**不得**标注为官方订阅百分比——内部计量只是估算，spec §10列明了系统保证与不保证的边界，措辞不得越界。
- 版本固定：禁止`@latest`或未固定版本安装；版本记录在`deploy/versions.lock`。
- **诚实表述**：未验证的功能不写成"已完成"；e2e里没实现的断言用`t.Skip`而不是空过——避免把"没验证"误报成"通过"。

## 代码约定

- TDD：先写失败测试再实现，`go test ./...`全绿才提交。Docker与数据库逻辑用假API/内存实现做单测，真实集成放`tests/e2e`（无Docker自动skip）。
- 注释用中文，习惯在关键决策处标注依据（如"审查§2.3"、"审计§4.1"），改动这些代码时保留并更新引用。
- 中文文档遵守中英文之间无空格的排版；**不要**对`docs/superpowers/plans/`下的文件跑CJK空格清理脚本，会破坏命令语法。
- Commit message用conventional commits（`feat(sync):`、`fix(terminal):`），正文中文。

## 当前已知缺口

完整清单在`docs/STATUS.md`。最需要注意的三条：

1. `internal/usage`的采集器写完了但**没有任何文件import它**，`usage_events`永远为空 → 所有额度闸门实际不触发。
2. `cmd/worker-agent/main.go`的`modeFor`恒返回`"rw"`，超额时不降级为cleanup —— `control-api`已经会签发cleanup模式令牌，worker不据此限制写入。
3. **文件系统硬配额已决定不做**（`docs/STATUS.md`的T1）：`deploy/quota-setup.sh`创建的卷名与compose实际使用的不是同一个，执行了也不生效且无报错——**不要执行它，也不要把它接进任何流程**。后果是容器内直接写盘可撑爆宿主机磁盘，这是明示的取舍。

会话JSONL已于`4093e3d`按项目分卷（`<slug>-claude-projects`），凭据仍共享以保证「一台机器只授权一次」；这是对spec §6的偏离，详见`docs/design-deviations.md`的D1。**该改动尚未在真实部署上验证。**
