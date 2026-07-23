# 管理员Claude登录运行手册

**适用：**双项目远程Claude工作空间（Project A/Project B）
**前提：**两个项目容器运行中（PID 1为`sleep infinity`），且已用带权限修复的镜像重建（见下方"前置：卷权限"）
**授权模式：**共享授权——两个项目容器共用同一个 Claude HOME 卷（`claude-shared`），**只需登录一次**，两个项目共用同一份凭据（本人自用自己账号，非转售第三方）

## 前置：卷权限（否则登录写不进凭据）

Claude 授权后，账户资料写 `/home/claude/.claude.json`、访问令牌写 `/home/claude/.claude/.credentials.json`。容器以 `claude`(UID 1001) 运行，若挂载卷归 `root:root`，`claude` 无法写入凭据 → 登录后 `loggedIn=false`、反复要求登录。

修复已在 `deploy/Dockerfile.claude` 落实：镜像预建 `/home/claude`、`/workspace`、`/var/lib/cclaude-sync` 并 `chown` 给 claude；**空命名卷首次挂载会继承镜像内该路径的所有权**，从而卷归 claude、可写。因此务必用最新 Dockerfile **重新构建镜像**并**用全新的空卷**（旧的 root:root 卷需先删除）。

## 1. 登录一次（共享授权）

只需在任一个容器里登录一次，凭据写入共享卷 `claude-shared`，两个项目立即共用：

```bash
# 1) 确认容器在跑
docker ps --filter name=ccw-project-a

# 2) 准备 tmux 会话（不存在才创建；PID 1 不是 tmux）
docker exec ccw-project-a tmux -L <project-a-id> has-session -t main \
  || docker exec ccw-project-a tmux -L <project-a-id> new-session -d -s main -c /workspace claude

# 3) 附着并完成官方登录（必须带 -t 分配容器 TTY）
docker exec -it ccw-project-a tmux -L <project-a-id> attach-session -t main
```

在附着的终端里按 Claude Code 提示完成登录。**project-b 无需再登录**——它挂的是同一个 `claude-shared` 卷，凭据已共享。

## 2. 验证登录持久化

```bash
# 凭据文件应已生成，且归 claude(1001) 所有
docker exec ccw-project-a ls -la /home/claude/.claude/.credentials.json /home/claude/.claude.json

# Claude 自身状态应为已登录
docker exec ccw-project-a claude auth status    # 期望 loggedIn: true

# project-b 共用同一份凭据，同样应已登录
docker exec ccw-project-b claude auth status    # 期望 loggedIn: true

# 容器重建后仍保持登录（卷持久）
docker compose up -d --force-recreate project-a
docker exec ccw-project-a claude auth status    # 仍应 loggedIn: true
```

> 卷名提示：docker compose 会给卷加项目名前缀，实际卷名通常是 `deploy_claude-shared`（在 `deploy/` 目录部署时）。用 `docker volume ls` 查实际名称。

```bash
# 秘密泄漏扫描（应无输出）
journalctl -u control-api -u worker-agent | grep -iE 'ccw_[0-9a-f]{16}|oauth|refresh_token|access_token'
```

## 3. 24小时双登录阻断验证（Task 0 Step 1）

**这一步消耗真实Claude额度，必须先获得账号所有者明确同意再执行。**

目的：确认同一账号在两个独立Claude HOME中分别登录后，refresh token不会互相失效。

步骤：

1. 按第1节完成A、B两个容器的登录；
2. 每小时在两个容器中各发起一次正常请求（可用`claude -p "ping"`）；
3. 连续运行24小时；
4. 记录每次请求的成功/失败与时间点。

判定：

- **通过**——两边24小时内均未被要求重新登录：可进入双项目并行阶段；
- **失败**——任一方被踢下线：本方案降级为**分时使用**（同一时间只保持一个项目登录），或改用两个独立上游账号／官方API接入。无论哪种，都必须更新设计文档的额度与并发章节。

结果写入`docs/phase1-evidence/dual-login-24h.md`，包含：起止时间、Claude Code版本、每小时请求结果表、最终判定。

## 4. 边界提醒

- 本手册的所有操作都属于管理员通道，不得暴露给CDK持有者；
- CDK只能附着已经准备好的Claude会话，不能进入登录管理入口；
- 容器可随时删除重建，登录凭据随Claude HOME持久卷保留；只有显式"删除项目并删卷"才会清除。
