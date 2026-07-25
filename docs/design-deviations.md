# 代码与Design Spec v3的偏离记录

**用途：**实现过程中偏离`docs/superpowers/specs/2026-07-19-remote-claude-workspace-design.md`的地方，在这里如实记录。spec本身**未**因这些偏离而修订——读spec时以本文件为补丁。

**最后核对：**2026-07-25（分支`v2`，commit `4bbd4c0`）

---

## D1. Claude HOME由"每项目独立卷"改为两个项目共享一个卷

| | |
|---|---|
| **spec原文** | §6：每项目三个命名卷，`<slug>-claude`→`/home/claude/.claude`，项目间互相隔离 |
| **实际代码** | `deploy/compose.yaml`：`claude-shared:/home/claude`同时挂给`project-a`与`project-b`；挂的是整个home而非仅`.claude`（为同时持久化`.claude.json`与`.claude/.credentials.json`） |
| **引入时间** | `3caa271 fix(deploy): shared Claude auth volume with correct ownership` |

**动机：**规避spec风险R2（双容器同账号OAuth凭据互踢）与"登录后反复要求登录"的现场故障。共享卷意味着管理员**只需登录一次**，两个项目共用同一份凭据。

**代价（尚未解决）：**

1. **按项目计量在原理上不成立。**两个项目的会话JSONL写进同一个Claude HOME，`internal/usage`即使接线也无法区分某条usage属于A还是B。5小时/7天的**项目级**闸门因此失去数据基础——这是spec §10承诺的核心能力之一。
2. **Phase 1 Step 1（24小时双登录验证）被绕过而非通过。**spec把它列为引入第二项目并行前的上线闸门；共享凭据回避了这个问题，但"两个容器同时用同一份凭据跑"本身没有做过等价的稳定性验证。
3. **隔离性下降。**A容器内的进程可读到共享HOME里的全部内容，包括B产生的会话记录。spec §1的"完全隔离的项目容器"在凭据维度上不再成立。

**待决策（二选一，尚未定）：**

- **回到独立卷**：恢复`<slug>-claude`每项目一份，完成24小时双登录验证；失败则按spec降级为分时使用。计量与隔离都恢复。
- **保留共享卷**：必须为usage找到新的归属依据（例如按JSONL记录里的cwd/workspace路径切分，或改为每项目独立Claude HOME但共享凭据文件），并把这个决定与其代价正式写进spec §6与§10的"系统不保证"清单。

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
