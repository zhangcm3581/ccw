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

---

## ⑤ 回滚

```bash
cd /opt/ccw
git log --oneline -10             # 找要回退到的提交
git checkout <commit>             # 或用打的 tag
cd deploy && docker compose build && docker compose up -d
```

数据卷不受回滚影响；只有当回滚跨越了不兼容的数据库迁移时才需额外处理（本项目目前只有一个初始迁移，无此风险）。
