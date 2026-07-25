# 部署文档：Ubuntu 24 + Docker Compose一键部署

面向运营方管理员。本文档在一台全新Ubuntu 24.04VPS上，用Docker Compose一键起全部服务，创建项目与CDK，供客户端CLI登录并附着云端终端。

> **当前范围说明**（最后核对：2026-07-25）
> 本编排提供：CDK认证、连接令牌、额度门户、**云端终端通道**（tmux会话保持、断线重连）、**文件双向同步**（`/ws/sync`，已接线可用）。
> **不含**：用量采集（额度闸门因此不会真正触发）、文件系统硬配额、备份恢复。见第11节。
> worker-agent挂载docker.sock，等同宿主机高权限，因此**只在内部网络运行、不对公网暴露**。

> **重装用户看这里**：如果你之前部署过（尤其遇到"登录后反复要求登录"），先按 **第 0 节卸载**清掉旧的坏卷，再从第 3 节走一遍。授权模式已改为**共享授权、登录一次**（第 7 节）。

---

## 0. 卸载旧部署（重装前必做）

早期部署的 Claude 卷是 `root:root`、`claude` 用户写不进凭据，必须清掉重来。在 `deploy/` 目录执行：

```bash
cd /opt/ccw/deploy
./uninstall.sh                 # 停服务 + 删所有数据卷（含旧的坏卷）
# 或者彻底一点，连镜像也删（重装会重新 build）：
# ./uninstall.sh --purge-images
```

脚本做的事：`docker compose down -v` 删掉容器/网络/卷，并额外清理早期版本遗留的独立 `project-a-claude / project-b-claude` 卷。跑完会打印残留检查，正常应显示"无残留卷"。

> ⚠ 这会删除数据库、已登录凭据、项目 workspace——重装本就要全新开始，属预期。若只想停服务保留数据，用 `docker compose down`（不加 `-v`）。

卸载后，重新获取最新代码（含权限修复的 `Dockerfile.claude` 与共享授权的 `compose.yaml`），然后从第 3 节继续。

---

## 1. 硬件与前置

- Ubuntu 24.04LTS，x86_64
- 建议8 vCPU/16GB/150–200GB SSD（最低4 vCPU/8GB/100GB）
- 一个解析到本机公网IP的域名（Caddy自动签发HTTPS证书用）。无域名的快速验证见第9节。
- root或具备sudo的账号

## 2. 安装Docker与Compose插件

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

## 3. 获取代码

把项目目录（含本文件与 `deploy/`）放到服务器，例如 `/opt/ccw`：

```bash
sudo mkdir -p /opt/ccw && sudo chown "$USER" /opt/ccw
# 用scp/rsync/git把项目内容传到/opt/ccw
cd /opt/ccw
ls deploy/compose.yaml   # 确认存在
```

## 4. 配置环境变量

```bash
cd /opt/ccw/deploy
cp .env.example .env
# 生成令牌签名密钥（64位hex）
sed -i "s|^CCW_TOKEN_KEY=.*|CCW_TOKEN_KEY=$(openssl rand -hex 32)|" .env
# 设置强数据库密码
sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 16)|" .env
# 填写你的域名
sed -i "s|^CCW_DOMAIN=.*|CCW_DOMAIN=你的域名.example.com|" .env
# 可选：固定Claude Code版本
# sed -i "s|^CLAUDE_CODE_VERSION=.*|CLAUDE_CODE_VERSION=1.2.3|" .env
cat .env    # 核对
```

## 5. 构建镜像并启动

所有compose命令在 `deploy/` 目录执行：

```bash
cd /opt/ccw/deploy
docker compose build          # 构建control-api/worker-agent/项目容器镜像
docker compose up -d          # 后台启动全部服务
docker compose ps             # 应看到postgres/control-api/worker-agent/caddy/project-a/project-b均Up
```

control-api启动时自动执行数据库迁移（`schema_migrations` 保证只执行一次）。

查看日志确认无错误：

```bash
docker compose logs control-api worker-agent caddy --tail=50
```

对外只有Caddy的80/443端口；control-api、worker-agent不映射宿主机端口，仅在内部 `ccw` 网络中被Caddy访问。

## 6. 创建项目并签发CDK

用 `ccwadmin`（打包在control-api镜像内）创建两个项目，各得一张一次性CDK：

```bash
# init-project <slug> [disk_gib] [five_hour_units] [seven_day_units]
docker compose run --rm --entrypoint /ccwadmin control-api init-project project-a 20 1000000 10000000
docker compose run --rm --entrypoint /ccwadmin control-api init-project project-b 20 1000000 10000000
```

每条命令末尾会打印形如 `ccw_<public>.<secret>` 的CDK——**只显示一次，立即保存**。project-a的CDK只能连project-a容器，project-b同理。

> 容器名约定：`ccwadmin init-project project-a` 建立的项目container_name为 `ccw-project-a`，与compose中的项目容器一一对应。

## 7. 管理员登录Claude（共享授权，只需一次）

两个项目容器共用同一个 Claude HOME 卷（`claude-shared`），所以**只在一个容器登录一次**，两个项目共用同一份凭据（本人自用自己账号）。详见 `docs/admin-login-runbook.md`。

```bash
# 准备并附着 project-a 的 tmux 会话（PROJECT_A_ID 为第6步输出的 project id）
docker exec ccw-project-a tmux -L "$PROJECT_A_ID" has-session -t main \
  || docker exec ccw-project-a tmux -L "$PROJECT_A_ID" new-session -d -s main -c /workspace claude
docker exec -it ccw-project-a tmux -L "$PROJECT_A_ID" attach-session -t main
# 按提示完成登录，然后 Ctrl-b d 脱离。project-b 无需再登录（共用同一凭据卷）。

# 验证：两个项目都应已登录，且容器重建后仍保持
docker exec ccw-project-a claude auth status    # 期望 loggedIn: true
docker exec ccw-project-b claude auth status    # 期望 loggedIn: true（共用凭据）
```

凭据（`.claude.json` 与 `.claude/.credentials.json`）持久化在 `claude-shared` 卷，容器重建不丢。

> **重要**：登录能持久化的前提是卷可写。镜像已在 `Dockerfile.claude` 里预建挂载目录并 chown 给 claude(1001)，空卷首次挂载会继承该所有权。**若你之前用旧镜像部署过、卷是 root:root，必须先删旧卷再重建**（见下方"升级已部署实例"）。

### 升级已部署实例（修复登录不持久）

若已按旧版部署、遇到"登录后反复要求登录"，按此修复：

```bash
cd /opt/ccw/deploy
docker compose down                       # 停服务（不加 -v，先保留数据库）
# 删除权限错误的旧 Claude 卷（workspace/数据库不受影响）
docker volume rm deploy_project-a-claude deploy_project-b-claude 2>/dev/null || true
docker compose build --no-cache project-a # 用带权限修复的 Dockerfile 重建镜像
docker compose up -d                      # 全新空卷 claude-shared 会继承 claude 所有权
# 然后回到本节顶部，登录一次即可
```

## 8. 客户端使用（CLI）

在客户端机器上准备 `cclaude` 二进制（在开发机交叉编译）：

```bash
# Linux
GOOS=linux   GOARCH=amd64 go build -o cclaude       ./cmd/cclaude
# macOS (Apple Silicon)
GOOS=darwin  GOARCH=arm64 go build -o cclaude-macos ./cmd/cclaude
# Windows
GOOS=windows GOARCH=amd64 go build -o cclaude.exe   ./cmd/cclaude
```

进入本地项目目录，用CDK登录并附着云端终端：

```bash
cd ~/my-project-a
export CCW_API=https://你的域名.example.com/api
./cclaude          # 提示输入CDK（不回显），登录后显示状态栏并附着云端Claude终端
```

状态栏形如 `[project-a] 5h:10/1000000 7d:60/10000000 disk:0/21474836480 mode:rw`。断网会自动重连；session过期会用内存中的CDK自动重新换取。

> 文件同步已启用：CLI 会把**当前目录**与云端 `/workspace` 双向同步（每2秒一轮）。首次运行前请确认 `cd` 到了正确的项目目录——同步以运行目录为根。凭据类文件（`.env*`、`.ssh/`、`.aws/`、`.claude/` 等）与 `.git/`、`node_modules/` 等目录默认排除，名单见 `internal/sync/paths.go`。本地基线索引存在目录下的 `.cclaude/index.json`，建议加进项目的 `.gitignore`。

## 9. 用服务器 IP 测试（无域名，纯 HTTP）

域名只用于 Caddy 自动签发 HTTPS 证书。没有域名时，用服务器公网 IP + HTTP 即可完整测试（认证、连接、终端）。**明文不加密，仅限测试，切勿用于生产。**

在 `deploy/` 目录执行（把 `<IP>` 换成服务器公网 IP）：

```bash
cd /opt/ccw/deploy

# 1) 配 .env：CCW_DOMAIN 填服务器公网 IP，并生成真实密钥
sed -i "s|^CCW_DOMAIN=.*|CCW_DOMAIN=<IP>|" .env
sed -i "s|^CCW_TOKEN_KEY=.*|CCW_TOKEN_KEY=$(openssl rand -hex 32)|" .env
sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 16)|" .env

# 2) 无 TLS：control-api 返回的终端地址改用 ws:// 而非 wss://
sed -i 's|wss://\${CCW_DOMAIN}/ws|ws://\${CCW_DOMAIN}/ws|' compose.yaml

# 3) Caddy 改用 HTTP 版（监听 80，不签证书）
cp Caddyfile.http Caddyfile

# 4) 起服务（确保云厂商安全组/防火墙放行 80 端口）
docker compose up -d
docker compose ps
```

创建项目、拿 CDK（第 6 节），然后验证：

```bash
# 认证端点（任意机器）
curl -s http://<IP>/api/v1/auth/exchange \
  -H 'Content-Type: application/json' -d '{"cdk":"<你的CDK>"}'
# 成功返回 {"session_token":"...","project_id":"...","project_slug":"project-a"}

# 客户端连终端
export CCW_API=http://<IP>/api
./cclaude
```

上线时改回域名版：`cp` 回原 `Caddyfile`、把 `compose.yaml` 的 `ws://` 改回 `wss://`、`CCW_DOMAIN` 填真实域名并让 DNS 指向本机。

## 10. 运维常用命令

```bash
docker compose ps                       # 服务状态
docker compose logs -f control-api      # 跟踪日志
docker compose restart worker-agent     # 重启单个服务
docker compose down                     # 停止（保留数据卷）
docker compose down -v                  # 停止并删除所有卷（销毁数据，慎用）

# 秘密泄漏自查（应无输出）
docker compose logs | grep -iE 'ccw_[0-9a-f]{16}\.|oauth|refresh_token|access_token'

# 数据库备份（一致性）
docker compose exec postgres pg_dump -U ccw ccw | gzip > ccw-$(date +%Y%m%d).sql.gz
```

VPS重启后 `docker compose up -d`（或依赖 `restart: unless-stopped` 自动拉起）。tmux内存会话会丢失，worker-agent重新准备会话时对已登录项目执行 `claude --continue` 恢复上下文（该行为尚未做过真实验证，见 `docs/STATUS.md`）。

## 11. 尚未包含（部署前请确认你能接受）

本编排**已包含**：CDK认证与令牌、云端终端（tmux保持+断线重连）、文件双向同步（`/ws/sync`，含冲突副本与逻辑磁盘配额）、`/usage`门户。

**尚未包含：**

- **用量采集**：`internal/usage` 的JSONL采集器没有接进 worker-agent，`usage_events` 表始终为空。后果是 **5小时/7天额度闸门不会真正触发**——门户与CLI状态栏的用量恒显示0，超额也不会关闭终端。部署后请自行留意实际消耗。
- **文件系统硬配额**：`deploy/quota-setup.sh` 已提供（每项目一个固定大小的 ext4 loop 文件系统），但**本编排默认不调用它**，直接用普通命名卷。若不手工执行该脚本，容器内的 Claude 可以绕过同步接口把宿主机磁盘写满。用法见第11.1节。
- **cleanup模式的服务端降级**：超额时 control-api 会签发 cleanup 模式的同步令牌，但 worker-agent 目前不据此限制写入。
- **完整备份/恢复演练**：无脚本、无演练记录。第10节只有数据库的 `pg_dump`，卷数据未覆盖。
- **反向代理路径合同与e2e自动化验收**：`tests/e2e` 的断言全部是 skip 状态。

完整缺口清单与建议推进顺序见 `docs/STATUS.md`；与设计spec的偏离见 `docs/design-deviations.md`。

### 11.1 启用文件系统硬配额（可选但强烈建议）

必须在 `docker compose up` **之前**执行，否则命名卷已按默认方式创建，需要先删卷：

```bash
sudo ./quota-setup.sh project-a 20     # 给 project-a 的 workspace 封顶 20GiB
sudo ./quota-setup.sh project-b 20
```

脚本幂等：建 `/srv/ccw/quota/<slug>.img` → `mkfs.ext4` → 挂到 `/srv/ccw/workspaces/<slug>` → 写 `/etc/fstab` 保证重启自动挂回 → 用 `--opt device=` 把该目录注册成 `<slug>-workspace` 命名卷。之后 compose 复用这个卷，容器内写满只会撑爆自己那个 loop 文件系统，不影响另一个项目与宿主机系统盘。

## 12. 安全要点

- `.env`（含令牌密钥、数据库密码）不得提交版本库、不得进日志
- worker-agent挂docker.sock等同宿主机root，务必只在内部网络、不映射公网端口（本compose已如此）
- 项目容器非root运行、不挂docker.sock、卷互相隔离
- CDK明文只在创建时显示一次；库中只存Argon2id哈希
- 门户 `/usage` 仅供管理员经SSH隧道访问，不经公网（Caddy未暴露该路由）
