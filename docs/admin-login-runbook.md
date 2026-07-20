# 管理员Claude登录运行手册

**适用：**双项目远程Claude工作空间（Project A/Project B）
**前提：**两个项目容器已由`EnsureProjectRuntime`创建并处于运行状态（PID 1为`sleep infinity`）
**原则：**系统不复制、不下发、不展示OAuth凭据；CDK通道永远没有登录入口

## 1. 进入容器完成官方登录

管理员经**管理socket**（仅监听localhost、权限0660）操作，普通CDK的HTTP API不注册这些路由。

对每个项目分别执行一次（以Project A为例）：

```bash
# 1)确认容器在跑
docker ps --filter name=ccw-project-a

# 2)准备tmux会话（不存在才创建；PID 1不是tmux）
docker exec ccw-project-a tmux -L <project-a-id> has-session -t main \
  || docker exec ccw-project-a tmux -L <project-a-id> new-session -d -s main -c /workspace claude

# 3)附着并完成官方登录流程（必须带-t分配容器TTY）
docker exec -it ccw-project-a tmux -L <project-a-id> attach-session -t main
```

在附着的终端里按Claude Code官方提示完成登录。Project B重复同样步骤，容器名与project-id换成B的。

## 2. 验证凭据隔离

登录后确认凭据只落在各自的Claude HOME卷：

```bash
docker run --rm -v project-a-claude:/m alpine ls -la /m
docker run --rm -v project-b-claude:/m alpine ls -la /m
```

要求：两个卷内容互不相同、互不引用；control-api与worker-agent的日志、数据库中均无OAuth明文。

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
