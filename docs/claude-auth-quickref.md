# 授权 Claude 账号（速查）

纳管只把栈起起来，**Claude Code 还没有上游账号的凭据**，需要上机登录一次。
这一步在 `DEPLOY.md` 是 A7，后台目前代劳不了。

**整台节点只做一次。**同节点的全部项目容器共用一个 `claude-shared` 卷，
在任意一个项目容器里登录，其余项目自动共用同一份凭据与同一个额度池。

> 后台的**节点详情页**有一张「授权 Claude 账号」卡片，命令里的 project id
> 已按该节点填好，可以直接复制——比照着本文手敲少一步。本文用于后台不可达时。

## 1. 检查是否已登录

```bash
docker exec ccw-<slug> claude auth status
```

输出 `loggedIn: true` 就已经好了，下面不用做。

## 2. 登录

```bash
ssh root@<节点IP>
cd /srv/ccw/deploy

# 取 project id（tmux 的 socket 名要用它）
docker compose -p ccw run --rm --entrypoint /ccwadmin control-api list-projects

# 起会话并附着；<pid> 换成上一步输出里对应项目的 id
docker exec ccw-<slug> tmux -L <pid> new-session -d -s main -c /workspace claude
docker exec -it ccw-<slug> tmux -L <pid> attach-session -t main
```

在附着的终端里跟着提示走完官方登录，然后按 **Ctrl-b** 松开再按 **d** 脱离。

**不要按 Ctrl-c**——那会把会话里的 claude 杀掉。

## 3. 验证

```bash
docker exec ccw-<slug-a> claude auth status   # loggedIn: true
docker exec ccw-<slug-b> claude auth status   # loggedIn: true（共用凭据，无需再登）
```

## 几个约定与坑

- **容器名恒为 `ccw-<slug>`**（`internal/deploy/compose.go` 的 I5 契约，有测试守着）
- **tmux socket 名必须是 project id**：worker-agent 给客户端接会话时用的就是这个名字。
  用别的名字登录，客户端连上来看到的不是同一个会话
- **工作目录是容器内的 `/workspace`**，由 `new-session -c /workspace` 指定
- **不要 `docker exec -it ... bash` 进去直接跑 `claude`**：那样起的会话不在 tmux 里，
  断线即丢，也不是客户端会附着的那一个
- **`-it` 必须带**：容器侧的 TTY 靠它，缺了 tmux 附着不上

## 登录后仍是 `loggedIn: false`

多半是**卷权限**：容器以 `claude`(UID 1001) 运行，若 `claude-shared` 卷归 `root:root`，
凭据写不进去，表现为登录完还是未登录、反复要求重新登录。

```bash
docker exec ccw-<slug> ls -la /home/claude/.claude/.credentials.json /home/claude/.claude.json
# 应归 claude 所有
```

修复要点：用最新 `deploy/Dockerfile.claude` **重新构建镜像**，并用**全新的空卷**
（空命名卷首次挂载会继承镜像内该路径的所有权；旧的 root:root 卷要先删）。
完整排查见 `docs/admin-login-runbook.md`。

## 多人共用同一账号

运行手册与 `docs/design-deviations.md` 的 D6 都写明：多人共用一个 Claude 账号
在上游服务条款下是否被允许，**本仓库不作判断，由部署者自行核实**。

同一节点的全部项目共用这一个账号的额度，所以内部额度闸门是唯一能阻止某个项目
吃光全机额度的机制——而它目前**只接线未校准**，限额设得很宽，实际拦不住人
（见 `docs/STATUS.md` 的第 1、2 条）。把项目分给别人之前先知道这一点。
