# 代码与Design Spec v3的偏离记录

**用途：**实现过程中偏离`docs/superpowers/specs/2026-07-19-remote-claude-workspace-design.md`的地方，在这里如实记录。spec本身**未**因这些偏离而修订——读spec时以本文件为补丁。

**最后核对：**2026-07-25（分支`v2`，commit `4bbd4c0`）

---

## D1. Claude HOME共享凭据 + 会话JSONL按项目独立卷

**状态：已定方案**（2026-07-26），取代此前"二选一待决策"。设计依据：`docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md` §7.3。

| | |
|---|---|
| **spec原文** | §6：每项目三个命名卷，`<slug>-claude`→`/home/claude/.claude`，项目间互相隔离 |
| **实际代码** | `deploy/compose.yaml`：`claude-shared:/home/claude`同时挂给`project-a`与`project-b`（共享凭据）；**另有`<slug>-claude-projects`嵌套挂载到`/home/claude/.claude/projects`**（会话JSONL按项目隔离） |
| **引入时间** | `3caa271 fix(deploy): shared Claude auth volume with correct ownership`（共享卷）；JSONL独立卷为2026-07-26追加 |

**产品硬约束：**Claude账号在一台VPS上**只授权一次**，客户数增加也不得增加授权次数。共享`/home/claude`是满足该约束的手段，不可回退为每项目独立HOME。

**方案要点：**凭据与会话JSONL位于不同路径，因此可分别挂载：

```
/home/claude/.claude.json                  配置        ← 共享卷
/home/claude/.claude/.credentials.json     OAuth凭据    ← 共享卷
/home/claude/.claude/projects/             会话JSONL    ← 每项目独立卷（嵌套挂载遮蔽）
```

`projects/`是`.credentials.json`的**兄弟目录**，嵌套挂载只遮蔽该子目录，凭据文件不受影响——**授权一次的性质完整保留**，同时按项目计量成立。

**根因订正（重要）：**本条目原先记载共享卷的动机是"规避spec风险R2（双容器同账号OAuth凭据互踢）"。这与`deploy/Dockerfile.claude`的注释不符，后者写明当时故障的真实根因是：

> 避免默认root:root导致claude无法写入`.credentials.json`（登录后`loggedIn=false`、**反复要求登录的根因**）

即那次"反复要求登录"是**命名卷ownership问题**，与OAuth凭据轮换无关。两件事在`3caa271`里被同时处理，因而在本文档中被归并成同一个动机。**R2从未被观测到发生过**，也从未被验证过（见下）。保留此订正是为了避免后续读者把ownership问题当作凭据轮换的证据，从而高估调整卷布局的风险。

**同一个ownership陷阱适用于新增卷：**`Dockerfile.claude`须预建`/home/claude/.claude/projects`并`chown claude:claude`，否则新命名卷以root:root初始化，JSONL写不进去。已于2026-07-26补上。

**已解决：**

- **P0-2（按项目计量在原理上不成立）**——JSONL已按项目分卷，归属由挂载关系保证，采集器可直接按卷读取，无需解析JSONL里的`cwd`。
  **注意：**这只是解除了结构性障碍；`internal/usage`采集器**仍未接线**（P0-1），端到端计量尚未成立，不得记为完成。

**仍未解决：**

1. **Phase 1 Step 1（24小时双登录验证）仍未做。**"两个容器同时用同一份凭据长期运行"的稳定性没有等价验证。现有部署一直在此形态下运行，但从未做过24小时以上的观察。客户数上去后应补做。
2. **隔离性残留。**共享HOME意味着客户间仍可互相读到`~/.claude/history.jsonl`（命令历史）、`shell-snapshots/`（含环境变量）、`sessions/`、`file-history/`、`todos/`。当前按"客户可信"接受（设计§7.5）。若要收紧，成本是为这几个路径各加一个嵌套卷。
3. **spec §6文本未修订**，仍描述每项目独立`<slug>-claude`卷。以本条目为准。

---

## D2. 服务在容器内监听`0.0.0.0`而非回环

| | |
|---|---|
| **spec原文** | §3：control-api与worker-agent**默认只监听**`127.0.0.1:8080`/`127.0.0.1:8081`，绝不绑定所有网卡 |
| **实际代码** | `deploy/compose.yaml`用环境变量覆盖为`0.0.0.0:8080`/`0.0.0.0:8081` |

**评估：不构成实际暴露。**两个服务都不发布宿主机端口，只在compose的内部`ccw`网络里被Caddy访问；容器内监听回环反而会让Caddy连不上。`internal/config`的**默认值**仍是回环（`config.Load`没有改），符合spec"默认"的要求。

**遗留要求：**若将来改为systemd直接跑在宿主机（非容器），必须去掉这两个环境变量覆盖，否则就是真的绑定全网卡。

---

## D3. 云端workspace变更靠manifest时全量reconcile，不是fsnotify watcher

| | |
|---|---|
| **spec原文** | §8：Worker监控云端workspace——500ms静默窗口→前后双哈希一致才入账→与file index比较→分配新revision |
| **实际代码** | `internal/sync/syncserver.go`的`reconcileCloud`在`HandleManifest`里被调用，每次客户端拉清单时全量扫描项目目录并与`file_index`比对 |

**评估：功能等价，非功能性有差距。**容器内Claude直接改的文件确实能被发现并分配新revision（验收22成立），但：

- 没有500ms静默窗口与前后双哈希，理论上可能把Claude正在写的半成品文件入账（spec风险R5的对策未落地）
- 没有`fsnotify`依赖（`go.mod`里就没有），大目录下每轮同步都是O(文件数)的扫描；客户端每2秒一轮，开销随项目规模线性增长

**修复方向：**引入fsnotify + 静默去抖 + 写入前后双哈希校验，reconcile退化为兜底的周期性全量校对。

---

## D4. 同步路径安全用EvalSymlinks，未用openat2

| | |
|---|---|
| **spec原文** | §8：Linux生产**必须**用`openat2(RESOLVE_BENEATH\|RESOLVE_NO_SYMLINKS)`或逐级目录fd等价实现；`MkdirAll`+`EvalSymlinks`只允许作为非Linux平台的测试替代，**不得作为生产安全边界** |
| **实际代码** | `internal/sync/server.go:32-50`用`EvalSymlinks`，代码注释已如实标明这是TOCTOU窗口、只适合本机/非Linux测试 |

**评估：这是明确的欠账，不是设计变更。**当前生产部署运行在这个已知不安全的实现上。触发条件是攻击者能在检查与写入之间替换项目目录内的符号链接——在单管理员个人版场景下风险有限，但spec把它列为"必须"，不应长期停留。

**修复方向：**Linux构建标签下用`unix.Openat2`实现`resolve`，非Linux保留现有实现并只用于测试。
