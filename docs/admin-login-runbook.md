# 管理员Claude登录运行手册

**适用：**远程Claude工作空间的节点（当前编排为 Project A/Project B，单节点上限3个项目）
**前提：**项目容器运行中（PID 1为`sleep infinity`），且已用带权限修复的镜像重建（见下方"前置：卷权限"）
**授权模式：**共享授权——**同一节点上的全部项目容器**共用一个 Claude HOME 卷（`claude-shared`），**整台节点只需登录一次**，全部项目共用同一份凭据与同一个上游额度池。会话JSONL已按项目分卷（`<slug>-claude-projects`），凭据不受影响。

> 项目可以全归管理员自己，也可以分配给他人使用（2026-07-26定）。多人共用同一个Claude账号在上游服务条款下是否被允许，本仓库不作判断，由部署者自行核实——见`docs/design-deviations.md`的D6。

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

## 2.1 关于凭据自动刷新的冲突（共享授权模式）

OAuth 的 access token 几小时过期后会用 refresh token 换新的。两个容器共用同一份凭据文件时，是否冲突取决于 Claude 的 refresh token 是否"轮换"：

- **refresh token 不轮换（固定）**：两容器各换各的 access token，互不影响，永不冲突。
- **refresh token 轮换（用一次即换新、旧的作废）**：存在一个**窄窗口竞态**——A 刷新后把新 refresh 写回、旧的作废；若 B 恰好同时拿旧 refresh 去刷新会失败，可能掉线。

Claude 具体是否轮换需在本环境实测。现实中风险较小：竞态窗口极窄（需两容器恰好同秒刷新）、多数实现有旧 token 宽限期；且真撞上了，**重新登录一次两个项目一起恢复**（凭据共享）。

日常检查与恢复：

```bash
docker exec ccw-project-a claude auth status
docker exec ccw-project-b claude auth status
# 若任一变 loggedIn:false，按第1节重新登录一次即可（俩都恢复）
```

需要**零冲突**（两项目长期同时高频使用）时，唯一彻底的办法是改用**两个独立 Claude 账号**（各自独立凭据、各自刷新），但那就失去"共享一份授权"、且需两份订阅。自用双项目场景推荐先用共享方案，实测几乎不会真撞上。

## 3. 24小时并发稳定性验证（Task 0 Step 1的替代形态，**尚未执行**）

**这一步消耗真实Claude额度，必须先获得账号所有者明确同意再执行。**

> **与原计划的差异：**Design Spec §6与实施计划Task 0 Step 1要求验证的是"同一账号在**两个独立**Claude HOME中分别登录后互不失效"。当前部署改为**共享同一个Claude HOME卷**（见`docs/design-deviations.md` D1），原验证目标已不适用；真正需要验证的风险变成了第2.1节的**共享凭据并发刷新竞态**。原风险没有被证伪，只是被换掉了——两种形态的验证**都还没有做过**。

目的：确认两个容器长期共用同一份凭据并发使用时，不会因refresh token轮换而掉线。

步骤：

1. 按第1节完成登录（只需一次）；
2. 每小时在**两个容器中各**发起一次正常请求（可用`claude -p "ping"`），尽量让两次调用时间接近，以放大竞态窗口；
3. 连续运行24小时；
4. 每次记录成功/失败、时间点，以及`claude auth status`的`loggedIn`。

判定：

- **通过**——24小时内两个容器均未被要求重新登录：可进入双项目长期并行；
- **失败**——出现掉线：说明refresh token会轮换且无足够宽限期。选项为（a）降级为分时使用，同一时间只让一个项目活跃；（b）改用两个独立Claude账号（各自独立凭据，代价是两份订阅）；（c）回到每项目独立Claude HOME并改为验证原始的双登录风险。无论选哪个，都必须同步更新`docs/design-deviations.md`与设计spec的额度与并发章节。

结果写入`docs/phase1-evidence/dual-login-24h.md`，包含：起止时间、Claude Code版本、每小时请求结果表、最终判定。**在该文件产生之前，不要在任何文档里把双项目并发描述为"已验证"。**

## 4. 边界提醒

- 本手册的所有操作都属于管理员通道，不得暴露给CDK持有者；
- CDK只能附着已经准备好的Claude会话，不能进入登录管理入口；
- 容器可随时删除重建，登录凭据随Claude HOME持久卷保留；只有显式"删除项目并删卷"才会清除。
