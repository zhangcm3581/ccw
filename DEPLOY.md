# 部署文档：Ubuntu 24 + Docker Compose一键部署

面向运营方管理员。本文档在一台全新Ubuntu 24.04VPS上，用Docker Compose一键起全部服务，创建项目与CDK，供客户端CLI登录并附着云端终端。

> **当前范围说明**
> 本编排提供：CDK认证、连接令牌、额度门户、**云端终端通道**（tmux会话保持、断线重连）。
> **文件双向同步（`/v1/sync`）属于Task 12，尚未实现**，本次部署不含该功能。
> worker-agent挂载docker.sock，等同宿主机高权限，因此**只在内部网络运行、不对公网暴露**。

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

## 7. 管理员登录Claude（两个容器各一次）

项目容器以 `sleep infinity` 运行，Claude Code已安装但未登录。管理员分别进入每个容器完成官方登录（详见 `docs/admin-login-runbook.md`）：

```bash
# 准备并附着project-a的tmux会话（PROJECT_A_ID为第6步输出的project id）
docker exec ccw-project-a tmux -L "$PROJECT_A_ID" has-session -t main \
  || docker exec ccw-project-a tmux -L "$PROJECT_A_ID" new-session -d -s main -c /workspace claude
docker exec -it ccw-project-a tmux -L "$PROJECT_A_ID" attach-session -t main
# 在附着的终端里按Claude Code提示完成登录，然后Ctrl-b d脱离
```

project-b重复同样步骤，容器名换成 `ccw-project-b`、id换成project-b的。

凭据只落在各自的 `*-claude` 持久卷，容器重建不丢失。

> **24小时双登录验证**（`docs/admin-login-runbook.md` 第3节）：两个容器同时保持登录并各自定时请求24小时，确认同账号双登录不互相踢下线。**该验证消耗真实Claude额度，需账号所有者同意后再做。**若失败，降级为分时使用或改用两个独立账号。

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

> 文件同步（本地目录↔云端workspace）在本版本尚未启用，客户端目前仅提供终端通道。

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

VPS重启后 `docker compose up -d`（或依赖 `restart: unless-stopped` 自动拉起）。tmux内存会话会丢失，worker-agent重新准备会话时对已登录项目执行 `claude --continue` 恢复上下文（该行为的真实验证属Task 12）。

## 11. 尚未包含（Task 12/13，需在VPS上继续）

- **文件双向同步**：`/v1/sync` WebSocket端点、客户端同步循环的真实传输、云端workspace watcher
- **文件系统硬配额**：每项目固定大小loop文件系统（防Claude绕过同步接口写满宿主机）
- **额度主动执行**：超额时关闭已连接终端输入
- **完整备份/恢复演练**：加密异机备份与空服务器恢复
- **反向代理路径合同与e2e自动化验收**

这些的设计与实施步骤见 `docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md` 的Task 12、13。

## 12. 安全要点

- `.env`（含令牌密钥、数据库密码）不得提交版本库、不得进日志
- worker-agent挂docker.sock等同宿主机root，务必只在内部网络、不映射公网端口（本compose已如此）
- 项目容器非root运行、不挂docker.sock、卷互相隔离
- CDK明文只在创建时显示一次；库中只存Argon2id哈希
- 门户 `/usage` 仅供管理员经SSH隧道访问，不经公网（Caddy未暴露该路由）
