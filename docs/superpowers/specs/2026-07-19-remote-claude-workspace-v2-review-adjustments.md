# 远程Claude工作空间v2审查与v3调整要求

**日期：**2026-07-19  
**审查对象：**

- `2026-07-19-remote-claude-workspace-design.md`（Design Spec v2）
- `2026-07-19-remote-claude-workspace-plan.md`（Implementation Plan v2）

**审查结论：**Design Spec v2方向基本正确，可作为后续修订基础；Implementation Plan v2仍存在端到端不一致、关键机制缺失和占位实现，当前不能直接全量执行。修订为v3前，只允许进行Phase 1／Task 0架构阻断验证，不应开始Task 1–12的正式编码。

---

## 1. v2已经正确吸收的内容

以下设计应在v3中继续保留：

1. 两个项目使用独立容器、workspace卷、Claude HOME卷、同步状态卷和tmux会话。
2. CDK与项目一对一绑定，CDK不等同于Claude登录凭据。
3. 只有反向代理的443端口对公网开放，worker-agent不直接暴露公网。
4. 同步状态以服务端`file_index`、`server_revision`和持久tombstone为准。
5. 项目磁盘采用“应用层逻辑用量＋文件系统硬配额”两层保护。
6. 用量事件按项目记账，项目分别计算5小时与7天内部额度。
7. 项目超额后需要主动关闭现有终端输入，而不是只拒绝签发新令牌。
8. 超额或磁盘满时仍允许下载、删除和缩小文件。
9. VPS重启后不承诺原tmux进程仍存在，只恢复持久文件、Claude HOME、数据库状态并重建会话。
10. 24小时双Claude HOME登录、tmux容器原型和JSONL真实语义属于编码前阻断验证。

## 2. 编码前必须完成的调整

### 2.1 修复终端TTY命令

实施计划中的终端附着仍使用：

```text
docker exec -i <container> tmux ... attach-session
```

必须改成真正分配容器TTY的形式：

```text
docker exec -it <container> tmux ... attach-session
```

Go端即使使用`creack/pty`启动本机进程，也不能代替Docker的`-t`参数。需要增加真实容器集成测试，验证tmux附着、断开和重连。

### 2.2 统一公网URL与反向代理路径

当前计划同时存在后端`/v1/*`、公网`/api/*`、`/ws/*`和门户`/portal/*`等路径，但没有明确的前缀删除或重写规则，部署后无法保证路由命中。

v3必须固定一套公开合同，例如：

| 公网路径 | 目标服务 | 后端路径 |
|---|---|---|
| `/api/v1/auth/exchange` | control-api | `/v1/auth/exchange` |
| `/api/v1/connection` | control-api | `/v1/connection` |
| `/ws/terminal` | worker-agent | `/v1/terminal` |
| `/ws/sync` | worker-agent | `/v1/sync` |
| `/portal/*` | control-api | 明确的门户路由 |

Caddy配置必须使用明确的`handle_path`、`rewrite`或与后端完全相同的路径，并增加反向代理集成测试。

### 2.3 重新定义“单用途连接令牌”

当前HMAC令牌为无状态令牌，没有`jti`、消费记录或防重放缓存，因此在有效期内可以重复使用。

v3必须选择一种语义：

1. **推荐的个人版方案：**改称“2分钟短期连接令牌”，允许有效期内重连，不再声称单用途；或
2. 增加随机`jti`，worker在数据库或共享缓存中原子消费，已经使用的`jti`再次出现时拒绝连接。

文档、令牌结构、测试和门户说明必须保持一致。

### 2.4 改为真正的三方同步判断

不能继续使用“revision较大的一方自动获胜”。本地只有当前`FileEntry`无法判断文件是未修改的旧版本，还是基于旧版本产生的新修改。

本地索引至少需要保存：

- `base_revision`：上次服务端确认的revision；
- `base_sha256`：上次确认内容的SHA-256；
- `current_sha256`：当前本地文件SHA-256；
- 本地状态：`clean`、`modified`或`deleted`。

正确规则：

- 本地未修改、服务端已变化：下载服务端版本；
- 本地已修改、服务端仍等于`base_revision`：允许CAS上传；
- 本地已修改、服务端也已变化：产生冲突副本，禁止静默覆盖；
- 删除也必须按相同的基线和CAS规则处理。

### 2.5 修复同步路径的TOCTOU问题

当前计划先`MkdirAll`，再通过`EvalSymlinks`检查父目录。Claude进程可以在检查和写入之间替换符号链接，甚至可能在检查前就在workspace外产生目录副作用。

Linux生产实现应使用`openat2`的`RESOLVE_BENEATH`与`RESOLVE_NO_SYMLINKS`，或通过逐级目录文件描述符实现等价保护。普通`EvalSymlinks`只能作为非Linux测试替代，不能作为生产安全边界。

临时文件还必须满足：

- 使用随机且独占创建的文件名；
- 设置真实字节上限，并用“上限＋1字节”判断超限；
- 失败时删除临时文件并释放空间预留；
- 写入、校验、原子替换和数据库提交的失败恢复语义明确。

### 2.6 增加文件系统硬配额的实施任务

Task 7目前只实现数据库逻辑统计，不能阻止容器里的Claude直接写满VPS。

v3必须增加独立任务，并在目标VPS固定采用一种方案：

- XFS project quota；
- ZFS dataset quota；
- 每项目固定大小的loop文件系统。

必须验收：Claude绕过同步接口直接创建大文件时，Project A达到上限不会占用Project B的预留空间，也不会写满宿主机系统盘。

### 2.7 重写JSONL增量采集实现

当前样例代码与设计声明不一致，v3必须补齐：

1. 完整定义并实现`OffsetStore`，生产环境写入`usage_offsets`表；
2. 只在读取到完整换行后推进`committed_offset`；
3. 末尾半行保存为`partial_line`，下一轮补全后再解析；
4. 通过文件identity处理截断、重建和轮转；
5. Scanner超长行、坏行和读取错误必须记录指标，不能静默忽略；
6. SQL冲突键使用`(project_id, source_event_id)`；
7. 同一requestId多条记录必须按Task 0确认的“最终记录”或“各字段最大值”语义更新，不能简单保留第一条；
8. Worker只能通过只读挂载或受控`docker exec`读取Claude HOME，不能依赖Docker内部卷目录结构。

### 2.8 让cleanup模式真正可达

当前CLI在`conn.Over == true`时直接退出，因此后续cleanup同步永远不会运行。

正确流程应为：

1. 不打开Claude终端；
2. 继续建立sync连接；
3. 显示当前为cleanup模式；
4. 允许下载、删除和缩小文件；
5. 拒绝新增文件和扩大现有文件；
6. 窗口恢复后重新获取连接信息并恢复正常模式。

同时删除所有`?token=`示例。WebSocket令牌只能放Authorization头或首个认证帧，不能在同一计划中同时保留两种相互冲突的说法。

### 2.9 补齐占位实现

以下部分当前只是自然语言说明，不能称为“完整实现”：

- PostgreSQL文件索引、revision CAS和原子空间预留；
- 同步WebSocket状态机与所有消息错误处理；
- CLI终端循环、同步循环、重连和session重新exchange；
- 云端workspace watcher；
- 超额主动断流与恢复；
- 管理socket、管理员身份校验和CDK轮换；
- E2E测试的实际函数；
- 备份与恢复脚本。

v3可以选择提供完整代码级步骤，也可以改成接口、状态机、测试先行的工程计划，但不能继续用“完整实现”描述尚未给出的内容。

## 3. 部署前必须完成的调整

### 3.1 收紧worker和HTTP服务

- `worker-agent`默认监听地址改为`127.0.0.1:8081`或Unix socket，不能使用会绑定所有网卡的`:8081`；
- control-api也只监听反向代理可访问的本机或内网地址；
- 使用`http.Server`并设置`ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`和`IdleTimeout`；
- 检查并处理`ListenAndServe`返回错误；
- WebSocket设置最大消息大小、连接数上限、ping/pong和读写deadline；
- worker在接受terminal连接时再次实时检查项目额度，不能只信任两分钟前签发的令牌。

### 3.2 固定依赖与镜像版本

禁止正式实施步骤使用`go get ...@latest`或`npm install -g @anthropic-ai/claude-code`而不固定版本。

需要固定并记录：

- Go版本与模块版本；
- PostgreSQL主版本；
- Ubuntu基础镜像digest或固定tag；
- Node.js版本；
- Claude Code版本；
- Caddy／Nginx版本。

Claude Code升级必须先用脱敏JSONL样例和tmux恢复测试验证。

### 3.3 固定门户认证方案

v3不能继续保留“管理员cookie或SSH隧道二选一”的未决状态。个人版建议采用以下其中一种：

- 门户只监听localhost，通过SSH隧道访问；或
- 门户公网开放，但使用独立管理员登录cookie、CSRF保护、安全cookie属性和登录限速。

CDK会话只能查看其绑定项目的JSON状态，不能获得管理员能力。

### 3.4 完善备份与恢复

简单地对正在写入的Docker卷执行`tar`不能证明备份一致性。备份流程必须包含：

1. 暂停相关写入或使用文件系统一致性快照；
2. PostgreSQL一致性备份；
3. workspace、Claude HOME和同步状态卷备份；
4. 备份加密；
5. 复制到VPS之外；
6. 保留周期与失败告警；
7. 从空服务器执行真实恢复演练。

### 3.5 补齐真实端到端验收

交叉编译不等于三平台可用。至少需要：

- Windows、macOS和Linux的CLI启动与路径测试；
- 真实WebSocket终端重连；
- 同步冲突、删除传播和云端直接修改；
- 两个并发上传不能突破配额；
- A超额时已连接终端停止输入，B仍可用；
- control-api、worker、容器和VPS分别重启后的恢复；
- 日志与代理记录的秘密泄漏扫描；
- 从空服务器恢复备份。

## 4. 不可改变的额度边界

两个项目共用同一个Claude订阅账号时，Anthropic官方5小时／7天额度仍然是账号级共享额度。

本系统可以做到：

- 分别统计A和B的内部消耗；
- 为A和B设置各自的内部停止阈值；
- A达到内部阈值后停止A的新请求；
- 保留一部分内部预算供B继续使用。

本系统不能保证：

- 把官方订阅余额精确切成A、B两份；
- 内部显示百分比与Anthropic官方百分比完全一致；
- A的最后一个在途请求不会产生少量额外消耗；
- 上游账号级额度耗尽后B仍一定可用。

如果需要官方层面的精确隔离，必须改用两个独立上游账号，或者使用能按API key统计和限额的官方API接入。

## 5. v3修订完成的判定标准

只有同时满足以下条件，才能把实施计划状态改为“允许编码”：

- [ ] 本文第2节的编码前问题全部落实到Design Spec v3和Implementation Plan v3；
- [ ] 终端、同步、API和Caddy路径形成一份无歧义合同；
- [ ] 同步协议采用基线revision＋基线SHA＋服务端CAS；
- [ ] JSONL半行、轮转和同requestId更新规则有可执行测试；
- [ ] 磁盘硬配额有具体技术选型、部署步骤和验收；
- [ ] cleanup模式能在CLI与服务端端到端运行；
- [ ] 所有声称“完整实现”的任务都不再包含未定义占位函数；
- [ ] Design Spec状态由用户确认改为“已批准”；
- [ ] Task 0的24小时双登录、tmux容器原型和脱敏JSONL语义验证全部通过，或已接受明确降级方案。

## 6. 推荐执行顺序

1. 先根据本文修订Design Spec v3与Implementation Plan v3，暂不编码；
2. 对v3重新做一次设计—计划一致性审查；
3. 只执行Task 0架构阻断验证；
4. 任一阻断验证失败，先修改设计，不继续后续任务；
5. Task 0通过后，按单项目垂直切片开始编码；
6. 单项目终端和认证跑通后再做同步；
7. 同步可靠性通过后再引入第二项目；
8. 最后接入用量闸门、门户、硬配额、备份和完整E2E。

## 7. 可直接交给Claude的修订指令

```text
请先不要编码，也不要执行Implementation Plan中的任务。

请完整阅读以下三份文档：
1. Design Spec v2
2. Implementation Plan v2
3. remote-claude-workspace-v2-review-adjustments.md

根据第3份审查文档，把前两份修订为Design Spec v3和Implementation Plan v3。必须逐项解决审查文档第2节和第3节的问题，保留第4节说明的官方额度边界。

修订完成后，请输出：
- 每一条审查意见对应修改了哪个章节；
- 仍未解决的技术选择；
- Task 0的具体执行前提；
- 设计、计划、API路径、同步协议和验收测试之间的一致性检查结果。

在用户批准v3并明确允许前，不得开始正式编码，不得运行会消耗真实Claude额度的24小时测试。
```

