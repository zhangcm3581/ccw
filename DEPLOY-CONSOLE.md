# Console部署手册（官网 + 下载分发 + CDK查询页）

Console是**独立主机、独立数据库**的控制平面，与节点栈（`DEPLOY.md`）完全分开。它不在用户数据路径上：Console停机时，已配置好的 `cclaude` 终端与同步完全不受影响。

**当前可用范围：**公开站点（落地页、下载页、快速开始）、客户端产物分发与校验和、`/connect` CDK查询页。
**尚未实施：**管理后台（`/admin/*`）、SSH纳管流水线、DNS自动化、审计与巡检——这些是设计文档的C2–C19，本手册会随实施更新。因此**目前仍需按 `DEPLOY.md` 用SSH+`ccwadmin` 管理节点**。

---

## 1. 前置

- 一台独立的Linux主机（Ubuntu 22.04/24.04或Debian 12），公网IP，开放80/443
- 两个域名：站点域名（如 `ccw.example.com`）与管理域名（如 `admin.example.com`）。**必须是不同域名**（设计§8.3：同域名下靠路径隔离，配置写错就把后台暴露给公网）
- 两个域名的A记录都指向本机，且已生效——Caddy要凭它签发证书
- Docker与compose插件（装法同 `DEPLOY.md` 第2节）

## 2. 取代码与配置

```bash
sudo mkdir -p /opt/ccw-console && cd /opt/ccw-console
git clone https://github.com/zhangcm3581/ccw.git .    # 或用你自己的仓库地址

# 数据目录（都在Docker data-root之外，避免磁盘被撑爆时连数据库一起挂）
sudo mkdir -p /var/lib/ccw-console/pgdata /srv/ccw-console/dist

cd deploy/console
cp .env.example .env
```

编辑 `.env`：填两个域名、管理白名单IP（**你自己的出口IP**）、强数据库密码。

## 3. 启动

```bash
cd /opt/ccw-console/deploy/console
docker compose build
docker compose up -d
docker compose ps          # postgres / ccw-console / caddy 均Up
docker compose logs ccw-console --tail=20
```

`ccw-console` 启动时自动跑数据库迁移（`schema_migrations` 保证只执行一次）。

验证：

```bash
curl -s https://ccw.example.com/healthz          # 期望 ok
curl -sI https://ccw.example.com/ | head -1      # 期望 200
curl -s -o /dev/null -w '%{http_code}\n' https://admin.example.com/   # 白名单外期望 404
```

首访 `https://ccw.example.com/download` 会显示「暂无发布」——正常，下一步就是发布客户端。

## 4. 发布客户端（下载页的内容来源）

**在开发机上**交叉编译六个平台的产物：

```bash
make release VERSION=v0.1.0        # 输出到 dist/，含 SHA256SUMS
```

把产物同步到Console主机的产物目录，然后登记入库：

```bash
# 开发机
rsync -av dist/ user@console-host:/srv/ccw-console/dist/

# Console主机
cd /opt/ccw-console/deploy/console
docker compose run --rm --entrypoint /ccw-console ccw-console \
  register-release --version v0.1.0 --notes "首个版本"
# 确认输出的产物清单与sha256无误后再发布：
docker compose run --rm --entrypoint /ccw-console ccw-console \
  register-release --version v0.1.0 --publish
```

**未 `--publish` 的版本对下载页完全不可见**——`/dist/<文件>` 也不发（只发已发布版本登记过的文件名）。这是半成品保护：往产物目录里放一半的文件不会被任何人下载到。

登记时若有平台缺产物，命令会显式警告 `缺少目标 xxx/yyy`，不会静默略过。

验证下载与校验链路：

```bash
curl -s https://ccw.example.com/dist/SHA256SUMS | head -3
curl -sO https://ccw.example.com/dist/cclaude_v0.1.0_linux_amd64
sha256sum -c SHA256SUMS --ignore-missing      # 期望 OK
```

## 5. `/connect` 查询页现在还查不到东西

查询页按 `CDK公开ID → 项目 → 节点 → 域名` 解析，数据来自Console库的 `cdk_issues`/`node_projects`/`nodes`/`node_domains` 四张表。**这些表目前没有写入方**——填充它们的是管理后台的纳管流水线与CDK签发（C11/C17），尚未实施。

因此当前 `/connect` 对任何CDK都返回「未找到」。页面本身与后端约束（只收公开ID、拒绝完整CDK、限速、统一错误）已就绪并有测试，等后台接入数据后即可用。

在此之前，签发CDK时请直接把API域名一并告诉用户（`cclaude --api https://api-01.example.com`）。

## 6. 更新Console

```bash
cd /opt/ccw-console
git pull
cd deploy/console
docker compose build ccw-console
docker compose up -d
```

**禁止 `docker compose down -v`**：`caddy-data` 卷持有已签发的证书，丢卷即重签，会消耗Let's Encrypt「完全相同标识符5张/周」的限额（设计§6.5），撞顶后要等到窗口滚动才能恢复HTTPS。

## 7. 备份

```bash
# 数据库（机队元数据、发布记录）
docker compose exec -T postgres pg_dump -U ccw ccw_console | gzip > console-$(date +%F).sql.gz
```

产物目录 `/srv/ccw-console/dist` 可从开发机重新构建，不必备份。

> **注意：**备份恢复演练（设计验收A33）尚未做过，上述命令只验证过导出侧。恢复流程与演练记录属于C21剩余部分。

## 8. 安全要点

- Console主机的SSH私钥、`.env` 与数据库备份等同于机队的高权限凭据，妥善保管
- 管理域名的IP白名单是第一道闸；后台认证（密码+TOTP、可撤销会话）属C3，**尚未实施**——在它完成之前，`/admin/*` 路由**根本没有注册**，白名单内访问也只会得到404。这是有意的：没有认证就不上任何管理页面
- `/v1/resolve` 只接收CDK的公开前半段；服务端收到含 `.` 的输入直接拒绝且不记录请求体
