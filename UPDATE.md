# 更新操作文档

代码同步链路：

```
开发环境（git 仓库）──①推送──▶ GitHub(zhangcm3581/ccw) ──②拉取──▶ 服务器(/opt/ccw)
```

当前 GitHub `main` 与开发环境一致（HEAD `6c4f98e`，24 次提交）。

---

## ① 把新更新推到 GitHub

凭据由你掌控，**不放到开发环境**。两种方式二选一：

**方式 A（推荐）：开发环境用 git bundle 交付，你在本地推**
- 开发方（我）产出一个 `ccw-update.bundle`（含新提交）
- 你在本地已 clone 的仓库里：
  ```bash
  git pull                      # 若已 clone 且能访问
  # 或用 bundle：
  git fetch ccw-update.bundle main:updates && git merge updates
  git push origin main
  ```

**方式 B：给开发环境一把 Deploy Key（如果你希望我能直接推）**
- 在 GitHub 仓库 Settings → Deploy keys 加一把带写权限的 key
- 之后开发环境可 `git push origin main` 直接同步
- 好处：省去中转；此 key 只对本仓库有效、可随时撤销

> 现状：GitHub 已有全部代码，日常你只需把「新增的提交」推上去即可。

---

## ② 服务器更新部署（核心，保留数据与登录）

**前提：把服务器 `/opt/ccw` 改成 git 管理一次**（只需做一次）：

```bash
cd /opt/ccw
git init
git remote add origin https://github.com/zhangcm3581/ccw.git
git fetch origin
git reset --hard origin/main      # 对齐 GitHub。.env 是未跟踪文件，不受影响
```

> 若你改过 `deploy/Caddyfile`（如切成 IP/HTTP 测试版），先备份：`cp deploy/Caddyfile deploy/Caddyfile.local`，reset 后再拷回。或直接用 `deploy/Caddyfile.http`（已在仓库）。

> ⚠ **2026-07-26 之后的版本新增了一个必填变量。**`.env` 是未跟踪文件，`git reset` 不会帮你补上，而 **worker-agent 缺了它会直接拒绝启动**（这是有意的，见下）：
>
> ```bash
> cd /opt/ccw/deploy
> grep -q '^CCW_USAGE_WEIGHTS=' .env || echo 'CCW_USAGE_WEIGHTS=1,5,1,1' >> .env
> ```
>
> `CCW_USAGE_ROOT` 已写在 `compose.yaml` 里，不用管。之所以宁可拒绝启动也不给默认值：带着空配置跑起来时，采集器看上去一切正常但 `usage_events` 永远为空，与没接线时的现象完全一样，极难排查。
>
> 本次更新还会执行一个新迁移（`002_account_pool_limits.sql`，给 `accounts` 加两列池上限），由 control-api 启动时自动跑，无需手动操作。

**以后每次更新，三步：**

```bash
cd /opt/ccw
git pull origin main              # 拉最新代码
cd deploy
docker compose build              # 重建镜像（代码/Dockerfile 变了才需要）
docker compose up -d              # 滚动重启
```

**关键：不加 `-v`、不跑 `uninstall.sh`**，所以：

- PostgreSQL 数据（项目、CDK、用量）保留
- `claude-shared` 卷（登录凭据）保留 → **无需重新登录**
- 各项目 workspace 保留

---

## ③ 按改动类型决定要不要 build

| 改了什么 | 操作 |
|---|---|
| 只改文档（*.md） | 无需任何操作 |
| 改 Go 代码 | `docker compose build <服务>` + `up -d` |
| 改 `Dockerfile.claude`（项目容器镜像） | `docker compose build project-a` + `up -d --force-recreate project-a project-b` |
| 改 `compose.yaml` / `Caddyfile` | `docker compose up -d`（会按需重建受影响容器） |
| 数据库 schema 变更（新增 migration） | 无需手动：control-api 启动时自动执行 `schema_migrations`，只跑未执行的 |

镜像若怀疑用了缓存的旧层，加 `--no-cache` 强制重建：`docker compose build --no-cache <服务>`。

---

## ④ 验证更新生效

```bash
docker compose ps                                   # 全部 Up
docker exec ccw-project-a claude auth status        # 仍 loggedIn: true（登录未丢）
git -C /opt/ccw log -1 --format='当前版本: %h %s'   # 确认代码版本
```

**首次升到含用量采集的版本时，额外确认三条**（详细步骤见 `DEPLOY.md` 第11.2节）：

```bash
docker compose logs worker-agent --tail=20 | grep -i 'usage\|config:'   # 无配置报错
docker compose exec worker-agent ls /srv/ccw/usage/project-a            # 目录存在
docker compose exec postgres psql -U ccw -d ccw -c \
  "SELECT count(*) FROM usage_events;"                                  # 跑过会话后应 > 0
```

> **首轮采集会把历史 JSONL 全部入账。**已经运行一段时间的部署，升级后第一轮扫描会把过去积累的会话记录一次性写进 `usage_events`，用量数字会突然跳高。这是预期行为不是 bug——那些用量本来就消耗了真实额度，如实计入更诚实。

---

## ⑤ 加第3个项目（render-compose）

`compose.yaml` 由 `ccwadmin render-compose` 生成，**不要手工编辑**——过去手工加项目要改4处YAML，漏掉worker-agent的挂载会让同步/用量采集**静默失效**（不报错、文件写丢/用量恒为空）。现在是一条命令：

```bash
cd /opt/ccw/deploy
# 1. 渲染新编排（列出全部项目，含已有的；上限3个，第4个会被拒绝）
docker compose run --rm --entrypoint /ccwadmin control-api \
  render-compose --projects project-a,project-b,project-c > compose.yaml.new
head -3 compose.yaml.new && mv compose.yaml.new compose.yaml   # 确认非空再替换

# 2. 应用（会重建worker-agent：在线客户的终端会短暂断开，tmux现场不丢，重连即恢复；
#    避开使用高峰执行）
docker compose up -d

# 3. 建项目并签发CDK
docker compose run --rm --entrypoint /ccwadmin control-api init-project project-c
```

巡检漂移（数据库与compose是否一致）：

```bash
docker compose run --rm --entrypoint /ccwadmin control-api \
  render-compose --check --projects project-a,project-b,project-c
# 退出码非0＝有漂移，输出会说明是哪个slug、漂在哪一侧
```

> 注意：`git pull` 若带来新版 `compose.yaml`（仓库里是双项目的渲染结果），会覆盖你本地渲染的三项目版本——pull之后重跑一次上面的render-compose即可（幂等，同输入同输出）。

---

## ⑥ 回滚

```bash
cd /opt/ccw
git log --oneline -10             # 找要回退到的提交
git checkout <commit>             # 或用打的 tag
cd deploy && docker compose build && docker compose up -d
```

数据卷不受回滚影响；只有当回滚跨越了不兼容的数据库迁移时才需额外处理。目前有两个迁移（`001_initial`、`002_account_pool_limits`），002只是给`accounts`加两列，回滚代码到002之前也不需要动数据库（旧代码不读那两列）。
