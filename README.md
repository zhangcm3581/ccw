# ccw — 远程Claude Code工作空间

一台Linux VPS上跑多个互相隔离的Claude Code容器（**单节点上限3个**）；本地用`cclaude` CLI凭CDK登录，附着云端tmux会话（本地断线不中断、重连即恢复），本地目录与云端workspace双向同步，每个项目有独立的磁盘配额与5小时/7天内部额度闸门。

单管理员、单节点自治。**非目标**：多租户（多个互不信任的付费客户共用一套控制面与计费）、LLM Gateway计费代理、分块增量同步、Kubernetes、浏览器终端。

> **使用形态与边界（如实描述，非合规背书）：**
>
> - **账号**：管理员在容器内用官方流程完成Claude Code登录，**一台节点只授权一次**；该节点上的全部项目共用这一个上游账号的额度。项目可以全归管理员自己，也可以分配给他人使用。
> - **凭据**：系统不复制、不下发、不展示OAuth凭据；凭据只存在于节点上的Docker卷内。
> - **用量**：对外一律称"内部额度单位"，是**估算值**，不等同官方订阅余额。
> - **未作断言**：多人共用同一个Claude账号在上游服务条款下是否被允许，本仓库不作判断，由部署者自行核实。此前版本此处写有"仅供本人两个项目使用、不向第三方转售访问权"，与2026-07-26确定的使用形态不符，已删除——详见`docs/design-deviations.md`的D6。
> - **隔离强度**：同节点的项目之间**不是**强隔离边界，见下方"已知边界"。

## 它是怎么搭起来的

```
本地 cclaude ──HTTPS/WSS 443──▶ Caddy ──┬─▶ control-api   CDK验证/令牌/额度/门户
                                        └─▶ worker-agent  容器编排/终端/同步/用量
                                                  │
                                                  ├─▶ 容器A（tmux: claude）── A的卷
                                                  └─▶ 容器B（tmux: claude）── B的卷   最多3个
                                        PostgreSQL 存元数据
```

- **唯一公网入口**是Caddy的443；control-api与worker-agent只在内网可达（worker持有docker.sock，等同宿主机root，绝不暴露公网）。
- **CDK**格式`ccw_<public-id>.<secret>`，一张CDK只能连一个项目，库里只存Argon2id哈希。
- **令牌**：session token 15分钟，terminal/sync连接令牌2分钟；只走`Authorization`头，禁止URL查询参数。
- **同步**是自研SHA-256清单同步，全文件粒度，服务端分配revision，CAS上传，冲突时双方文件都保留（远端存为`<name>.conflict-remote-<UTC>`），绝不静默覆盖。

## 快速上手

### 部署服务端

全新Ubuntu 24.04 VPS的一键部署走`DEPLOY.md`（Docker Compose，含Caddy自动签TLS）。更新已部署的实例走`UPDATE.md`。简化到最短：

```bash
cd deploy && cp .env.example .env
# 填 CCW_DOMAIN、POSTGRES_PASSWORD，并生成 CCW_TOKEN_KEY=$(openssl rand -hex 32)
docker compose up -d --build
docker compose run --rm --entrypoint /ccwadmin control-api init-project project-a
# 打印的CDK只显示一次，立即保存
```

管理员登录Claude（共享凭据，只需一次）与无域名的IP测试模式见`DEPLOY.md`第7、9节。

### 本地使用

```bash
go build -o cclaude ./cmd/cclaude
cd ~/my-project-a                              # 同步以运行目录为根，先 cd 对
export CCW_API=https://你的域名/api             # 注意带 /api 前缀（Caddy 路径合同）
./cclaude                                      # 提示输入CDK（不回显），缓存在 ~/.ccw/cdk (0600)
```

CLI后台同步当前目录、前台附着云端终端，状态栏形如`[project-a] 5h:10/1000000 7d:60/10000000 disk:0/21474836480 mode:rw`。超额或磁盘满时**不退出**，转为cleanup模式（仍可下载、删除、缩小文件），窗口恢复后自动回到正常模式。凭据类文件与`.git/`、`node_modules/`等默认排除（名单见`internal/sync/paths.go`）；本地基线索引写在目录下的`.cclaude/index.json`。

## 开发

```bash
go build ./...        # cclaude / control-api / worker-agent / ccwadmin
go test ./...         # 全部单测，无外部依赖
go test -race ./...   # 提交前
gofmt -l .            # 应无输出
```

Docker与数据库逻辑用假API/内存实现做单测；需要真实VPS的验收在`tests/e2e`（无Docker自动skip）。

代码布局：`cmd/`是四个二进制入口，逻辑全在`internal/`——`auth`(CDK) `token`(HMAC) `project` `store`(PG+迁移) `runtime`(Docker) `terminal`(tmux+WS) `sync`(清单同步) `storage`(磁盘记账) `usage`(JSONL采集) `quota`(闸门) `control`(HTTP)。

## 文档

| 文件 | 内容 |
|---|---|
| `CLAUDE.md` | 给Claude Code的仓库指南：命令、架构、不可违反的规则 |
| `docs/STATUS.md` | **实施进度与缺口清单的唯一可信来源** |
| `docs/design-deviations.md` | 代码与spec的已知偏离及其代价 |
| `docs/superpowers/specs/` | 设计spec v3与两份审查文档（权威顺序见`CLAUDE.md`） |
| `docs/superpowers/plans/` | compose渲染计划（未实施）；Task 0–13实施计划**已归档**，仅供历史参考 |
| `DEPLOY.md` / `UPDATE.md` | 部署与更新运维 |
| `docs/admin-login-runbook.md` | 管理员在容器内完成Claude登录的流程 |
| `deploy/versions.lock` | 版本固定清单 |

## 当前状态

代码覆盖到实施计划的Task 1–13，单测全绿。完整清单与建议推进顺序见`docs/STATUS.md`。

### 已知边界

会影响功能预期的三条，部署前必读：

- **用量采集器尚未接线**：`usage_events`恒为空，因此**5小时/7天额度闸门实际不触发**，门户与CLI状态栏的用量恒显示0。多个项目共用同一个上游账号时，没有任何机制阻止其中一个吃光全机额度。
- **文件系统硬配额已决定不做**：磁盘配额只统计走同步接口的文件；容器内`npm install`、构建缓存或直接`dd`可以突破上限并撑爆宿主机磁盘，**同节点全部项目一并受影响**。这是明示的取舍，见`docs/STATUS.md`的T1。
- **同节点项目之间不是强隔离边界**：Claude HOME共享（凭据、命令历史、shell快照互相可见，只有会话JSONL按项目分卷）；`worker-agent`持有docker.sock等同宿主机root，任一容器逃逸即影响全机。设计上按"使用者可信"处理，**不做gVisor/Kata等加固**。
