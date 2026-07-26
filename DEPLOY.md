# 部署文档

从零把 ccw 跑起来。两部分互相独立，按需要读：

| 部分 | 部署什么 | 什么时候需要 |
|---|---|---|
| **[A · 节点](#a-节点)** | 提供工作空间的机器：终端、同步、额度闸门 | **必需**。一台节点最多 3 个项目 |
| **[B · Console](#b-console)** | 官网、客户端下载、管理后台、纳管流水线 | 可选。只有一台节点、愿意用 SSH 管理时可以完全不装 |
| **[C · 运维](#c-运维)** | 常用命令、备份、安全要点、已知边界 | 部署后必读 |

**最后核对：**2026-07-26，对照分支 `v2`。

---

## 先读这一页

**这套系统给你什么：**本地 `cclaude` 凭 CDK 登录，附着云端 tmux 会话（断线不中断、重连即恢复），本地目录与云端 `/workspace` 双向同步，每个项目独立容器与磁盘配额，内部额度闸门按项目计量。

**四条会影响预期的边界，部署前请确认能接受：**

1. **额度闸门未经真机验证，且当前实际不会拦人。** 代码链路是闭环的（用量入库 → 超额关终端 → 同步降级 cleanup），但从未在真实部署上跑过；加权系数处于「先记账、后校准」第一阶段，限额刻意设得很宽。**在你按 [A9](#a9-部署后自查用量采集是否真的在工作) 自查并校准之前，不要依赖它防止某个项目吃光额度。**
2. **磁盘配额是逻辑配额，不是硬配额。** 15 GiB 只统计走同步接口的文件。项目在终端里 `npm install`、堆构建缓存或直接 `dd`，可以突破上限并撑爆宿主机磁盘，撑爆后**同机全部项目一并受影响**。这不需要恶意即可触发。文件系统硬配额**已决定不做**（原因见 [C4](#c4-已知边界与取舍)）。
3. **同节点项目之间不是强隔离边界。** Claude HOME 共享（凭据、命令历史、shell 快照互相可见，只有会话 JSONL 按项目分卷）；`worker-agent` 持有 docker.sock 等同宿主机 root，任一容器逃逸即影响全机。设计上按「使用者可信」处理，**不做 gVisor/Kata 等加固**。
4. **Console 的纳管流水线从未对真实 VPS 走完。** 有单测与本机冒烟，但 12 步没有在真机上完整跑过一次。首次使用请拿一台可以随时重装的机器。

完整缺口清单见 `docs/STATUS.md`；系统结构与数据流见 `docs/diagrams/index.html`（七张图，浏览器直接打开）。

---

# A · 节点

## A1 前置

- **Ubuntu 22.04 / 24.04 或 Debian 12**，x86_64 或 arm64
- **磁盘 ≥ 80 GB**：3 个项目 × 15 GiB workspace = 45 GiB，加系统、镜像、构建缓存与数据库 13–17 GiB，余量约 17 GiB
- 建议 8 vCPU / 16 GB（最低 4 vCPU / 8 GB）
- 一个解析到本机公网 IP 的域名（Caddy 自动签 HTTPS）。没有域名的验证方式见 [A10](#a10-无域名的-ip-测试模式)
- 放行 22 / 80 / 443
- root 或有 sudo 的账号

## A2 安装 Docker

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
docker --version && docker compose version
```

> Debian 12 把上面两处 `ubuntu` 换成 `debian`。

## A3 取代码

```bash
sudo mkdir -p /opt/ccw && sudo chown "$USER" /opt/ccw
cd /opt/ccw
git clone https://github.com/zhangcm3581/ccw.git .
ls deploy/compose.yaml   # 确认存在
```

> **重装的人看这里：**如果之前部署过（尤其遇到过「登录后反复要求登录」），先跑 `deploy/uninstall.sh` 清掉旧卷再继续——详见 `UPDATE.md` 的「历史问题」一节。

## A4 配置环境变量

```bash
cd /opt/ccw/deploy
cp .env.example .env
sed -i "s|^CCW_TOKEN_KEY=.*|CCW_TOKEN_KEY=$(openssl rand -hex 32)|" .env
sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 16)|" .env
sed -i "s|^CCW_DOMAIN=.*|CCW_DOMAIN=你的域名.example.com|" .env
cat .env    # 核对
```

| 变量 | 说明 |
|---|---|
| `CCW_DOMAIN` | 公网域名，Caddy 用它签证书 |
| `CCW_TOKEN_KEY` | 令牌签名密钥，**必须 64 位 hex** |
| `POSTGRES_PASSWORD` | 数据库密码 |
| `CCW_USAGE_WEIGHTS` | 用量加权系数 `input,output,cache_read,cache_write`，默认 `1,5,1,1`。**缺了它 worker-agent 会拒绝启动**——这是有意的，带着空配置跑起来时采集器看着正常但永远采不到东西 |
| `CLAUDE_CODE_VERSION` | 留空＝装最新。生产建议填具体版本 |

> `.env` 含密钥与密码，**不得提交版本库、不得进日志**。

## A5 渲染 compose 并启动

`deploy/compose.yaml` 是 `ccwadmin render-compose` 的产物，仓库里那份是双项目版本。**不要手工编辑**——加项目、改项目列表都走渲染命令（见 `UPDATE.md`）。

按你要的项目列表重新渲染（1–3 个）：

```bash
cd /opt/ccw
go run ./cmd/ccwadmin render-compose --projects project-a,project-b --out deploy/compose.yaml
# 服务器上没装 Go 时，可先用仓库自带的双项目版本，起服务后再用容器里的 ccwadmin 渲染
```

启动：

```bash
cd /opt/ccw/deploy
docker compose build          # 首次要构建 control-api / worker-agent / 项目镜像，需要几分钟
docker compose up -d
docker compose ps             # postgres / control-api / worker-agent / caddy / 各项目容器均 Up
docker compose logs control-api worker-agent caddy --tail=50
```

control-api 启动时自动跑数据库迁移（`schema_migrations` 保证每个迁移只执行一次）。

对外只有 Caddy 的 80/443；control-api 与 worker-agent 不映射宿主机端口，只在内部 `ccw` 网络里被 Caddy 访问。

**公开路径合同**（改 Caddyfile 必须同步改 spec §3，否则客户端连不上）：

| 公开路径 | 后端 | 处理 |
|---|---|---|
| `/api/*` | control-api | 剥前缀：`/api/v1/auth/exchange` → `/v1/auth/exchange` |
| `/ws/terminal` | worker-agent | rewrite → `/v1/terminal` |
| `/ws/sync` | worker-agent | rewrite → `/v1/sync` |
| `/portal`、`/usage` | — | 公网 404；门户走 SSH 隧道访问 localhost |

## A6 创建项目并签发 CDK

`ccwadmin` 打包在 control-api 镜像里。为方便，先起个别名：

```bash
cd /opt/ccw/deploy
alias ccwadmin='docker compose run --rm --entrypoint /ccwadmin control-api'
```

建项目：

```bash
ccwadmin init-project --slug project-a
ccwadmin init-project --slug project-b
```

每条命令末尾打印形如 `ccw_<public>.<secret>` 的 CDK——**只显示一次，立即保存**。一张 CDK 只能连它自己的项目。

**产品硬上限**：单节点最多 3 个项目、单项目磁盘配额上限 15 GiB。第 4 个项目与 `--disk-gib 16` 都会被当场拒绝。默认值：磁盘 15 GiB，5 小时限额 1000000，7 天限额 10000000（先记账阶段的宽值）。

`init-project` 是幂等的：slug 已存在时返回现有信息、不报错、**不签发新 CDK**（要新 CDK 用 `issue-cdk`）。

其他管理命令，全部支持 `--json`：

```bash
ccwadmin list-projects                              # 项目与配额
ccwadmin issue-cdk --slug project-a                 # 补发一张新 CDK
ccwadmin rotate-cdk --slug project-a                # 轮换：旧 CDK 24 小时宽限后自动失效
ccwadmin rotate-cdk --slug project-a --revoke-now   # 凭据泄露应急：旧 CDK 当场失效
ccwadmin list-cdks --slug project-a                 # 各 CDK 状态（无明文，明文不可再取）
ccwadmin disable-cdk --public-id <id>
ccwadmin status                                     # schema 版本 / 磁盘水位 / 每项目用量新鲜度
```

轮换后客户端的表现：旧 CDK 失效时 `cclaude` 收到 `invalid_cdk` 并清除本地缓存的 CDK（保留 API 地址），输入新 CDK 重连即可，**云端 tmux 现场不受影响**。

> `ccwadmin status` 里的 `last_usage_event_at` 是发现「采集停摆」的关键信号：项目明明在用、这个时间却停在几小时前，说明采集链路断了。

## A7 管理员登录 Claude（整台机器只需一次）

同一节点的全部项目容器共用一个 Claude HOME 卷（`claude-shared`），所以**整台节点只在一个容器里登录一次**，全部项目共用同一份凭据与同一个上游额度池。

```bash
# PROJECT_A_ID 是 A6 输出的 project id
docker exec ccw-project-a tmux -L "$PROJECT_A_ID" has-session -t main \
  || docker exec ccw-project-a tmux -L "$PROJECT_A_ID" new-session -d -s main -c /workspace claude
docker exec -it ccw-project-a tmux -L "$PROJECT_A_ID" attach-session -t main
# 按提示完成登录，然后 Ctrl-b d 脱离

# 验证：全部项目都应已登录
docker exec ccw-project-a claude auth status    # 期望 loggedIn: true
docker exec ccw-project-b claude auth status    # 期望 loggedIn: true（共用凭据）
```

凭据（`.claude.json` 与 `.claude/.credentials.json`）持久化在 `claude-shared` 卷，容器重建不丢。详细流程见 `docs/admin-login-runbook.md`。

**为什么会话 JSONL 又是分开的：**`~/.claude/projects/` 被每项目独立卷嵌套挂载遮蔽，而凭据文件是它的兄弟节点、仍在共享卷里。所以「授权一次」与「按项目计量」同时成立——见 `docs/diagrams/06-volumes.svg`。

## A8 客户端接入

在开发机上编译，或从 Console 的下载页取（见 [B4](#b4-发布客户端)）：

```bash
GOOS=linux   GOARCH=amd64 go build -o cclaude       ./cmd/cclaude
GOOS=darwin  GOARCH=arm64 go build -o cclaude-macos ./cmd/cclaude
GOOS=windows GOARCH=amd64 go build -o cclaude.exe   ./cmd/cclaude
```

进入本地项目目录使用：

```bash
cd ~/my-project-a
./cclaude --api https://你的域名.example.com   # 首次指定地址，自动补 /api 前缀并记住
# 提示输入 CDK（不回显）；之后直接 ./cclaude 即可
```

寻址优先级：`--api` > `CCW_API` 环境变量 > `~/.ccw/config.json`（0600）> 交互提示。旧版 `~/.ccw/cdk` 单行文件会自动迁移。`cclaude logout` 清除本地配置。**CDK 不接受命令行参数**（会进 shell history 与 `ps` 输出），只走交互输入或 `CCW_CDK`。

状态栏形如 `[project-a] 5h:10/1000000 7d:60/10000000 disk:0/16106127360 mode:rw`。断网自动重连；session 过期会用内存里的 CDK 自动换取。超额或磁盘满时**不退出**，转 cleanup 模式（仍可下载、删除、缩小），窗口恢复后自动回到正常模式。

**同步的边界，第一次用之前要知道：**

- 同步以**运行目录**为根，先 `cd` 对地方
- 凭据类文件（`.env*`、`.ssh/`、`.aws/`、`.claude/` 等）与 `.git/`、`node_modules/` 等默认排除，名单在 `internal/sync/paths.go`
- **符号链接不参与同步**（两端一律跳过，防止经链接把目录外内容带进清单）
- 本地基线索引写在目录下的 `.cclaude/index.json`，建议加进 `.gitignore`
- 两端同时改同一文件时不覆盖你的本地版本，云端版另存为 `<name>.conflict-remote-<UTC>`

## A9 部署后自查：用量采集是否真的在工作

**这一节别跳过。**采集链路只有单测证据，最危险的失败模式是「采集器在跑、日志正常、`usage_events` 永远为空」——与没接线时的现象完全一样。

**① worker-agent 起来了**

```bash
docker compose logs worker-agent --tail=20
```

看到 `config: CCW_USAGE_ROOT is required` 或 `CCW_USAGE_WEIGHTS ... all zero` 说明 `.env` 缺变量。这是有意的硬失败，照 `.env.example` 补上再起。

**② JSONL 目录确实挂进来了**——最容易漏的一步

```bash
docker compose exec worker-agent ls /srv/ccw/usage/project-a
# 应看到 *.jsonl。项目还没跑过会话时是空目录——空目录正常，"目录不存在"才是问题

docker compose logs worker-agent | grep 'JSONL目录不存在'
# 有输出＝漏挂卷，该项目的用量永远不会被记录
```

**③ 事件真的进库了**——在项目容器里跑一次 Claude 会话，等 30 秒（采集周期），然后：

```bash
docker compose exec postgres psql -U ccw -d ccw -c \
  "SELECT p.slug, count(*), sum(u.weighted_units) FROM usage_events u
     JOIN projects p ON p.id=u.project_id GROUP BY p.slug;"
```

有行且随会话增长＝链路通了。**始终为空＝没通**，回到 ②。

**④ 归属没串**——两个项目各跑一次会话，上面那条 SQL 应看到两行、各自增长。某个项目的数字算到另一个头上，说明卷挂错了。

> **首轮采集会把历史 JSONL 全部入账。**已经跑过一段时间的部署，第一轮扫描会把过去积累的会话一次性写进 `usage_events`，用量数字突然跳高——这是预期行为，那些用量本来就消耗了真实额度。

**校准之前，闸门不会拦人。**权重与限额都是估算起点。跑够一周真实数据后按实际分布调整（改权重是改一个环境变量，改限额是一条 UPDATE），否则闸门会永远停在「看着完成、从未生效」。

## A10 无域名的 IP 测试模式

域名只用于 Caddy 签 HTTPS。没有域名时用公网 IP + HTTP 可以完整验证认证、连接、终端、同步。**明文不加密，仅限测试。**

```bash
cd /opt/ccw/deploy

# 1) .env 里 CCW_DOMAIN 填 IP
sed -i "s|^CCW_DOMAIN=.*|CCW_DOMAIN=<IP>|" .env

# 2) 用 override 文件把终端地址改成 ws://（不要改生成的 compose.yaml）
cat > compose.override.yaml <<'EOF'
services:
  control-api:
    environment:
      CCW_AGENT_WS_BASE: ws://${CCW_DOMAIN}/ws
EOF

# 3) Caddy 改用 HTTP 版
cp Caddyfile.http Caddyfile

# 4) 起服务（确认安全组放行 80）
docker compose up -d && docker compose ps
```

`docker compose` 会自动合并 `compose.override.yaml`——这样重新渲染 compose.yaml 时你的本地改动不会丢。

验证：

```bash
curl -s http://<IP>/api/v1/auth/exchange \
  -H 'Content-Type: application/json' -d '{"cdk":"<你的CDK>"}'
# 成功返回 {"session_token":"...","project_id":"...","project_slug":"project-a"}

./cclaude --api http://<IP>     # 裸 IP 同样自动补 /api
```

**改回生产：**删掉 `compose.override.yaml`、`git checkout deploy/Caddyfile`、`.env` 里填真实域名并让 DNS 指向本机，然后 `docker compose up -d`。

---

# B · Console

Console 是**独立主机、独立数据库**的控制平面，与节点栈完全分开。它**不在用户数据路径上**：Console 停机时，已配置好的 `cclaude` 终端与同步完全不受影响。

**现在能做：**公开站点（落地页、下载页、快速开始）、客户端产物分发与校验和、`/connect` 查询页、管理员登录（密码 + 两步验证、IP 白名单、可立即撤销、审计）、**在浏览器里纳管新节点**。

**还不能做：**DNS 自动化（当前是手动模式）、后台里签发/轮换 CDK、节点巡检与证书预算、解除纳管。**CDK 签发与日常项目管理仍走 [A6](#a6-创建项目并签发-cdk) 的 SSH + `ccwadmin`。**

> **纳管流程从未在真实 VPS 上跑完过。**首次使用请拿一台可以随时重装的机器。

## B1 前置

- 一台独立的 Linux 主机（Ubuntu 22.04/24.04 或 Debian 12），公网 IP，放行 80/443
- **两个主机名**：站点域名与管理域名，**必须不同**。可以是同一域名的两个子域名，也可以是两个不相干的注册域名——取舍见 [B2](#b2-目录与配置)
- 两个的 A 记录都指向本机公网 IP 且**已生效**（先加 DNS 再启动，否则 Caddy 连续签证书失败会撞 Let'"'"'s Encrypt 的「每标识符每小时 5 次失败」限额）
- Docker 与 compose 插件（装法同 [A2](#a2-安装-docker)）

## B2 目录与配置

**① 取代码**——`deploy/console/` 这个目录来自仓库，不需要你手工创建：

```bash
sudo mkdir -p /opt/ccw-console && sudo chown "$USER" /opt/ccw-console
cd /opt/ccw-console
git clone https://github.com/zhangcm3581/ccw.git .
ls deploy/console/compose.yaml    # 确认存在，这就是 Console 栈的编排
```

> `chown` 不能省：`sudo mkdir` 建出的目录属主是 root，普通用户 `git clone` 进去会 Permission denied。

**② 建数据目录**——这些在仓库之外，是数据落盘的地方，与上面的 `deploy/console/` 无关：

```bash
# 都在 Docker data-root 之外，避免磁盘被撑爆时连数据库一起挂
sudo mkdir -p /var/lib/ccw-console/pgdata /var/lib/ccw-console/logs /srv/ccw-console/dist
sudo chown 65532:65532 /var/lib/ccw-console/logs   # 容器内以 nonroot 运行，漏了日志写不进去
```

| 目录 | 存什么 |
|---|---|
| `/opt/ccw-console/` | 代码与编排文件（git clone 来的） |
| `/var/lib/ccw-console/pgdata` | Console 数据库 |
| `/var/lib/ccw-console/logs` | 纳管流水线的运行日志 |
| `/srv/ccw-console/dist` | 客户端产物（B4 发布的二进制） |

**③ 配置**——之后所有 compose 命令都在这个目录执行：

```bash
cd /opt/ccw-console/deploy/console
cp .env.example .env
```

编辑 `.env`：

```bash
CCW_SITE_DOMAIN=ccw.example.com          # 官网：落地页、下载、/connect
CCW_ADMIN_DOMAIN=my-ops-panel.net        # 管理后台
CCW_ADMIN_ALLOWLIST=0.0.0.0/0 ::/0       # 见下方「白名单怎么填」
CCW_SECRET_KEY=<openssl rand -hex 32>
CONSOLE_POSTGRES_PASSWORD=<openssl rand -hex 16>
```

### 两个域名的关系

**必须是不同的主机名**，但**不要求同源**——代码完全不认识这两个变量，只有 Caddy 读，两个独立的站点块。所以两种写法都合法：

| 写法 | 例子 | 特点 |
|---|---|---|
| 同一注册域名的两个子域名 | `ccw.example.com` + `admin.example.com` | 只需一个域名；后台易被子域名枚举猜到 |
| 两个不同的注册域名 | `ccw.example.com` + `my-ops-panel.net` | 挡住子域名枚举；证书限额分开算；节点子域名不会与后台撞名 |

**不能**用同一域名下的路径前缀隔离（`ccw.example.com/admin`）：Caddy 配置写错一处就会把后台暴露给公网，风险不对称。

> 换域名**挡不住证书透明日志**。Caddy 签的每张证书都会进公开的 CT 日志，盯着 crt.sh 的人几分钟内就能看到你的后台主机名——换成不相干的域名也一样。真正拦住人的是白名单，见下。

### 白名单怎么填

`CCW_ADMIN_ALLOWLIST` 是 Caddy 的 `remote_ip` 语法：**必须用空格分隔，不能用逗号**。

> 逗号会让 Caddy 把整串当成一个 CIDR 解析失败、启动失败、无限重启，结果是**两个域名都连不上**。而应用层的解析器空格和逗号都接受，所以启动日志仍会显示「白名单 N 条」——看着正常，实际 Caddy 已经死了。排查时先看 `docker compose ps` 有没有 `Restarting`。

两种取法：

| 取法 | 值 | 未认证者看到 | 剩下的防线 |
|---|---|---|---|
| **限制来源**（推荐） | `203.0.113.7 198.51.100.0/24` | 404，不确认后台存在 | 网络层 + 密码 + TOTP |
| **对外开放** | `0.0.0.0/0 ::/0` | 完整登录页 | 密码 + TOTP + 双维限速 |

出口 IP 固定就填 ①。IP 不固定又想随处登录就填 ②——密码 + 两步验证对登录页是合格姿态，但你把两道门减成了一道，**补偿办法是管理员密码用 20 位以上随机串**（`openssl rand -base64 24`，反正存密码管理器）。

限速与统一错误已经在了，不用配：每 IP 与每用户名各 5 次/分钟，用户名不存在／密码错／验证码错三种情况返回逐字节相同的响应。

> 客户端 IP 取 `X-Forwarded-For` 的**最后一段**（我们自己的 Caddy 追加的真实值），伪造请求头绕不过白名单与限速。前提是恰好一层反代——**在 Console 前面再加 CDN 或 LB 会打破这个前提**，见 `internal/console/server.go` 的 `clientIP`。

### 两个必填的密钥

**`CCW_SECRET_KEY` 与 `CCW_ADMIN_ALLOWLIST` 缺任一项，`/admin/*` 路由根本不注册**——启动日志会说明原因。这是有意的：没有认证与网络层限制就不上管理页面。

`CCW_SECRET_KEY` 加密着全部节点的托管 SSH 私钥与管理员的两步验证密钥。**丢了就要对每台节点重新纳管、重建管理员账号**——存进密码管理器，别只留在服务器上。

## B3 启动

```bash
cd /opt/ccw-console/deploy/console
docker compose build && docker compose up -d
docker compose ps
docker compose logs ccw-console --tail=20
```

启动日志里应看到「管理后台已启用」与「机队管理已启用（源码包 N KiB）」。若看到「机队管理未启用」，日志会写明缺什么。

验证：

```bash
curl -s https://ccw.example.com/healthz                                # ok
curl -sI https://ccw.example.com/ | head -1                            # 200
curl -s -o /dev/null -w '%{http_code}\n' https://admin.example.com/    # 白名单外 404
```

首访下载页会显示「暂无发布」——正常，下一步就是发布客户端。

## B4 发布客户端

**在开发机上**交叉编译六个平台：

```bash
make release VERSION=v0.1.0        # 输出到 dist/，含 SHA256SUMS
```

同步到 Console 主机并登记：

```bash
# 开发机
rsync -av dist/ user@console-host:/srv/ccw-console/dist/

# Console 主机
cd /opt/ccw-console/deploy/console
alias console='docker compose run --rm --entrypoint /ccw-console ccw-console'

console register-release --version v0.1.0 --notes "首个版本"   # 先登记，核对清单与 sha256
console register-release --version v0.1.0 --publish            # 确认无误再发布
```

**未 `--publish` 的版本对下载页完全不可见**，`/dist/<文件>` 也不发——只发已发布版本登记过的文件名。往产物目录里放一半的文件不会被任何人下载到。缺平台时命令会显式警告，不会静默略过。

验证：

```bash
curl -s https://ccw.example.com/dist/SHA256SUMS | head -3
curl -sO https://ccw.example.com/dist/cclaude_v0.1.0_linux_amd64
sha256sum -c SHA256SUMS --ignore-missing      # 期望 OK
```

## B5 创建管理员

```bash
cd /opt/ccw-console/deploy/console
docker compose run --rm -it --entrypoint /ccw-console ccw-console create-admin --username admin
# 交互输入密码两次（至少 12 位，不回显、不进 shell history）
```

输出会打印**只显示一次**的两步验证密钥与 `otpauth://` 链接——立刻添加到认证器 App，随后登录一次确认可用。密钥丢失只能重建账号。

登录：浏览器打开 `https://admin.example.com/admin/login`。

几个刻意的行为，遇到别当成 bug：

- 用户名不存在、密码错、验证码错三种情况提示**完全一样**——避免用错误差异枚举用户名
- IP 白名单之外返回 **404 而不是 403**——不暴露后台是否存在。Caddy 与应用层各校验一遍
- 连续失败按 IP 与用户名两个维度限速（每分钟 5 次）
- 会话 12 小时绝对超时、30 分钟空闲超时；退出登录**立即**作废服务端会话

## B6 在后台纳管一台新节点

「机队管理」→「新增节点」，填三段：连接信息（IP、用户名、SSH 密码）、域名（选或新建 zone，自动分配 `api-01`、`api-02`…）、初始项目 slug（最多 3 个）。

SSH 密码**只用于首次连接**——注入托管 ed25519 密钥并验证成功后立即丢弃，不落库、不进日志。

点「开始部署」跳到实时日志页，流水线依次做：

```
probe（发行版白名单、磁盘核算）→ harden（托管密钥、防火墙）→ install-docker
→ dns-allocate → push-source（推源码包）→ push-artifacts（渲染的 compose.yaml）
→ render-env（密钥在节点本地生成）→ compose-up → cert-wait
→ healthcheck → init-projects → disk-guard
```

**中途一定会停在 dns-allocate**：手动 DNS 模式下，日志打出要添加的 A 记录（形如 `类型 A ｜ 主机记录 api-01.example.com ｜ 记录值 203.0.113.7 ｜ TTL 60`）。到你的 DNS 服务商加完这条记录，回节点详情页点「继续/重新部署」——已完成的步骤跳过，从 dns-allocate 继续。

> DNS 校验交叉查询 1.1.1.1 与 8.8.8.8，两个都指向节点 IP 才算生效。这不是保守：DNS 没生效就起 Caddy 会连续触发 Let's Encrypt 验证失败，撞上「每标识符每小时 5 次失败」后即使 DNS 修好也要再等一小时。

其他会遇到的行为：

- **只支持 Ubuntu 22.04/24.04 与 Debian 12**，其它发行版当场失败不做猜测
- **重跑安全**：同一节点点「继续/重新部署」用同一个 run，已成功的步骤标记 skipped；`.env` 里已有的数据库密码不会被换掉
- **CDK 明文不经 Console 存储**：`init-projects` 签发的 CDK 在节点侧输出，Console 只记 `public_id`。当前需要从节点上取回明文（`ccwadmin issue-cdk --slug xxx`）
- **Console 重启后**「继续部署」会失效：部署参数存在内存里，重启后请重走「新增节点」

## B7 `/connect` 查询页现在还查不到东西

查询页按「CDK 公开 ID → 项目 → 节点 → 域名」解析。纳管流水线已经会写 `nodes` 与 `node_domains`，但 `node_projects` 与 `cdk_issues` 还没有写入方——那属于后台的 CDK 管理，尚未实施。

因此当前 `/connect` 对任何 CDK 仍返回「未找到」。页面本身与后端约束（只收公开 ID、拒绝完整 CDK、限速、统一错误）已就绪。在此之前，签发 CDK 时请直接把 API 域名一并告诉用户。

---

# C · 运维

## C1 常用命令

节点（在 `/opt/ccw/deploy`）：

```bash
docker compose ps                       # 服务状态
docker compose logs -f control-api      # 跟踪日志
docker compose restart worker-agent     # 重启单个服务
docker compose down                     # 停止，保留数据卷
docker compose down -v                  # 停止并删除全部卷（销毁数据，慎用）

# 秘密泄漏自查（应无输出）
docker compose logs | grep -iE 'ccw_[0-9a-f]{16}\.|oauth|refresh_token|access_token'

# 磁盘水位
df -h && docker system df
```

Console（在 `/opt/ccw-console/deploy/console`）：

```bash
docker compose ps
docker compose logs -f ccw-console
ls /var/lib/ccw-console/logs            # 流水线运行日志，每个 run 一个文件
```

VPS 重启后 `docker compose up -d`（或依赖 `restart: unless-stopped` 自动拉起）。tmux 内存会话会丢，worker-agent 重新准备会话时对已登录项目执行 `claude --continue` 恢复上下文——**该行为尚未做过真实验证**。

## C2 备份

```bash
# 节点数据库（项目、CDK、用量、文件索引）
cd /opt/ccw/deploy
docker compose exec -T postgres pg_dump -U ccw ccw | gzip > ccw-$(date +%F).sql.gz

# Console 数据库（机队元数据、发布记录、管理员）
cd /opt/ccw-console/deploy/console
docker compose exec -T postgres pg_dump -U ccw ccw_console | gzip > console-$(date +%F).sql.gz
```

**还必须单独备份的东西：**

| 内容 | 丢了会怎样 |
|---|---|
| 节点 `.env`（`CCW_TOKEN_KEY`） | 全部已签发的令牌失效，客户端要重新登录 |
| Console `.env`（`CCW_SECRET_KEY`） | 全部托管 SSH 私钥与两步验证密钥不可解，每台节点要重新纳管 |
| `claude-shared` 卷 | 要重新登录 Claude 账号 |
| 各项目 workspace 卷 | 客户的文件没了（本地还有，靠同步可恢复） |

> **备份恢复演练没做过。**上面的命令只验证过导出侧，恢复流程未演练。

## C3 安全要点

- `.env` 不得提交版本库、不得进日志
- worker-agent 挂 docker.sock 等同宿主机 root，务必只在内部网络、不映射公网端口（本 compose 已如此）
- 项目容器非 root 运行、不挂 docker.sock、卷互相隔离
- CDK 明文只在创建时显示一次；库中只存 Argon2id 哈希
- 令牌只走 `Authorization` 头，全仓禁止 `?token=`
- `/usage` 门户仅供管理员经 SSH 隧道访问 localhost，Caddy 对公网 404
- Console 的特权动作都写审计日志，**审计写入失败时动作也失败**
- 流水线日志在写盘与推流前都经过脱敏（密码、私钥、CDK 明文、令牌、云厂商 AccessKey）

## C4 已知边界与取舍

**文件系统硬配额：已决定不做。** `deploy/quota-setup.sh` 保留在仓库里但**请勿执行**——它创建的卷名是 `<slug>-workspace`，而 compose 实际使用的卷带项目前缀（`deploy_<slug>-workspace`），两者不是同一个卷。脚本会正常退出并打印 `capped at NN GiB`，容器却仍挂着不受约束的普通命名卷，**没有任何报错**。执行它唯一的效果是让你误以为配额已经生效。

因此逻辑配额只统计走同步接口的文件，容器内直接写盘可以突破上限。**对外只能称「配额」，不得表述为「保证不超过」。**

建议的替代防线（尚未实施）：

1. Docker `data-root` 指向独立分区/盘——撑爆的是该盘，宿主机根分区与 SSH 救援能力不受影响
2. Postgres 数据移出 data-root（`ccw-pg` 现在是普通命名卷，落在 data-root 上，会跟着一起挂）
3. 磁盘水位告警——在撑爆之前收到通知

这三项完成前，请定期人工检查 `df -h` 与 `docker system df`。**注意它们只把后果从「整机死亡且救不回来」降到「项目一起降级、机器能救」，不消除项目之间的互相影响。**

**其他未包含的：**备份恢复演练、反向代理路径合同的自动化测试、`tests/e2e` 的十条断言（全部是 `t.Skip`，没有任何一条真实 VPS 验收跑过）。

完整缺口清单与推进顺序见 `docs/STATUS.md`；与设计 spec 的偏离见 `docs/design-deviations.md`。
