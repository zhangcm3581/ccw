# 双项目远程Claude工作空间审计与修订说明

**日期：**2026-07-19  
**状态：**实现前必须阅读  
**适用范围：**`remote-claude-workspace`个人双项目版本  
**优先级：**本文件与原设计稿或执行计划冲突时，以本文件为准

## 1. 当前真实状态

截至本次审计，`D:\remote-claude-workspace`只有以下两个文档：

- `docs/superpowers/specs/2026-07-19-remote-claude-workspace-design.md`
- `docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md`

当前没有Git仓库、Go源码、`go.mod`、数据库迁移、Docker配置或可运行测试；执行计划中的61个检查项均未完成。因此准确状态是：

> 已完成设计和计划初稿，尚未开始代码实现。

原设计稿把代码位置写为`/root/code1/remote-claude-workspace/`，但当前机器没有可访问的WSL/Linux仓库。以后若在远端Linux实现，必须把真实源码目录作为审计对象，不能用本地两份文档代表实现结果。

## 2. 已确定、可以保留的设计

以下方向正确，可以继续使用：

1. 系统只供同一个账号所有者本人使用，不向第三方销售或共享访问权。
2. Project A和Project B分别使用一张CDK，一张CDK只能解析到一个项目。
3. A/B分别拥有独立容器、workspace、Claude HOME、tmux、同步索引、日志和内部额度账本。
4. 管理员在两个容器中分别完成官方Claude Code登录；系统不复制、不下发、不展示OAuth凭据。
5. 本地CLI、控制面、Worker、项目容器和PostgreSQL分层是合理的。
6. tmux用于断线后的会话保持；持久卷用于容器重建后的数据保持。
7. 文件同步必须校验相对路径、SHA-256并使用临时文件加原子重命名。
8. 门户必须把“内部项目额度”与“上游账号真实用量”明确区分。
9. JSONL只采集用量字段，不保存用户提示、Claude回复或文件内容。
10. 真实验收必须包含A超额B仍可用、断线重连、容器重建、冲突文件和秘密不泄漏。

## 3. 有记录但仍需重新验证的事项

设计稿记录了以下实测结论，但当前仓库没有对应原始样例或自动化证据。在写核心代码前必须重新生成脱敏证据并纳入测试：

1. Claude Code JSONL中的`requestId`、时间、模型及四类token字段结构。
2. 同一`requestId`是否可能出现多条记录，以及哪一条代表最终用量。
3. 两个独立Claude HOME使用同一个账号分别登录后，refresh token是否会互相失效。
4. Claude Code升级后JSONL字段是否保持兼容。
5. tmux与目标容器镜像中的实际TTY行为。

其中第3项是架构前置条件：先让两个测试容器同时登录并连续运行24小时，各自定时发起正常请求。若任一登录被另一方刷新操作踢下线，则个人双项目版本必须降级为分时使用，或者改用官方API/企业接入方式。

## 4. 修订后的运行时与tmux方案

### 4.1 容器生命周期

原计划把前台`tmux new-session`直接作为容器启动命令，但容器启动时没有TTY，存在立即退出风险。修订为：

- 容器PID 1使用`tini`加受控守护进程，或最小`sleep infinity`；
- Worker确认容器运行后，再通过Docker API执行tmux命令；
- 不把tmux客户端进程作为容器生命周期；
- 容器重建时重新挂载原来的三个持久卷。

Worker的会话准备流程固定为：

```text
docker exec <container> tmux -L <project-id> has-session -t main
如果不存在：
docker exec <container> tmux -L <project-id> new-session -d -s main -c /workspace claude
终端附着：
docker exec -it <container> tmux -L <project-id> attach-session -t main
```

Go端使用PTY启动`docker exec -it`，关闭本地PTY时只结束`docker exec`附着进程，不执行`tmux kill-session`。

### 4.2 VPS重启

VPS重启后tmux内存会话必然丢失，不能声称原进程继续存在。可恢复的是：

- workspace文件；
- Claude HOME和会话JSONL；
- 同步索引；
- 数据库中的会话元数据。

重启后Worker重新创建tmux，并按项目策略执行`claude --continue`；首次没有历史时回退到普通`claude`。该行为必须做真实集成测试。

## 5. 修订后的网络与令牌方案

### 5.1 唯一公网入口

公网只开放HTTPS 443，由Caddy或Nginx统一终止TLS：

```text
/api/*          → control-api
/ws/terminal    → worker-agent终端端点
/ws/sync        → worker-agent同步端点
/portal/*       → control-api管理页面
```

worker-agent只监听`127.0.0.1`、Unix socket或内网地址，不直接暴露公网`ws://agent:8081`。

### 5.2 CDK查询

不能在每次失败登录时遍历全部Argon2哈希。CDK格式改为：

```text
ccw_<public-id>.<random-secret>
```

数据库用`public-id`做O(1)查询，再对`random-secret`执行Argon2id验证。认证端点必须有IP和public-id双维度限速，所有失败统一返回`invalid_cdk`。

### 5.3 连接令牌

- session token：15分钟，只含project ID、audience、签发时间和过期时间；
- terminal/sync token：2分钟，只用于建立一次对应类型连接；
- WebSocket令牌放在`Authorization`请求头或首次认证帧中，禁止放URL查询参数；
- CLI在内存中保留本次输入的CDK，session token过期时自动重新exchange；
- CDK、session token和连接令牌永远不进入日志。

### 5.4 控制面状态

不能使用进程内普通map保存已登录项目。session claims中的project ID必须通过`GetProjectByID`从PostgreSQL读取。这样control-api并发安全，重启后未过期会话也不会失效。

## 6. 修订后的文件同步状态机

原计划的目录扫描没有生成revision，文件删除后tombstone也不会继续出现在清单中，因此不能可靠判断冲突。修订为服务器分配revision：

### 6.1 服务端记录

每个项目、每条路径保存：

```text
path
server_revision
sha256
size_bytes
deleted
updated_by_device
updated_at
```

客户端本地索引保存上一次服务端确认的`server_revision`。

### 6.2 上传规则

客户端上传：

```text
path + base_revision + declared_size + sha256 + content
```

服务端在项目级事务/锁中处理：

1. 读取当前`server_revision`；
2. `base_revision`不等于当前值时拒绝覆盖并返回conflict；
3. 限制实际读取字节数，不能信任`declared_size`；
4. 写项目内临时文件并计算真实大小和SHA-256；
5. 校验通过后原子替换；
6. `server_revision + 1`并更新索引；
7. 返回新revision。

### 6.3 云端Claude直接修改

Worker必须监控云端workspace。发现Claude新增、修改或删除文件时：

- 等待500ms静默窗口；
- 前后两次哈希一致才入账；
- 与file index比较；
- 为变更分配新的server revision；
- 删除写入持久tombstone。

服务端Manifest必须来自file index，并包含未过保留期的tombstone，不能只扫描当前仍存在的文件。

### 6.4 冲突

发生revision冲突时双方文件都保留。默认把远端版本保存为：

```text
<name>.conflict-remote-<UTC时间>
```

系统不得静默选择“revision更大的一端”覆盖另一端。

## 7. 修订后的磁盘配额

磁盘保护分两层：

1. 应用层逻辑用量：门户展示`SUM(size_bytes WHERE deleted=false)`；
2. 文件系统硬配额：防止Claude进程绕过同步接口直接写爆workspace。

硬配额优先使用XFS project quota、ZFS dataset quota或独立受限文件系统。普通Docker命名卷本身不能被当作可靠的逐卷硬配额。

同步上传必须在同一个项目级锁/数据库事务中完成“预留空间→接收并核对真实大小→提交索引或释放预留”。两个并发上传不能各自读取旧用量后同时通过。

即使项目已满，也必须允许：

- 下载文件；
- 删除文件；
- 把文件缩小；
- 查看用量。

因此磁盘超限时不能简单拒绝签发所有sync token，而应签发只读/清理权限的sync token。

## 8. 修订后的JSONL用量采集

### 8.1 数据来源

Worker不能依赖`/var/lib/docker/volumes/.../_data`内部路径。推荐顺序：

1. 给Worker或专用采集helper只读挂载对应Claude HOME卷；
2. 或通过受控Docker exec读取；
3. 不直接假设Docker daemon的磁盘布局。

### 8.2 增量读取

每个JSONL文件保存：

```text
project_id + file_identity + path + committed_offset + partial_line
```

- 只在读取到完整换行后推进`committed_offset`；
- 文件末尾半行保存在`partial_line`，下一轮拼接；
- 文件截断或轮转时重新识别file identity；
- Scanner错误和超长行必须记录指标，不能静默丢弃；
- Worker重启后从持久化offset恢复；找不到offset时从头重扫，依靠幂等写入去重。

### 8.3 requestId去重

唯一键使用`(project_id, source_event_id)`。不能简单保留同一requestId的第一条记录；应验证Claude JSONL语义，并保存最终/最大token计数，防止先出现中间记录时永久少计。

采集器只解析时间、模型和usage字段，禁止把提示、回复内容写入数据库或日志。

## 9. 修订后的额度语义与执行

### 9.1 可以保证什么

系统可以保证：

- A/B分别有内部5小时和7天上限；
- A达到内部上限后，系统不再允许A继续提交新操作；
- A的内部账本不会扣减B的内部账本；
- B在自己的内部额度内仍可连接。

### 9.2 不能保证什么

一个Claude Max账号的官方额度仍然是账号级。没有获授权的逐项目官方接口时，内部加权单位无法精确等同于官方百分比，因此不能保证Anthropic真正为B保留了某个精确百分比。

如果必须获得精确项目预算，应改用支持请求级计量的官方API/企业网关，或使用两个独立上游账号。Max订阅模式只能做保守估算和安全余量。

### 9.3 真正的超额动作

仅仅“不再签发新连接令牌”不够，因为已经连接的终端仍能继续输入。Worker需要每30秒和每次用量事件入库后重新检查：

1. 项目超额时关闭该项目所有终端输入连接；
2. 允许当前响应在短暂宽限期内收尾；
3. 宽限期后仍在持续调用时向Claude进程发送SIGINT；
4. 保留tmux、workspace和Claude HOME；
5. 同步切换为下载/删除/缩小模式；
6. 窗口恢复后允许重新连接。

这种方式最多允许最后一个请求产生少量超额，门户必须说明存在一个请求的计量延迟。

项目级闸门要分别计算5小时和7天；整体池保护也必须同时考虑5小时和7天。原计划只有`PoolFiveHour`，不足以保护周额度。

## 10. 数据库和并发修订

1. 使用正式迁移工具或`schema_migrations`表；每个迁移只能执行一次，不能在每次启动时重新执行全部`CREATE TABLE`。
2. 不复制两份migration文件，避免根目录和embed目录漂移。
3. `file_index`更新必须比较期望revision，防止旧请求覆盖新状态。
4. 项目配额检查与空间预留使用行锁或advisory lock。
5. sessions表增加`UNIQUE(project_id, tmux_name)`或明确支持多会话。
6. control-api和worker-agent启动时必须Ping数据库；启动失败要返回非零退出码。
7. HTTP服务设置读取、写入、空闲和header超时，不能直接忽略`ListenAndServe`错误。

## 11. 门户与管理入口

- 普通浏览器不能方便地给SSR页面手动添加Bearer header，因此门户使用独立的管理员登录cookie，或只监听localhost并通过SSH隧道访问；
- 管理socket必须设置所有者、组和`0660`权限，并检查调用者身份；
- CDK会话只能查看自己的项目，不具备管理员能力；
- 管理端可以同时查看A/B，但不得通过普通CDK路由暴露；
- 所有状态值注明采样时间和是否为估算。

## 12. 安全与运维修订

1. worker-agent拥有Docker控制权时等同宿主机高权限，不能描述成普通低权限服务；优先使用rootless Docker或隔离的本地特权helper，且Worker端口不对公网开放。
2. WebSocket设置最大消息/文件大小、读写deadline、ping/pong和连接数限制。
3. 同步拒绝绝对路径、`..`、NUL、设备文件、FIFO和越界符号链接；Linux实现优先使用`openat2`的`RESOLVE_BENEATH/NO_SYMLINKS`避免TOCTOU竞态。
4. 默认敏感文件规则至少覆盖：`.env*`、`.ssh/`、`.aws/`、`.azure/`、`.kube/`、`.claude/`、`.npmrc`、`.pypirc`、`.netrc`、`.git-credentials`、私钥和常见云凭据文件。
5. Docker镜像固定Claude Code版本；升级前先用脱敏JSONL样例跑兼容测试。
6. 数据库和卷备份必须加密并复制到VPS之外；卷备份前要暂停写入或使用一致性快照，不能一边写一边直接tar。
7. 删除项目及卷、轮换密钥、真实登录和外部发布都必须使用独立管理员操作并记录审计日志。

## 13. 修订后的实施顺序

### Phase 0：仓库与规则

- 在真实开发目录初始化Git；
- 创建`AGENTS.md`、README、Go模块和CI；
- 把本文件设为实现前置依据；
- 删除计划中的临时scratchpad安装路径和破坏性Go安装命令。

### Phase 1：架构阻断验证

- 两容器独立Claude HOME同时登录24小时；
- 最小容器+tmux+PTY断线重连原型；
- 保存脱敏JSONL样例并确认requestId最终记录语义。

上述任一项失败，先修改设计，不进入完整开发。

### Phase 2：单项目垂直切片

- CDK交换；
- 数据库项目查询；
- 通过统一TLS入口连接Worker；
- 启动/附着tmux；
- 断线后重新exchange并恢复终端。

### Phase 3：可靠同步

- 服务端revision和tombstone；
- 云端workspace watcher；
- 上传字节上限与SHA校验；
- 冲突副本；
- 本地索引持久化。

### Phase 4：第二项目与隔离

- Project B容器和卷；
- A/B路径、Claude HOME、tmux和CDK隔离测试；
- 容器重建和VPS重启恢复。

### Phase 5：用量与额度执行

- 可靠JSONL tail；
- 幂等用量事件；
- 5h/7d内部账本；
- A超额时真正关闭A输入、B保持可用；
- 整体5h/7d安全余量。

### Phase 6：门户、备份与发布验收

- 管理员门户；
- 硬盘逻辑用量和文件系统硬配额；
- 加密异机备份与恢复演练；
- 全量e2e、race test、重启和秘密扫描。

## 14. 实现开始条件

只有同时满足以下条件才开始完整编码：

- 设计状态由“待用户审阅”改为“已批准”；
- 确认真实仓库路径和目标Linux环境；
- 双Claude HOME登录24小时验证通过或接受分时降级；
- tmux容器原型通过断开/重连测试；
- JSONL脱敏样例进入测试目录；
- 同步revision/tombstone协议按本文件修订；
- 确认内部额度只是估算，不宣传为官方精确额度。

## 15. 最终验收补充

除原验收标准外，必须增加：

1. control-api重启后，未过期会话仍能解析项目；
2. 两个并发上传不能突破项目硬盘限额；
3. 客户端伪报文件大小不能写爆临时目录；
4. JSONL末尾半行在补全后必须被准确采集；
5. 同一requestId多条记录按确认后的最终规则计量；
6. session token过期后CLI自动重新exchange；
7. 代理日志、服务日志和错误信息中没有任何令牌；
8. A超额时已有终端也不能继续提交新请求；
9. A超额或磁盘满时仍能下载、删除和缩小文件；
10. 服务端直接修改和删除文件都产生revision/tombstone并同步到本地；
11. worker-agent不暴露公网，所有客户端流量通过WSS/TLS；
12. 备份恢复到空服务器后A/B项目、数据库和Claude HOME均可恢复。

