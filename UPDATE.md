# 更新文档

已部署实例的日常维护：更新代码、加项目、轮换凭据、回滚、排障。

首次部署看 `DEPLOY.md`。

| 我要做的事 | 去哪一节 |
|---|---|
| 更新节点到最新代码 | [1 更新节点](#1-更新节点) |
| 更新 Console | [2 更新 Console](#2-更新-console) |
| 加一个项目 | [3 加项目](#3-加项目) |
| 轮换/撤销 CDK | [4 轮换与撤销-cdk](#4-轮换与撤销-cdk) |
| 发布新版客户端 | [5 发布新版客户端](#5-发布新版客户端) |
| 出问题要回退 | [6 回滚](#6-回滚) |
| 遇到具体故障 | [7 排障](#7-排障) |

---

## 1 更新节点

### 常规三步

```bash
cd /opt/ccw
git pull origin v2
cd /opt/ccw/deploy
docker compose build          # 代码或 Dockerfile 变了才需要
docker compose up -d          # 滚动重启
```

**关键：不加 `-v`、不跑 `uninstall.sh`**，所以：

- PostgreSQL 数据（项目、CDK、用量、文件索引）保留
- `claude-shared` 卷（登录凭据）保留 → **无需重新登录**
- 各项目 workspace 保留

数据库迁移由 control-api 启动时自动执行，`schema_migrations` 保证每个迁移只跑一次。

### 按改动类型决定要不要 build

| 改了什么 | 操作 |
|---|---|
| 只改文档（`*.md`） | 无需任何操作 |
| 改 Go 代码 | `docker compose build <服务>` + `up -d` |
| 改 `Dockerfile.claude`（项目镜像） | `docker compose build project-a` + `up -d --force-recreate <全部项目容器>` |
| 改 `compose.yaml` / `Caddyfile` | `docker compose up -d`（按需重建受影响容器） |
| 新增数据库迁移 | 无需手动，control-api 启动时自动跑 |

怀疑用了缓存的旧层：`docker compose build --no-cache <服务>`。

### `git pull` 会覆盖 `compose.yaml`

`deploy/compose.yaml` 是 `render-compose` 的产物，仓库里那份是**双项目**版本。如果你的节点跑着别的项目组合，`git pull` 之后要重新渲染一次（幂等，同输入同输出）：

```bash
cd /opt/ccw/deploy
docker compose run --rm --entrypoint /ccwadmin control-api \
  render-compose --projects project-a,project-b,project-c > compose.yaml.new
head -3 compose.yaml.new && mv compose.yaml.new compose.yaml
docker compose up -d
```

用 `--check` 巡检数据库与 compose 是否一致：

```bash
docker compose run --rm --entrypoint /ccwadmin control-api \
  render-compose --check --projects project-a,project-b,project-c
# 退出码非 0 ＝ 有漂移，输出会说明是哪个 slug、漂在哪一侧
```

> 本地对 compose 的改动（比如 IP 测试模式的 `ws://`）应该放在 `compose.override.yaml` 里——`docker compose` 会自动合并，重新渲染时不会丢。

### 更新后验证

```bash
cd /opt/ccw/deploy
docker compose ps                                   # 全部 Up
docker exec ccw-project-a claude auth status        # 仍 loggedIn: true
git -C /opt/ccw log -1 --format='当前版本: %h %s'

# 用量采集仍在工作（详细四步见 DEPLOY.md 的 A9）
docker compose logs worker-agent --tail=20 | grep -i 'config:'   # 无配置报错
docker compose exec worker-agent ls /srv/ccw/usage/project-a     # 目录存在
docker compose exec postgres psql -U ccw -d ccw -c "SELECT count(*) FROM usage_events;"
```

---

## 2 更新 Console

```bash
cd /opt/ccw-console
git pull origin v2
cd /opt/ccw-console/deploy/console
docker compose build ccw-console
docker compose up -d
```

**禁止 `docker compose down -v`**：`caddy-data` 卷持有已签发的证书，丢卷即重签，会消耗 Let's Encrypt「完全相同标识符 5 张/周」的限额，撞顶后要等窗口滚动才能恢复 HTTPS。

更新后确认：

```bash
docker compose logs ccw-console --tail=20
# 应看到「管理后台已启用」与「机队管理已启用（源码包 N KiB）」
curl -s https://ccw.example.com/healthz    # ok
```

> Console 镜像里内置了推给节点的源码包。**更新 Console 后，新纳管的节点会拿到新版本的源码**——已有节点不受影响，它们要单独按第 1 节更新。

---

## 3 加项目

单节点上限 **3 个项目**（产品规则，第 4 个会被拒绝）。

过去加一个项目要手工改 4 处 YAML，漏掉 worker-agent 的挂载会让同步与用量采集**静默失效**（不报错、文件写丢、用量恒为空）。现在是一条命令：

```bash
cd /opt/ccw/deploy

# 1. 渲染新编排（列出全部项目，含已有的）
docker compose run --rm --entrypoint /ccwadmin control-api \
  render-compose --projects project-a,project-b,project-c > compose.yaml.new
head -3 compose.yaml.new && mv compose.yaml.new compose.yaml   # 确认非空再替换

# 2. 应用
docker compose up -d

# 3. 建项目并签发 CDK
docker compose run --rm --entrypoint /ccwadmin control-api init-project --slug project-c
```

**第 2 步会重建 worker-agent**，后果：

- 在线客户的终端 WebSocket 与 PTY 断开
- **但 tmux 会话不丢**（它在项目容器里），客户重连即恢复现场

所以别在使用高峰做。新项目容器起来后无需再登录 Claude——共用 `claude-shared` 卷。

---

## 4 轮换与撤销 CDK

```bash
cd /opt/ccw/deploy
alias ccwadmin='docker compose run --rm --entrypoint /ccwadmin control-api'

ccwadmin list-cdks --slug project-a                 # 先看现状（无明文）
ccwadmin rotate-cdk --slug project-a                # 例行轮换：旧 CDK 24 小时宽限
ccwadmin rotate-cdk --slug project-a --revoke-now   # 泄露应急：旧 CDK 当场失效
ccwadmin disable-cdk --public-id <id>               # 精确禁用某一张
```

- **宽限期靠 `expires_at` 自动生效，不需要任何定时任务**——`ResolveCDK` 每次查询都比对
- 新 CDK 明文**只显示一次**
- 客户端表现：旧 CDK 失效时 `cclaude` 收到 `invalid_cdk`，自动清除本地缓存的 CDK（**保留 API 地址**），输入新 CDK 重连即可，**云端 tmux 现场完好**
- 轮换失败一律返回统一错误，不区分「项目不存在／CDK 不存在／已禁用」

---

## 5 发布新版客户端

只有装了 Console 才需要——否则直接把编译好的二进制发给用户。

```bash
# 开发机
make release VERSION=v0.2.0
rsync -av dist/ user@console-host:/srv/ccw-console/dist/

# Console 主机
cd /opt/ccw-console/deploy/console
alias console='docker compose run --rm --entrypoint /ccw-console ccw-console'
console register-release --version v0.2.0 --notes "..."   # 先登记，核对清单
console register-release --version v0.2.0 --publish        # 确认后发布
```

未 `--publish` 的版本对下载页完全不可见，`/dist/` 也不发。旧版本仍在库里，只是下载页只展示最近发布的那个。

---

## 6 回滚

```bash
cd /opt/ccw
git log --oneline -10
git checkout <commit>          # 或用打的 tag
cd /opt/ccw/deploy && docker compose build && docker compose up -d
```

数据卷不受回滚影响。**跨迁移回滚要看具体迁移：**

| 迁移 | 回滚到它之前 |
|---|---|
| `001_initial` | 全部表的基础，不可回滚 |
| `002_account_pool_limits` | 给 `accounts` 加两列池上限。旧代码不读这两列，**可直接回滚代码，不用动数据库** |
| `003_cdk_created_at` | 给 `cdks` 加 `created_at`（NOT NULL DEFAULT now()）。旧代码不读它，**同样可直接回滚** |

也就是说目前三个迁移都是纯加列，回滚代码不需要改数据库。**将来新增删列或改类型的迁移时，这条要重新评估。**

---

## 7 排障

### 用量采集：链路通不通

最危险的失败模式是「采集器在跑、日志正常、`usage_events` 永远为空」。完整四步自查在 `DEPLOY.md` 的 A9，简版：

```bash
docker compose logs worker-agent | grep 'JSONL目录不存在'   # 有输出＝漏挂卷
docker compose exec worker-agent ls /srv/ccw/usage/project-a  # 目录必须存在
docker compose exec postgres psql -U ccw -d ccw -c \
  "SELECT p.slug, count(*), max(u.occurred_at) FROM usage_events u
     JOIN projects p ON p.id=u.project_id GROUP BY p.slug;"
```

`ccwadmin status --json` 里的 `last_usage_event_at` 也能看：项目在用、这个时间却停在几小时前 ＝ 采集停摆。

> **首轮采集会把历史 JSONL 全部入账**，用量数字突然跳高是预期行为——那些用量本来就消耗了真实额度。

### 登录后反复要求登录（历史问题）

早期版本的 Claude 卷是 `root:root`，容器内的 `claude` 用户写不进 `.credentials.json`。现在的 `Dockerfile.claude` 已预建目录并 chown，但**旧部署留下的坏卷要手动删**：

```bash
cd /opt/ccw/deploy
docker compose down                       # 不加 -v，保留数据库与 workspace
docker volume rm deploy_project-a-claude deploy_project-b-claude 2>/dev/null || true
docker compose build --no-cache project-a
docker compose up -d
# 然后按 DEPLOY.md 的 A7 重新登录一次
```

彻底重来（**会删掉数据库、凭据、workspace**）：

```bash
cd /opt/ccw/deploy && ./uninstall.sh
```

### 客户端连不上

按顺序排除：

```bash
# 1. 域名解析对不对
dig +short 你的域名.example.com

# 2. 证书好没好
curl -sI https://你的域名.example.com/api/v1/connection | head -1   # 401 是正常的（没带令牌）

# 3. 客户端配置
cat ~/.ccw/config.json        # api 字段是否正确（应带 /api 后缀）
cclaude logout                # 清掉重来
cclaude --api https://你的域名.example.com
```

`invalid_cdk` ＝ CDK 被轮换/撤销了，或者输错了。客户端会自动清缓存，重新输入即可。

### 文件没同步过去

- 确认 `cclaude` 是在**正确的目录**里跑的——同步以运行目录为根
- 确认文件没被排除：凭据类文件、`.git/`、`node_modules/` 等默认不同步，名单在 `internal/sync/paths.go`
- **符号链接不同步**（两端一律跳过）——这是安全边界，不是 bug
- 状态栏 `mode:cleanup` ＝ 超额或磁盘满，此时只允许下载/删除/缩小

### 磁盘满

```bash
df -h && docker system df
docker builder prune -f            # 回收构建缓存，通常能拿回 2–5 GiB
docker image prune -f
```

**逻辑配额拦不住容器内直接写盘**（见 `DEPLOY.md` 的 C4）。真撑爆了要进容器清：

```bash
docker exec -it ccw-project-a du -sh /workspace/* | sort -h | tail
```

### Caddy 无限重启 / 域名连不上

`docker compose ps` 里 caddy 是 `Restarting`、PORTS 列为空 ＝ 它根本没起来，80/443 没人监听。**先看它的日志，别急着查 DNS：**

```bash
cd /opt/ccw-console/deploy/console
docker compose logs caddy --tail=50
```

最常见的原因是 `CCW_ADMIN_ALLOWLIST` **用了逗号分隔**——Caddy 的 `remote_ip` 只认空格：

```bash
# 错：CCW_ADMIN_ALLOWLIST=0.0.0.0/0,::/0
# 对：CCW_ADMIN_ALLOWLIST=0.0.0.0/0 ::/0
sed -i 's|^CCW_ADMIN_ALLOWLIST=.*|CCW_ADMIN_ALLOWLIST=0.0.0.0/0 ::/0|' .env
docker compose up -d
```

迷惑之处在于应用层的解析器空格和逗号都接受，所以 `ccw-console` 的启动日志仍会显示「白名单 N 条」，看着一切正常。

排查顺序：`docker compose ps`（有没有 Restarting）→ `docker compose logs <崩溃的容器>` → `curl localhost:8090/healthz`（绕开 Caddy 看后端）→ 最后才是 DNS 与证书。

### 产物传不到 /srv/ccw-console/dist

`Permission denied` 多半是目录属主还是 root（`sudo mkdir` 建的）：

```bash
# Console 主机
sudo chown -R "$USER":"$USER" /srv/ccw-console/dist
```

容器是只读挂载这个目录、以 nonroot 运行，属主给你自己不影响它读。

其余对号入座：`Permission denied (publickey)` ＝ 没带 `.pem`（`-e "ssh -i ~/key.pem"`）；`rsync: command not found` ＝ 远端没装 rsync，改用 `scp -i ~/key.pem dist/* ubuntu@IP:/srv/ccw-console/dist/`；`Connection timed out` ＝ 安全组没放行 22。

### 访问后台域名却看到官网页面

`CCW_ADMIN_DOMAIN` 没传给 `ccw-console` 容器。两个域名的 Caddy 站点块转发到同一个后端进程，应用层要靠这个变量才知道该把 `/admin/*` 限制在哪个域名上；不设置时两个域名的内容完全一样——**后台在官网域名上也是开着的**。

```bash
cd /opt/ccw-console/deploy/console
grep CCW_ADMIN_DOMAIN .env          # 确认 .env 里有
docker compose up -d                # compose.yaml 会把它传进容器
docker compose logs ccw-console --tail=20 | grep -i CCW_ADMIN_DOMAIN   # 有告警说明还是没传进去
```

修好后：管理域名的根路径 302 到 `/admin/login`、官网内容 404；官网域名上的 `/admin/*` 404。

### Console 的机队管理没启用

启动日志会写明原因，常见三种：

- `CCW_SECRET_KEY` 或 `CCW_ADMIN_ALLOWLIST` 没设 → `/admin/*` 路由不注册
- 读不到节点源码包 → 机队管理不启用（`CCW_NODE_SRC` 默认 `/node-src.tar.gz`，由 Console 镜像构建时生成）
- 源码包缺关键文件 → 同上，日志会说缺哪个

### 纳管流水线卡在某一步

打开节点详情页看步骤表：状态是 `failed` 的那一步就是断点。修好外部原因后点「继续/重新部署」，已完成的步骤会跳过。

- **卡在 dns-allocate**：正常，手动 DNS 模式下要你去加 A 记录。日志里有完整的记录内容
- **卡在 harden**：SSH 用户可能不在 sudoers，或密码不对。不会做任何猜测性修复
- **Console 重启过**：内存里的部署参数没了，「继续部署」会拒绝，需要重走「新增节点」

---

## 附：升级到 2026-07-26 之后版本的一次性事项

从更早的版本升上来时，这些是一次性的：

- **`.env` 要补 `CCW_USAGE_WEIGHTS`**（`.env` 是未跟踪文件，`git pull` 不会帮你补，而 **worker-agent 缺了它会直接拒绝启动**）：
  ```bash
  cd /opt/ccw/deploy
  grep -q '^CCW_USAGE_WEIGHTS=' .env || echo 'CCW_USAGE_WEIGHTS=1,5,1,1' >> .env
  ```
  `CCW_USAGE_ROOT` 写在 compose 里，不用管。
- **迁移 `002`、`003` 自动执行**，无需手动操作
- **`compose.yaml` 变成生成产物**：与旧手写文件语义完全相同（`docker compose config` 逐字节比对过），`up -d` 不会因此重建任何容器。今后别手工编辑
- **客户端有行为变化**：不再默认连任何写死域名，首次运行提示输入 API 地址；旧 `~/.ccw/cdk` 自动迁移到 `~/.ccw/config.json`。**需要重新分发客户端二进制**，但旧二进制配旧服务端仍可用，不强制同步升级
- **符号链接不再参与同步**：此前依赖同步符号链接的目录结构会表现为「该文件不同步」，这是预期行为
- **`ccwadmin init-project` 的默认磁盘配额从 20 GiB 改为 15 GiB**，且第 4 个项目、`--disk-gib 16` 会被拒绝
