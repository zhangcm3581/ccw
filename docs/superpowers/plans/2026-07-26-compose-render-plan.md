# 实施计划：compose渲染（`ccwadmin render-compose`）

**状态：**计划，**未实施**（本文档写作时零代码）
**日期：**2026-07-26
**分支：**`v2`
**对应任务：**`docs/superpowers/specs/2026-07-25-ccw-console-fleet-design.md`的C13
**触发问题：**`deploy/compose.yaml`硬编码`project-a`/`project-b`，第3个客户怎么加

---

## 1. 目标与范围

把「加一个客户」从**手工编辑4处YAML**变成**一条幂等命令**：

```bash
ccwadmin render-compose --projects project-a,project-b,project-c > compose.yaml
```

**范围内：**`compose.yaml`的渲染、slug校验、项目数与配额上限强制、卷布局不变量。

**范围外（本计划不做）：**

- `Caddyfile`渲染——它不随项目数变化，只依赖`CCW_DOMAIN`环境变量；仅在启用§6.2的CNAME别名时才需要模板化，届时单独处理
- `.env`生成——按设计§5.3步骤7，密钥必须**在节点本地生成**，不经Console，不属于渲染范畴
- Console侧的调用与UI——那是C15/C17，本计划只提供被调用的命令

---

## 2. 现状盘点

### 2.1 Go代码不需要改

`worker-agent`是完全数据驱动的，不认识`project-a`这类名字：

| 位置 | 行为 |
|---|---|
| `cmd/worker-agent/main.go:70-72` | 容器名从数据库查`container_name`，回退`ccw-<projectID>` |
| `cmd/worker-agent/main.go:102-103` | workspace路径＝`filepath.Join(cfg.WorkspaceRoot, slug)` |

因此第3个、第30个项目都**只是编排配置问题**，`internal/`与`cmd/`的现有逻辑零改动。

### 2.2 手工加一个项目要动4处

```
1. worker-agent.volumes      + project-c-workspace:/srv/ccw/project-c
2. services                  + project-c: 整个service块（4个挂载）
3. volumes                   + project-c-workspace / -claude-projects / -sync
4. 节点上执行                 ccwadmin init-project project-c
```

### 2.3 问题一：漏掉worker-agent挂载会**静默失败**

`internal/sync/server.go:44`的`DirStore.resolve`会`os.MkdirAll`目标目录。若忘了第1处：

- 同步**不报任何错误**
- 文件被写进worker-agent容器**自身的文件系统**里的`/srv/ccw/project-c`
- project-c容器永远看不到这些文件，客户在终端`ls`是空的
- worker-agent一重建，这些文件全部消失

失败模式是「客户说文件不见了，日志里没有任何异常」。手工维护到第5、第8个项目时，这一行迟早会漏。**这是本计划最主要的动机。**

### 2.4 问题二：`quota-setup.sh`与compose的卷名不兼容（新发现）

`deploy/quota-setup.sh`最后一步创建的是**bind-backed本地卷**：

```bash
VOL="${SLUG}-workspace"
docker volume create --driver local --opt type=none --opt o=bind \
  --opt device="/srv/ccw/workspaces/${SLUG}" "$VOL"
```

卷名是`project-a-workspace`。但compose创建的卷带项目前缀——`docker compose config`实测输出为`deploy_project-a-workspace`。

**两者不是同一个卷。**因此即使执行了`quota-setup.sh`，compose起的容器用的仍是自己那个不受配额约束的普通命名卷，**硬配额完全不生效且没有任何报错**。

这解释了`docs/STATUS.md`里Task 13标注的「脚本写好了但没有任何流程调用它」——它按现状调用了也不起作用。任何渲染方案都必须一并解决这个对齐问题，否则是在自动化一个坏掉的配置。

**本条为2026-07-26静态代码审查所得，未在真实部署上复现验证。**实施前应先在节点上执行`docker volume ls`比对确认。

### 2.5 问题三：加项目会导致在线客户短暂断线

修改`worker-agent`的`volumes`会使`docker compose up -d`**重建worker-agent容器**，后果：

- 所有在线客户的终端WebSocket与PTY断开
- 但**tmux会话不丢**（它在项目容器里，且设计明确「断开只关PTY与WebSocket，绝不`kill-session`」），客户重连即恢复现场

即：加新客户会让现有客户经历一次可感知的中断。这不是bug，但Console执行该操作时**必须提示**，且不应在业务高峰期执行。§4.3讨论了消除它的可能性。

---

## 3. 命令接口

### 3.1 签名

```
ccwadmin render-compose [flags]

  --projects   逗号分隔的slug列表（必填）
  --out        输出路径，缺省写stdout
  --claude-image      项目容器镜像，默认 ccw-claude:latest
  --disk-gib          单项目磁盘配额，默认15，超过15即拒绝（设计§7.6）
  --check      不输出，只校验入参并与数据库比对，退出码非0表示不一致

注：曾计划的 --workspace-mode 已取消，见§4.4——渲染器只产出方案A布局。
```

### 3.2 输入来源：显式列表，不读数据库

**决策：**项目列表由`--projects`显式传入，渲染过程**不依赖数据库**。

**理由：**

1. **鸡生蛋**：bootstrap顺序是`compose up` → `control-api`跑迁移 → `ccwadmin init-project`。首次渲染时数据库里一个项目都没有
2. **职责分离**：按设计§2.1，项目的权威源在Console的`node_projects`表；节点侧`ccwadmin`是执行器不是真相源
3. **可测试**：不依赖数据库的纯函数最容易做TDD

`--check`模式提供反向校验：读数据库，比对渲染结果覆盖的slug集合与`projects`表是否一致，用于巡检发现「库里有但compose里没有」的漂移。

### 3.3 必须绕开`config.Load`

`cmd/ccwadmin/main.go:31`现在无条件调用`config.Load`，缺`CCW_DATABASE_URL`即硬失败。渲染不需要数据库，因此`render-compose`必须在`config.Load`之前分支。

**注意不要削弱现有约束：**其他子命令仍须保持「缺失即硬失败、无默认值」的行为（CLAUDE.md）。改动只是让`render-compose`不进入那条路径，不是给`config.Load`加默认值。

### 3.4 幂等与确定性

同一份输入必须产出**字节级完全相同**的输出：

- 项目按slug字典序排序后渲染，不依赖`--projects`的书写顺序
- 不含时间戳、不含随机数、不含主机名
- 输出末尾统一`\n`，缩进固定两空格

这是`--check`能工作的前提，也是设计§5.3要求「每步幂等、可precheck跳过」的落脚点：`push-artifacts`步可以直接比对sha256决定是否需要重传。

---

## 4. 卷布局：三个方案

`workspace`卷的形态直接决定问题2.4与2.5能否解决。

### 4.1 方案A：维持现状（普通命名卷）

```yaml
worker-agent.volumes:  project-c-workspace:/srv/ccw/project-c
project-c.volumes:     project-c-workspace:/workspace
volumes:               project-c-workspace: {}
```

- 改动最小，渲染最简单
- **不解决**问题2.4（硬配额仍然失效）
- **不解决**问题2.5（加项目仍重建worker-agent）

### 4.2 方案B：compose声明外部卷，与`quota-setup.sh`对齐

```yaml
volumes:
  project-c-workspace:
    external: true
    name: project-c-workspace     # 与quota-setup.sh创建的卷同名
```

- **解决**问题2.4：compose用的就是脚本创建的、bind到loop挂载点的那个卷
- 不解决问题2.5
- **新增前置依赖**：`quota-setup.sh`必须在`compose up`**之前**执行，否则external卷不存在、compose直接失败。这把bootstrap的步骤顺序变成硬约束（设计§5.3的步骤12`quota-setup`需前移）
- 渲染需要一个开关：未启用硬配额的部署仍走方案A

### 4.3 方案C：bind mount宿主机目录

```yaml
worker-agent.volumes:  /srv/ccw/workspaces:/srv/ccw          # 一个挂载，永不随项目数变化
project-c.volumes:     /srv/ccw/workspaces/project-c:/workspace
```

- **解决**问题2.4：直接就是loop挂载点，无卷名中间层
- **可能解决**问题2.5：worker-agent的挂载不再随项目数变化，加项目不触发它重建
- 备份从「导出命名卷」简化为「打包宿主机目录」

**但方案C有一个未验证的关键前提：**Docker bind mount默认传播模式是`rprivate`。worker-agent启动**之后**在宿主机新建的loop挂载（`/srv/ccw/workspaces/project-c`）**不会**传播进已运行的worker-agent容器。若如此，加项目仍需重启worker-agent，方案C相对方案B就只剩备份便利这一个好处。

绕开办法是把该挂载设为`rshared`并让宿主机对应挂载点也是shared，但这会放大容器对宿主机挂载命名空间的影响面，需要单独评估。

**结论：方案C的核心卖点（不重建worker-agent）依赖一个尚未验证的行为，不能作为选型依据。**已按§4.4放弃，该未验证前提因此不再影响任何决策。

### 4.4 本计划的选择：方案A（沿用现状）

**已定（用户2026-07-26）：沿用`main`分支既有的普通命名卷布局，不做任何卷形态变更。**

核实：`main`与`v2`的workspace布局完全一致（`main..v2`只有一个文档commit），本计划落地时无需迁移任何已有部署。

**因此本计划简化：**

- 不实现`--workspace-mode`开关，渲染器只产出方案A的布局
- **V1（bind传播实测）从前置闸门降级为不需要**——方案C不做，其未验证前提也就不影响任何决策
- R4（bind分支）从任务列表移除

**代价必须记录在案：**硬配额（`docs/STATUS.md`的T1）**不会**因本计划而修复，`quota-setup.sh`仍处于「执行了也不生效」的状态。后果是**客户在容器内直接写盘可撑爆宿主机磁盘，拖垮同机全部客户**——逻辑配额（`internal/storage`）只统计走同步接口的文件，拦不住容器内直接写入。

该风险不是多客户引入的新问题（现有双项目部署同样存在），但爆炸半径从「自有项目」变成「付费客户」。且它通常**不需要恶意**即可触发：`npm install`、构建缓存堆积、日志未轮转都会导致。用户此前确认的「客户可信」是针对容器逃逸这类主动攻击的判断，不覆盖意外写满。

**约定的替代防线（不改卷布局，成本递增）：**

| 档 | 做法 | 效果 | 归属 |
|---|---|---|---|
| 低 | 磁盘水位告警 | 出事前收到通知 | 并入设计§11.5的节点巡检 |
| 中 | Docker `data-root`独立分区/盘 **+ Postgres数据移出data-root** | 撑爆的是该盘，宿主机OS存活可SSH救援；**数据库存活以第二项为前提**，见下 | bootstrap的`install-docker`步骤 |
| 高 | loop-mount硬配额（方案B/C） | 单客户写满只影响自己 | 暂不做，见`docs/STATUS.md`的T1 |

**当前采纳低+中两档。**高档保留为未来选项；若将来启用，需回到§4.2/§4.3重新评估，并处理已有部署的卷迁移。

**中档的一个必要修正（2026-07-26核实）：**本表原先写中档能让"宿主机OS与Postgres存活"，**Postgres那半句不成立**。`deploy/compose.yaml:112`的`ccw-pg`是普通命名卷，落在`/var/lib/docker/volumes`即data-root上——客户写满data-root，数据库跟着一起挂，认证、额度、同步全停。要兑现"数据库存活"，须把`ccw-pg`改为bind到data-root之外的宿主机目录（如`/var/lib/ccw/pgdata`）。**已有部署改此项需要迁移数据**（`pg_dump`后恢复，或停栈后拷贝卷内容），不可直接改配置重启。该项已并入设计§12.1的N4。

**中档不消除客户间的互相影响**：三个客户共用同一个data-root，任一客户撑爆后另外两个同样写不下去。中档只保证机器可救、数据不丢。

---

## 5. 模板契约（渲染必须保证的不变量）

这些是**渲染器的正确性定义**，每条都要有对应测试：

| # | 不变量 | 违反的后果 |
|---|---|---|
| I1 | 每个项目在`worker-agent.volumes`里有且仅有一条`<slug>-workspace:/srv/ccw/<slug>` | 同步静默写到worker容器内（§2.3） |
| I2 | 每个项目容器挂载4项：workspace、`claude-shared`、`<slug>-claude-projects`、`<slug>-sync` | 缺workspace＝无文件；缺claude-shared＝**要求重新授权**；缺projects卷＝用量归属失效 |
| I3 | **所有**项目共用同一个`claude-shared:/home/claude` | 违反产品硬约束「账号只授权一次」（设计§7.3） |
| I4 | `<slug>-claude-projects`挂载点恒为`/home/claude/.claude/projects` | 挂错层级会遮蔽`.credentials.json`，导致每个项目各自要求登录 |
| I5 | 容器名恒为`ccw-<slug>`，与数据库`container_name`一致 | worker-agent的`docker exec`找不到容器，终端不可用 |
| I6 | `volumes:`段声明了所有被引用的卷，无多余、无遗漏 | compose启动失败或残留孤儿卷 |
| I7 | 渲染结果对同一输入字节级稳定 | `--check`与`push-artifacts`的sha256比对失效 |

**I3与I4是产品硬约束的技术表达，测试必须显式覆盖**——它们保护的是「一台VPS只授权一次」这条不可退让的要求。

---

## 6. slug校验（安全边界）

slug会被拼进**容器名、卷名、宿主机路径**，是注入面，必须严格校验：

```
^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$
```

- 只允许小写字母、数字、连字符；不得以连字符开头或结尾；总长2–32
- 拒绝：大写、下划线、点、斜杠、空格、任何非ASCII
- **保留名**（与compose内既有service冲突）：`postgres`、`control-api`、`worker-agent`、`caddy`
- 同一次渲染内不得重复

**项目数上限：最多3个**（设计§7.6的产品规则）。传入第4个即拒绝整次渲染，错误信息须说明上限来源，不做截断。

**校验失败即拒绝整次渲染**，不做「跳过非法项继续渲染」——部分成功会产出一个看似正常但少了项目的compose，比直接失败更危险。

**注意：渲染器只是上限的强制点之一，不是唯一。**`ccwadmin init-project`必须独立校验同一条规则（设计§11.1）——否则有人绕过渲染器直接建项目，就会出现「数据库里有4个项目、compose里只有3个」的漂移，而第4个项目的客户会拿到能认证但连不上容器的CDK。

与现有约束的一致性：`internal/sync/paths.go`已经是路径安全边界的一部分（CLAUDE.md），slug校验应与其风格一致，并同样有独立单测。

---

## 7. 实施步骤（TDD）

按CLAUDE.md：先写失败测试再实现，`go test ./...`全绿才提交。

| # | 步骤 | 产出 | 说明 |
|---|---|---|---|
| R1 | slug校验器 + 单测 | `internal/deploy/slug.go` | 覆盖§6全部拒绝用例 |
| R2 | 渲染器核心 + 契约测试 | `internal/deploy/compose.go` | 逐条断言§5的I1–I7 |
| R3 | 黄金文件测试 | `testdata/compose-{1,2,3,10}.golden.yaml` | 锁定输出，防回归；同时人工核对2项目输出与现有手写文件等价 |
| R4 | `render-compose`子命令接线 | `cmd/ccwadmin/main.go` | 绕开`config.Load`（§3.3） |
| R5 | `--check`模式（读库比对） | 同上 | 需要数据库，测试用假store |
| R6 | 用渲染结果替换手写`deploy/compose.yaml` | — | 提交前必须`docker compose config`校验通过 |
| R7 | 更新`DEPLOY.md`/`UPDATE.md`加项目流程 | — | 把「手工改4处」的旧说明删掉 |
| R8 | 磁盘水位告警（§4.4低档防线） | 并入节点巡检 | 可与渲染器并行，不阻塞 |

**前置实测已取消。**原计划的V1（bind传播行为）随方案C不做而失去意义；V2（`quota-setup.sh`卷名不兼容）不再是本计划的闸门——该结论已记入`docs/STATUS.md`的T1（已知取舍），留待将来真要上硬配额时验证。

**本计划因此是纯编码任务，无外部依赖，可直接开工。**

---

## 8. 验收标准

| # | 标准 |
|---|---|
| B1 | `render-compose --projects a,b`的输出与现有手写`deploy/compose.yaml`在语义上等价（`docker compose config`归一化后逐字段比对） |
| B2 | 渲染1/2/3/10个项目均通过`docker compose config`校验 |
| B3 | 同一输入连续渲染两次，输出字节级相同 |
| B4 | 项目顺序打乱后渲染，输出不变（字典序归一） |
| B5 | I1–I7每条不变量都有对应失败测试，且实现前该测试确实失败过 |
| B6 | 非法slug（大写、`..`、`/`、超长、保留名、重复）全部被拒绝，且错误信息指明具体哪一个 |
| B7 | **渲染出的每个项目都挂载同一个`claude-shared`**（I3的直接证据，对应产品硬约束） |
| B8 | `--check`能检出「数据库有project-c但compose没有」的漂移 |
| B9 | 用渲染结果在真实节点上加第3个项目，新客户可登录、可同步，且**现有两个客户的tmux现场在重连后完好** |
| B10 | 渲染结果与`main`分支既有的workspace卷布局一致——加项目不会导致已有部署的卷名变化（§4.4「沿用现状」的直接证据） |

**诚实表述（CLAUDE.md）：**B9与B10需要真实节点，未跑过就不得标记完成；e2e未实现的断言用`t.Skip`。

---

## 9. 开放问题

**已关闭（2026-07-26）：**

- ~~`--workspace-mode`默认值~~——沿用现状，不做该开关（§4.4）
- ~~硬配额是否默认启用~~——不启用，改走告警+独立分区两档防线（§4.4）
- ~~现有部署的卷迁移~~——布局不变，无迁移

- ~~项目数上限~~——**硬上限3个**（用户2026-07-26定，设计§7.6），已并入§6校验

**仍开放：**

1. **是否需要`render-compose --diff`**直接输出与当前`compose.yaml`的差异，供Console在执行前展示给管理员确认？
2. **磁盘水位告警的阈值定多少？**受§4.4影响升为必要功能。节点规格已定为80 GB盘 + `3 × 15 GiB = 45 GiB` workspace，非workspace开销13–17 GiB，余量约17 GiB（§7.6）；建议按可用空间80%/90%两级告警，但需结合实际部署确认。

---

## 10. 与Console的衔接

本命令是设计§2.2「节点侧动作必须是幂等的单条命令」的一个实例。Console将来通过SSH这样调用：

```bash
ccwadmin render-compose --projects a,b,c --out /srv/ccw/compose.yaml
docker compose -f /srv/ccw/compose.yaml up -d
```

因此本计划完成后，设计文档C13的实现主体即完成，Console侧只剩「组装slug列表并调用」的编排逻辑。

**Console需要向管理员提示的两件事**（对应§2.5与§8的B9）：加项目会短暂断开现有客户的终端；tmux现场不丢，重连即恢复。
