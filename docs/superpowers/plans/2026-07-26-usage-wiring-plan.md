# 实施计划：用量采集接线与额度闸门闭环（N1 + N2）

**状态：****R0–R9全部已实施**（2026-07-26同日），**但只有单测证据、未在真实部署上验证**
**日期：**2026-07-26
**分支：**`v2`
**对应任务：**`docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md`的N1与N2；解`docs/STATUS.md`的P0-1与P1-1
**优先级：****最高**——设计§12.3把N1定为共享节点的上线阻断项，未完成前一台机器只能服务一个客户

---

## 1. 目标与范围

让「内部额度闸门」从空转变为真正生效：JSONL用量进库 → 窗口计算有输入 → 超额时终端被关闭、同步降级为cleanup。

**范围内：**

- `internal/store`的用量写入端（`usage.Sink`与`usage.OffsetStore`的PG实现）
- `worker-agent`的采集goroutine与项目枚举
- 加权系数的生产配置入口
- `modeFor`按额度返回`rw`/`cleanup`（N2）
- 账号级池上限的存储与读取（**新发现，见§2.4**，是A30的前置）
- CI（无外部依赖、成本极低，放在最前面保护后续全部工作）

**范围外：**

- `openat2`路径解析（P1-2）——与本计划无耦合，建议紧随其后单独做
- `ccwadmin`扩展、compose渲染、Console任何部分
- 用量门户UI的改动——`/usage`已存在，数据非空后自然显示
- 上游真实用量校准（`quota.AccountUsageProvider`是spec预留接口，第一版无实现）

---

## 2. 现状盘点（2026-07-26核实）

### 2.1 比STATUS.md描述的小：读路径与表结构都已就位

| 部件 | 状态 |
|---|---|
| `usage_events`表（含`UNIQUE (project_id, source_event_id)`） | ✅ `001_initial.sql:28` |
| `usage_offsets`表（含`committed_offset`/`partial_line`） | ✅ `001_initial.sql:65` |
| `WindowUsed`/`PoolUsed`（`*Store`实现`quota.UsageReader`） | ✅ `internal/store/queries.go:37` |
| `quota.Service.Check`双窗口 + 池保护 | ✅ `internal/quota/service.go` |
| 采集器（offset持久化、半行拼接、坏行计数） | ✅ `internal/usage/collector.go`，190行，有单测 |
| worker的30秒主动执行循环（超额关终端） | ✅ `cmd/worker-agent/main.go:144` |
| **`usage.Sink`的PG实现** | ❌ 不存在 |
| **`usage.OffsetStore`的PG实现** | ❌ 不存在 |
| **采集goroutine** | ❌ 全仓无任何文件import`ccw/internal/usage` |
| **`usage.Weights`的生产配置入口** | ❌ 只出现在测试里 |

**因此N1不需要新建表，也不需要写窗口查询。**工作量集中在写入端与接线。

### 2.2 阻断项：worker-agent根本读不到JSONL

采集器跑在`worker-agent`进程里，但会话JSONL在`<slug>-claude-projects`卷中，而`deploy/compose.yaml:52-56`只把`<slug>-workspace`挂给了worker-agent：

```yaml
worker-agent.volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - project-a-workspace:/srv/ccw/project-a
  - project-b-workspace:/srv/ccw/project-b        # 没有 claude-projects
```

**没有这个挂载，采集goroutine写完也扫不到任何文件，且不会报错**——`Collector.Scan`用`filepath.WalkDir`，目录不存在时回调收到err直接`return nil`（`collector.go:140`，注释写明"单文件失败不中断整体采集"）。失败模式是「采集器在跑、日志正常、`usage_events`永远为空」，与今天的现象完全一致，极难排查。

**必须先改编排。**本计划§6的R1就是这一步。

### 2.3 采集范围必须是全部项目，不能是活跃项目

现有30秒循环遍历的是`registry.ActiveProjects()`（有活跃终端连接的项目）。**采集不能照抄这个范围：**tmux会话在客户端断开后继续运行，Claude照常消耗额度。若只采集活跃项目，断开期间的用量会全部丢失，直到下次连接才被补上——而闸门恰恰要在无人值守时生效。

**结论：**采集遍历**数据库里的全部项目**，需要`store`新增`ListProjects`。执行（关终端）仍只针对活跃项目——那是正确的，没有连接就没有东西可关。

### 2.4 新发现：账号级池上限无处存储，A30目前不可能成立

`accounts`表只有`id`/`name`/`upstream_pool`/`created_at`，**没有池额度上限的列**。而`cmd/worker-agent/main.go:157`构造Limits时写死：

```go
lim := quota.Limits{FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit,
    PoolFiveHour: 1 << 62, PoolSevenDay: 1 << 62}   // 池闸门被彻底禁用
```

`quota.Service`的池保护逻辑（`service.go:43-54`）是完整的，但输入是`1<<62`，永远不可能触发。

**后果：**即使N1做完，验收A30（账号池用尽时同节点全部客户一并降级）仍不成立。多个项目共用一个上游账号时，**没有任何机制防止总量被击穿**——这正是设计§7.4"两道闸门"里的第二道。

**因此本计划纳入一个最小切片：**`002`迁移给`accounts`加两列 + `store`读取 + worker使用。account模型的其余部分（一节点一account、`ccwadmin`不再硬编码`default-pool`）仍属N3，不在本计划内。

**注意：**这将是本仓库的**第二个迁移文件**。CLAUDE.md规定迁移只有`internal/store/migrations/`一份源、靠`schema_migrations`表保证只执行一次——新增文件放同一目录即可，**不得在别处复制**。

### 2.5 `fileIdentity`退化为路径

`collector.go:117`的`fileIdentity`直接返回路径，注释自己写明生产版应在`collector_linux.go`用`syscall.Stat_t`取`dev:inode`。

**实际风险有限但非零：**Claude的JSONL按session id命名，同路径重建的概率低；且`Scan`有截断检测（`fi.Size() < resume`时从头重扫，靠幂等写入兜底）。真正会漏的情况是：文件在同路径被替换为**更大**的新文件，此时游标停在旧偏移，中间一段被跳过。

**定级：**做，但不是阻断项；排在R6，可以在闸门跑通之后补。

---

## 3. 必须先定的语义决策

### 3.1 加权系数取值：先记账、后校准（用户2026-07-26定）

`usage.Weights{Input, Output, CacheRead, CacheWrite}`把四种token数折算成一个"内部额度单位"数值，闸门用它跟限额比较。**这些数字目前没有任何依据**——生产无配置入口，测试里的值是随便取的。

**决定：不在实施前定死，分两阶段。**

| 阶段 | 做法 |
|---|---|
| **一：只记账** | 权重取下表默认值，项目限额与池上限**都设得明显偏大**，闸门不真正拦人。`usage_events`开始积累真实数据，`/usage`门户可见 |
| **二：校准** | 跑够一周后按实际分布定权重与限额，改权重是改一个配置项、改限额是一条UPDATE，成本极低 |

**理由：**`usage_events`至今为空，没有人知道一次正常会话消耗多少token、一天能跑多少轮。**此刻拍脑袋定的数字和默认值一样没有依据**，而两者事后都极易调整。先让数据长出来。

**阶段一的默认值**（比例参照公开的相对定价量级，**是估算不是官方口径**）：

| 字段 | 默认权重 |
|---|---|
| `Input` | 1 |
| `Output` | 5 |
| `CacheRead` | 1（实际是十分之一量级，取整为1） |
| `CacheWrite` | 1 |

配置入口：`CCW_USAGE_WEIGHTS="1,5,1,1"`，按CLAUDE.md规则**缺失即硬失败**，不在代码里设默认——默认值写在`deploy/.env.example`里，让它是一个显式的部署选择而不是隐藏行为。

**表述边界（CLAUDE.md）：**对外一律称"内部额度单位"，**不得**标注为官方订阅百分比。

**阶段二的触发条件要写进`docs/STATUS.md`**，否则"先记账"会变成"永远不校准"——闸门看着已完成、实际从未拦过任何人，这与"未验证的功能不写成已完成"是同一类错误。

### 3.2 采集周期

**定为30秒**，与现有额度执行循环同频。理由：更密没有意义（闸门本来就是30秒粒度），更疏会让超额发现延迟。

**不与执行循环合并成一个goroutine**——采集遍历全部项目、执行只遍历活跃项目，两者失败域也不同（采集失败不应影响关终端）。分开两个goroutine，各自独立recover。

### 3.3 挂载点与配置

worker-agent以**只读**方式挂载各项目的claude-projects卷，路径约定`/srv/ccw/usage/<slug>`，与workspace的`/srv/ccw/<slug>`并列：

```yaml
worker-agent.volumes:
  - project-a-claude-projects:/srv/ccw/usage/project-a:ro
```

新增配置`CCW_USAGE_ROOT`（同样缺失即硬失败）。`Collector.Dir = filepath.Join(cfg.UsageRoot, slug)`。

只读的理由：采集器只读JSONL，游标写PG。给写权限没有用途，只会扩大worker对客户数据的影响面。

### 3.4 Sink失败的语义（沿用采集器现有设计，不改）

`Scan`在`Sink.Insert`失败时**直接返回错误且不保存游标**（`collector.go:172-174`），下一轮从同一位置重扫。配合`UNIQUE (project_id, source_event_id)`的幂等写入，重复扫描不会重复计量。

**这是正确的设计，PG实现必须与之匹配**：`INSERT ... ON CONFLICT DO UPDATE`而不是`DO NOTHING`——同一`requestId`再次出现时按`GREATEST`各字段取最大值（`collector.go:89`的注释与Phase 1证据`docs/phase1-evidence/jsonl-semantics.md`）。

---

## 4. 不变量（实现的正确性定义，每条要有测试）

| # | 不变量 | 违反的后果 |
|---|---|---|
| U1 | 同一`(project_id, source_event_id)`重复采集不增加总量 | 重启或重扫导致用量虚高，闸门误触发 |
| U2 | 同一`requestId`的后续记录按`GREATEST`更新各token字段 | 取最终值语义失效，用量偏低 |
| U3 | 采集遍历**全部**项目，不限于有活跃终端连接的项目 | 无人值守期间的用量丢失（§2.3） |
| U4 | 半行（无换行结尾）不入账，游标停在最后一个完整行 | 把Claude正在写的半成品当成事件 |
| U5 | `Sink`失败时游标不前进 | 事件丢失且无法察觉 |
| U6 | 项目A的JSONL只能记入A的`project_id` | 归属交叉，按项目计量失效（A28） |
| U7 | 采集goroutine的panic不影响执行goroutine，反之亦然 | 一个失败拖垮闸门。**注意Go的panic不分goroutine**：未recover的panic终止整个进程，因此两个循环必须各自recover（`guard`），只做一侧等于没做 |
| U8 | 用量相关的日志与错误信息不含JSONL内容 | 会话内容泄漏进日志 |

**U6是产品硬约束的技术表达**：它由挂载关系保证（每个项目一个独立卷），测试须显式覆盖「两个项目各自的Dir互不重叠」。

**U8需要特别注意：**JSONL里是用户与Claude的完整对话。解析失败时**不得**把行内容打进日志——现有`BadLines`只计数不记内容，这个设计要保持。

---

## 5. CI（步骤0，与N1无耦合）

现在没有`.github/`。加一个最小workflow：

```
go build ./...
go test ./...
go test -race ./...
gofmt -l .    （有输出即失败）
```

**放最前面的理由：**后面每一段都会动`internal/store`与`cmd/worker-agent`，而`internal/store`目前**零测试**。没有CI时，一次本地漏跑就可能把回归带进主线。成本约半天，收益覆盖整个v2。

---

## 6. 实施步骤（TDD）

按CLAUDE.md：先写失败测试再实现，`go test ./...`全绿才提交。

| # | 步骤 | 产出 | 说明 |
|---|---|---|---|
| R0 | CI workflow | `.github/workflows/ci.yml` | §5，可独立提交 |
| R1 | **编排：claude-projects只读挂进worker-agent** | `deploy/compose.yaml`、`deploy/.env.example` | §2.2的阻断项，必须最先。提交前`docker compose config`校验 |
| R2 | `store`用量写入端 + 首批`store`测试 | `internal/store/usage.go`、`usage_test.go` | 实现`usage.Sink`与`usage.OffsetStore`；覆盖U1、U2、U5 |
| R3 | `ListProjects` | `internal/store/admin.go` | §2.3；采集遍历全部项目 |
| R4 | 权重配置化 | `internal/config/config.go` | §3.1；缺失即硬失败 |
| R5 | 采集goroutine接线 | `cmd/worker-agent/main.go` | 每项目一个Collector，30秒一轮，独立recover；覆盖U3、U6、U7 |
| R6 | `fileIdentity`取dev:inode | `internal/usage/collector_linux.go` | §2.5；非Linux保留路径实现 |
| R7 | **`002`迁移：accounts加池上限两列** | `internal/store/migrations/002_account_pool_limits.sql` | §2.4；本仓库第二个迁移，放同一目录 |
| R8 | worker读取真实池上限 | `cmd/worker-agent/main.go:157` | 删掉`1<<62`；A30的前置 |
| R9 | **`modeFor`查额度（N2）** | `cmd/worker-agent/main.go:119` | 超额返回`cleanup`；覆盖验收21、27 |

**R1必须第一个做**，否则R5写完无法验证——采集器会安静地扫一个空目录。

**R7/R8可以推迟**：它们服务的是A30（池闸门），与项目级闸门独立。若想尽快让项目级闸门生效，R0–R6 + R9即可，R7/R8单独一提交。

---

## 7. 验收标准

**实施后的实际状态（2026-07-26，经代码审核修正）：**V1–V7、V11由单测覆盖并通过；**V8–V10未跑**，需要真实部署，自查步骤见`DEPLOY.md`第11.2节。

> **一处自我修正：**本节初版把V11（日志不含JSONL内容）标为"由单测覆盖"，但当时所有测试传入的`logf`都是空函数、没有任何断言——行为本身是对的，"有测试保证"却是假的。这正是CLAUDE.md禁止的"把没验证报成通过"。已补`TestLogsNeverContainJSONLContent`真正捕获日志文本做断言，该声明现在成立。

| # | 标准 | 需要真机 |
|---|---|---|
| V1 | 构造两个项目的假JSONL目录，采集后各自事件只记入自己的`project_id`，无交叉（U6、设计A28） | 否 |
| V2 | 同一份JSONL重复采集两次，`usage_events`行数与`weighted_units`总和不变（U1） | 否 |
| V3 | 同`requestId`第二条记录token更大时，按`GREATEST`更新而非新增行（U2） | 否 |
| V4 | 半行结尾的文件采集后，该半行不入账；补全换行后再采集，恰好入账一次（U4、验收16） | 否 |
| V5 | `Sink`注入失败后游标不前进，恢复后无遗漏无重复（U5） | 否 |
| V6 | 无任何终端连接时用量仍被采集（U3） | 否 |
| V7 | 项目5h用量超限后，`modeFor`返回`cleanup`，上传被拒、下载/删除/缩小仍可用（验收21、27） | 否 |
| V8 | 超额时已连接的终端在30秒内被关闭（验收20） | 部分 |
| V9 | 账号池耗尽时，同节点全部项目一并降级为cleanup，`/usage`如实显示原因（设计A30） | 是 |
| V10 | 真实部署上，两个项目的JSONL确实分别落在各自卷中（设计A26，**P0-2至今未真机验证**） | 是 |
| V11 | 用量相关日志中搜不到JSONL内容片段（U8） | 否 |

**诚实表述（CLAUDE.md）：**V9与V10需要真实节点，未跑过不得标记完成；e2e未实现的断言用`t.Skip`，不用空断言。

---

## 8. 开放问题

**已关闭（用户2026-07-26定）：**

- ~~加权系数取值~~——**改为「先记账、后校准」两阶段**，阶段一用`1/5/1/1`，见§3.1。实施不再被此阻塞
- ~~池上限初始值~~——同上，阶段一设得明显偏大（只记账不拦人），跑一周后按真实分布定。设计§15开放问题2（超卖倍率）仍开放，但不阻塞本计划

**仍开放：**

1. **阶段二的校准何时做、由谁判断数据够了？**建议以「`usage_events`覆盖连续7个自然日且至少3个活跃日」为门槛，写进`docs/STATUS.md`跟踪。**不定这条，「先记账」就会变成「永远不校准」。**
2. **首次采集的历史积压**：已运行一段时间的部署，首轮`Scan`会把全部历史JSONL入账，可能瞬间把项目推到"已超额"。是否需要一个"只采集从接线时刻起的新事件"的开关？**倾向不做**——历史用量本来就消耗了真实额度，如实计入更诚实；但要在UPDATE.md里提示这个现象，避免被当成bug。
3. 每事件一次INSERT，首轮积压可能有数千次往返。是否需要批量写入？**倾向先不做**——批量会让"Sink失败游标不前进"的语义变复杂（§3.4），等实测确认是瓶颈再优化。

---

## 9. 与后续任务的衔接

**本计划改变了C13（compose渲染）的模板契约。**渲染计划§5的不变量I1只覆盖`worker-agent.volumes`里的workspace挂载，本计划的R1新增了claude-projects只读挂载。C13实施时必须把它补进不变量（建议编号I8），否则渲染出的编排会让新项目的用量采集静默失效——与§2.2描述的失败模式完全相同。

**这也是N1必须排在C13之前的理由**：先接线，模板契约才能一次写对，不用回头改渲染器与黄金文件。

**对Console的影响：**N1完成后，设计§12.3的第一道硬闸门解除，一台机器可以服务多个客户。N2完成后第二道解除。第三道（N4磁盘失控防线）已按`docs/STATUS.md`的T1改写，与本计划无关。
