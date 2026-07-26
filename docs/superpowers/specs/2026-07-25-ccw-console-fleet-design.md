# ccw v2设计：Console控制平面（用户站点 + 机队管理后台）

**状态：**草案，**未实施**（本文档写作时零代码）
**创建：**2026-07-25　**最后修订：**2026-07-26（增补§6域名体系与客户端寻址、§7多客户共用节点模型）
**分支：**`v2`
**决策依据：**用户2026-07-25与2026-07-26的明确要求；关键选型见§1.3

---

## 0. 文档地位

本文档是对现有权威链的**扩展**，不是替换。冲突时优先级：

1. `2026-07-19-remote-claude-workspace-v2-review-adjustments.md`（安全与审查结论，最高）
2. `2026-07-19-remote-claude-workspace-audit-corrections.md`
3. `2026-07-19-remote-claude-workspace-design.md`（Design Spec v3）
4. **本文档**（Console层设计）
5. `2026-07-19-remote-claude-workspace-plan.md`

**与CLAUDE.md「非目标」的冲突需要显式记录：**现有CLAUDE.md写明非目标包含「多租户」。本设计引入机队管理，但**不引入多租户**——仍是单管理员、每个节点独立自治。实施启动时须在CLAUDE.md把非目标改写为「多租户（多个互不信任的付费客户共用一套控制面与计费）」，并补一句「机队管理由Console负责，见本文档」。在改之前，本条冲突以本文档为准。

**与`docs/STATUS.md`的关系：**Console**不会**自动修复STATUS里的P0-1（用量采集未接线）与P1-1（cleanup模式未实施）。Console把这些缺口从「一台机器上没接」放大成「每台机器上都没接」。§12.3列出了两者的先后关系。

---

## 1. 目标

### 1.1 用户端站点

一个公网可访问的产品页，普通用户不需要任何解释就能：

- 明白ccw是什么（云端Claude Code工作空间，断线不中断，本地目录双向同步）
- 下载对应平台的`cclaude`客户端（macOS/Windows/Linux，amd64/arm64）
- 校验下载完整性（SHA256）
- **凭CDK查到自己该连哪个API域名**（§6.6）
- 看懂拿到CDK之后该做什么（三步快速开始）

### 1.2 管理后台

管理员在浏览器里完成**过去必须SSH上机手敲的全部动作**：

| 动作 | 现状 | 目标 |
|---|---|---|
| 新服务器纳管 | 手动SSH、手动装Docker | 填IP/用户名/密码，点「开始部署」 |
| 环境初始化 | 照`DEPLOY.md`逐条敲 | 流水线自动执行，浏览器实时看日志 |
| 部署服务栈 | `docker compose up -d` | 同上，失败可从断点重跑 |
| 分配子域名 | 手动想名字、手动加DNS | Console自动分配序号并建A记录（§6） |
| 配置域名 | 改`.env`、重启caddy、盯证书 | 流水线自动完成，证书就绪后提示 |
| 管理多个域名 | 无 | 域名（zone）管理页，含证书预算监控（§6.5） |
| 生成CDK | SSH进容器跑`ccwadmin` | 点「签发CDK」，明文只显示一次 |
| 查看机队状态 | 无 | 节点列表：在线/版本/磁盘/项目数 |

### 1.3 已定选型

| 决策点 | 选定方案 | 定于 |
|---|---|---|
| 管理后台暴露方式 | **公网独立域名 + 密码+TOTP + IP白名单**（§8） | 07-25 |
| 目标服务器SSH凭据 | **首次用密码，随后换成托管ed25519密钥；密码用完即弃、永不落库**（§9） | 07-25 |
| Web前端栈 | **Go html/template + 原生JS，`go:embed`打包，零Node工具链** | 07-25 |
| DNS托管 | **AWS Route 53**；但DNS自动化做成可选，`manual`模式为默认实现（§6.3） | 07-26 |
| 客户端寻址 | **域名不写死**，用户首次输入或`--api`指定，之后持久化本地（§6.7） | 07-26 |
| 域名数量 | **支持多个zone**，Console统一分配子域名与监控证书预算（§6.1、§6.5） | 07-26 |
| 节点共用 | **一台VPS一套服务、多客户共用**；Console独立一台只跑后台与官网（§7.1） | 07-26 |
| Claude账号 | **你提供账号，同节点客户共用一个上游池**；靠内部配额做公平分配（§7.2、§7.4） | 07-26 |
| 计量隔离 | **共享HOME不变，只把`.claude/projects`挂成每客户独立卷**——绕开未验证的R2风险（§7.3） | 07-26 |
| 容器隔离强度 | **按「客户可信」处理**，不上gVisor/Kata；风险写入服务条款（§7.5） | 07-26 |

### 1.4 非目标

- 多租户计费、LLM Gateway代理
- Kubernetes、服务网格、自动扩缩容
- Windows/macOS作为**被管理节点**（仅支持Linux节点，见§9.2的发行版白名单）
- 节点之间的负载均衡或项目迁移
- Console的高可用（单实例，靠备份恢复）

---

## 2. 架构总览

```
                          ┌─────────────────────────────────────┐
                          │  Console主机（控制平面，独立一台）      │
   浏览器（管理员）  ──────▶│  caddy ──▶ ccw-console:8090         │
   浏览器（普通用户）──────▶│           ├─ 公开站点 / 下载 / 查询   │
                          │           ├─ 管理后台 /admin/*       │
                          │           ├─ SSH执行引擎             │
                          │           └─ DNS Provider（Route53） │
                          │  postgres（console独立库）            │
                          └───────┬──────────────────┬──────────┘
                       SSH(22)    │                  │ Route53 API
                   托管ed25519密钥  │                  ▼
                     ┌────────────┼─────────┐   ┌──────────────┐
                     ▼            ▼         ▼   │ example.com  │
              ┌────────────┐ ┌────────┐ ┌──────┐│ api-01 A ... │
              │  api-01    │ │ api-02 │ │api-03││ api-02 A ... │
              │ caddy:443  │ │        │ │      │└──────────────┘
              │ control-api│ │ （同左） │ │（同左）│
              │ worker-agent│ │        │ │      │
              │ postgres   │ │        │ │      │
              │ project容器 │ │        │ │      │
              └────────────┘ └────────┘ └──────┘
                     ▲
                     │ wss://api-01.example.com/ws/terminal
                cclaude（本地CLI，直连节点，不经Console）
```

### 2.1 核心决策：节点自治，Console只做编排

**决策：**每个节点保留自己的PostgreSQL与完整栈（即现在`deploy/compose.yaml`那一套）。Console有**独立的库**，只存机队元数据。

**理由：**

- 备选方案「所有节点共用Console的中心库」要求节点跨公网连PG，直接违反「服务默认只监听回环地址」（CLAUDE.md不可违反规则）。要么暴露5432，要么维护N条隧道，两者都是净增攻击面。
- 节点自治使Console宕机时**已部署节点完全不受影响**：终端、同步、额度照常。Console只是运维工具，不在用户数据面上。
- 现有代码零改动即可作为节点使用（`control-api`/`worker-agent`/`ccwadmin`都假设本地库）。

**代价：**跨节点聚合查询（例如「全机队总用量」）需要Console主动轮询各节点，有延迟且非事务一致。个人版可接受，§11.4给出轮询设计。

### 2.2 核心决策：Console→节点只走SSH，节点不开任何新端口

Console执行节点侧动作的唯一通道是SSH，命令形如：

```bash
docker compose -f /srv/ccw/compose.yaml exec -T control-api ccwadmin issue-cdk --slug=demo --json
```

**理由：**备选方案「节点跑一个admin API」需要新端口、新认证体系、新令牌，攻击面净增；而SSH本来就必须开着（否则管理员根本救不了机器）。SSH-only意味着Console被攻破的爆炸半径**等于**管理员SSH私钥泄露的爆炸半径——没有变大。

**约束：**因此`ccwadmin`必须提供机器可读输出（§11.1），且所有节点侧动作必须是**幂等的单条命令**，不能依赖交互式输入。

### 2.3 核心决策：Console不在用户数据路径上

`cclaude`直连节点域名，**不经过Console**。Console只在两处出现在用户视野：下载客户端、凭CDK查询自己的API域名（§6.6）——都只发生在首次配置。

这条性质是可验证的验收标准（§13的A19）：Console停机时，已配置好的客户端一切照常。

### 2.4 新增二进制

| 二进制 | 监听 | 职责 |
|---|---|---|
| `ccw-console` | `127.0.0.1:8090` | 公开站点、下载分发、CDK查询、管理后台、SSH执行引擎、DNS编排、节点巡检 |

沿用现有三进程的风格：只监听回环，公网入口只有Caddy。

---

## 3. 用户端站点

### 3.1 路由

| 路径 | 内容 |
|---|---|
| `GET /` | 落地页：一句话价值主张、三步快速开始、特性区块、下载CTA |
| `GET /download` | 下载页：UA自动识别推荐平台 + 全平台表格 + SHA256 |
| `GET /download/{os}/{arch}` | 302重定向到具体产物 |
| `GET /dist/{filename}` | 产物本体（Console本地磁盘`CCW_DIST_DIR`） |
| `GET /dist/SHA256SUMS` | 校验和清单（纯文本，可直接`shasum -c`） |
| `GET /connect` | **CDK查询页**：粘贴CDK查出对应API域名与完整命令（§6.6） |
| `POST /v1/resolve` | 查询页的后端；**只接收CDK的public-id部分**（§6.6） |
| `GET /quickstart` | 拿到CDK之后怎么用：安装、首次运行、CDK与域名输入 |
| `GET /healthz` | 存活探针 |

**落地页内容边界（合规）：**遵守CLAUDE.md「用量对外一律称内部额度单位，不得标注为官方订阅百分比」。落地页描述额度时用「内部额度单位」，不得出现任何暗示官方订阅额度的措辞。

### 3.2 产物与发布流程

`Makefile`新增target：

```make
release: ## 交叉编译六目标 + 生成校验和
	VERSION=$(VERSION) ./scripts/build-release.sh
```

六个目标：`darwin/amd64`、`darwin/arm64`、`linux/amd64`、`linux/arm64`、`windows/amd64`、`windows/arm64`。

- 版本经`-ldflags "-X main.version=$(VERSION)"`注入，`cclaude --version`可查
- 产物命名：`cclaude_{version}_{os}_{arch}[.exe]`
- `SHA256SUMS`与产物同目录生成
- 构建完成后写`releases`与`release_artifacts`表（§10），下载页从表里渲染，**不扫目录**——避免半成品文件被下载

**注意：**二进制里**不再注入任何域名**（§6.7）。同一份产物对所有用户、所有节点通用。

**版本固定：**遵守CLAUDE.md禁令，构建用的Go版本记入`deploy/versions.lock`。

### 3.3 前端实现

- `internal/console/templates/`下的html/template，`//go:embed`嵌入（与`internal/control/http.go:20`同款做法）
- CSS单文件、原生JS，无外部CDN引用（下载页不应因第三方CDN不可达而挂掉）
- 深浅色都要可读；表格在窄屏横向滚动而非撑破页面

---

## 4. 管理后台功能面

### 4.1 页面

| 路径 | 页面 |
|---|---|
| `GET /admin/login` | 密码 + TOTP登录 |
| `GET /admin` | 机队总览：节点卡片（在线状态、域名、项目数、磁盘、最近部署） |
| `GET /admin/nodes/new` | 新增节点向导（§5.1） |
| `GET /admin/nodes/{id}` | 节点详情：基本信息、项目列表、部署历史、操作按钮 |
| `GET /admin/nodes/{id}/runs/{run}` | 流水线运行详情 + 实时日志（SSE） |
| `GET /admin/zones` | **域名管理**：zone列表、provider配置、证书预算水位（§6.5） |
| `GET /admin/zones/{id}` | zone详情：已分配子域名、DNS记录状态、本周签发计数 |
| `GET /admin/projects` | 全机队项目列表（跨节点聚合） |
| `GET /admin/releases` | 发布管理：已发布版本与产物 |
| `GET /admin/audit` | 审计日志 |

### 4.2 节点详情可执行的操作

每个都对应一条幂等流水线（§5.3）：

- **重新部署**（拉最新产物、`compose up -d`）
- **更换子域名/迁移zone**
- **新建项目**（→节点`ccwadmin init-project`；达到3个上限时按钮禁用并说明原因，§7.6）
- **签发CDK**（→节点`ccwadmin issue-cdk`，明文一次性展示）
- **轮换CDK**（→节点`ccwadmin rotate-cdk`；默认24小时宽限，另提供「立即撤销」用于凭据泄露，§11.1.1）
- **禁用CDK**
- **查看磁盘水位**（含告警阈值与当前占用；硬配额已决定不做，见§12.1的N4改写说明）
- **收集诊断**（`docker compose ps`、磁盘、日志尾部）
- **停止/启动栈**
- **解除纳管**（移除托管公钥、删DNS记录、删Console侧记录；**不删节点数据**）

---

## 5. 节点纳管与Provisioning流水线

### 5.1 新增节点向导

三步表单：

1. **连接信息**：IP/主机名、SSH端口（默认22）、用户名、密码
2. **节点配置**：节点显示名、**选择zone**（Console自动分配下一个子域名序号，可覆盖）、初始项目slug列表、每项目磁盘配额与额度上限
3. **确认**：展示即将执行的步骤清单与将要创建的DNS记录，点「开始」

提交后立刻跳转到运行详情页看实时日志。

### 5.2 SSH连接与host key信任

- **TOFU（首次使用即信任）**：首次连接记录host key指纹到`nodes.host_key_fp`，向管理员展示指纹供带外核对
- 后续连接指纹**不匹配即中止**，标记节点为`host_key_changed`，需管理员显式确认才能继续（防MITM）
- 拒绝`ssh.InsecureIgnoreHostKey()`——任何情况下都不允许

用`golang.org/x/crypto/ssh`（`x/crypto`已在`go.mod`中，不新增依赖树顶层）。

### 5.3 bootstrap流水线步骤

每步都必须满足三条性质：**幂等**（重复执行结果相同）、**可precheck**（已满足则跳过并标记`skipped`）、**可断点续跑**（从第一个失败步重开）。

| # | 步骤 | 动作 | precheck |
|---|---|---|---|
| 1 | `connect` | 密码登录、记录host key、探测sudo可用性 | — |
| 2 | `probe` | 采集发行版/内核/arch/内存/磁盘/已装Docker/端口22·80·443占用 | — |
| 3 | `harden` | 建`ccw`运维用户、注入托管公钥、验证密钥登录、**丢弃密码**、防火墙放行22/80/443 | 公钥已在`authorized_keys` |
| 4 | `install-docker` | 按`versions.lock`固定版本装Docker CE + compose插件；**写`/etc/docker/daemon.json`把`data-root`指向独立分区/盘**（N4） | `docker --version`匹配锁定版本且`data-root`已指向独立分区 |
| 5 | `dns-allocate` | 从zone分配子域名、创建/UPSERT A记录、等待生效（§6.3） | 记录已存在且指向本节点IP |
| 6 | `push-artifacts` | 上传`compose.yaml`/`Caddyfile`/`Dockerfile.*`到`/srv/ccw`（**不含`quota-setup.sh`**，见§12.1的N4改写说明） | 文件sha256全部匹配 |
| 7 | `render-env` | **在节点上**生成`POSTGRES_PASSWORD`与`CCW_TOKEN_KEY`（`openssl rand -hex 32`），写`/srv/ccw/.env`（0600） | `.env`存在且变量齐全 |
| 8 | `compose-up` | `docker compose up -d --build` | — |
| 9 | `cert-wait` | 轮询443 TLS握手直到证书就绪，记录签发者与有效期（§6.5） | 现有证书未过期 |
| 10 | `healthcheck` | 容器healthy + `/api/v1/...`可达；随后`docker builder prune -f`回收2–5 GiB构建缓存（§7.6） | — |
| 11 | `init-projects` | 对每个slug跑`ccwadmin init-project --json`，回收CDK明文 | slug已存在则跳过 |
| 12 | `disk-guard` | 校验Docker`data-root`确在独立分区、Postgres数据目录不在data-root上；注册磁盘水位告警阈值（N4） | 两项校验通过且阈值已注册 |

**步骤12曾是`quota-setup`（执行`deploy/quota-setup.sh`），已删除。**该脚本在当前卷布局下执行了也不生效，接进流水线只会制造「配额已启用」的假象——原因与替代方案见§12.1的N4改写说明。

**关键约束1：**步骤7的密钥**在节点本地生成**，明文从不经过Console。Console只知道「已生成」，不知道值。这样Console库泄露不等于节点令牌泄露。

**关键约束2：**`dns-allocate`（步骤5）必须**排在`compose-up`之前并且失败即阻断**。理由见§6.5：DNS没生效就起Caddy会连续触发Let's Encrypt验证失败，撞上「每标识符每小时5次失败」限额后即使DNS修好也要等一小时。

### 5.4 实时日志

- 每步stdout/stderr行式写入`/var/lib/ccw-console/logs/{run_id}/{seq}-{name}.log`，同时经内存channel广播
- 浏览器用**SSE**（`text/event-stream`）订阅`GET /admin/nodes/{id}/runs/{run}/stream`
- **日志脱敏是硬要求**：写盘与推流前都经过`redact()`，命中密码、私钥、`CCW_TOKEN_KEY`、CDK明文、`POSTGRES_PASSWORD`、云厂商AccessKey一律替换为`[REDACTED]`。这是CLAUDE.md「凭据永远不进日志」规则在Console上的延伸，须有专门单测。

---

## 6. 域名体系与客户端寻址

> 本节为2026-07-26增补，回应「一个主域名 + 每节点子域名，且不想为每个客户注册新域名」的需求。

### 6.1 为什么必须每节点一条A记录

**通配符记录解决不了这个问题。**`*.example.com A 1.2.3.4`只能指向单一IP，而每台节点有各自的公网IP。让通配符指向Console再由Console反代到各节点也不可行——那会把Console放进用户数据路径，直接摧毁§2.3的性质（Console宕机则全员断线），并且Console要承担全部终端与同步流量。

因此：**每个节点一条独立A记录**。要做到「管理员不动手」，就必须由Console调用DNS服务商API自动维护这些记录。

### 6.2 多zone模型与子域名分配

Console管理**一个或多个**DNS zone（`example.com`、`example.net`……）。每个节点归属一个zone，获得一个子域名。

**命名方案：**`api-01.example.com`、`api-02.example.com`……**从第一台就带序号，不设特例。**

用户最初设想第一台叫`api.example.com`、第二台才叫`api-01`。不建议：那会让第一台成为永久特例，代码里到处要判断「是不是第一台」，文档和运维脚本也要各写两遍。统一带序号的成本是零，特例的成本是永久的。若确实想要`api.example.com`这个好记的名字，作为**CNAME别名**指向`api-01`，并在该节点Caddyfile里同时声明两个名字（否则用户直连别名会证书不匹配）。

**分配规则：**

- 序号由Console的分配表统一发放，zone内单调递增
- **序号永不回收**：节点退役后其序号作废，不再分配给新机器
  - 这是**安全属性**不是洁癖：客户端会把域名持久化到本地（§6.7），旧客户端或旧文档里残留的`api-07.example.com`若被重新分配给新客户的机器，会导致误连
- **保留名单**（不可分配给节点）：`www`、`admin`、`api`、`app`、`docs`、`status`、`mail`、`ns*`、`_acme-challenge`、`_dmarc`、以及`CCW_SITE_DOMAIN`/`CCW_ADMIN_DOMAIN`已占用的名字

### 6.3 DNS Provider抽象与Route 53

定义接口，多实现：

```go
type DNSProvider interface {
    // UpsertA 幂等地把 name 指向 ip；返回后调用方需 WaitPropagated
    UpsertA(ctx context.Context, zone Zone, name, ip string, ttl int) (changeID string, err error)
    DeleteA(ctx context.Context, zone Zone, name, ip string) error
    WaitPropagated(ctx context.Context, zone Zone, changeID string) error
    CheckCAA(ctx context.Context, zone Zone) (allowsLetsEncrypt bool, err error)
}
```

**实现一：`manual`（默认，零依赖）**
Console展示待添加的记录，管理员到控制台手动添加，点「校验」。校验用**至少两个独立公共解析器**（`1.1.1.1`、`8.8.8.8`）交叉查询，都指向节点IP才通过。

**实现二：`route53`**

- API：`ChangeResourceRecordSets`用`UPSERT`动作——**天然幂等**，正好满足§5.3对每一步的要求；重跑不会报「记录已存在」
- 生效判定：`GetChange`轮询到`INSYNC`。这比猜传播时间可靠，也比查公共解析器准确（`INSYNC`是Route 53权威应答已更新的确证）
- TTL设60秒：加快首次校验，也让将来换IP能快速生效
- 凭据：AWS AccessKey，AES-GCM加密存库（复用§8.4的信封加密）
- **IAM权限必须收敛到最小**：只给`route53:ChangeResourceRecordSets`、`route53:ListResourceRecordSets`、`route53:GetChange`，且`Resource`限定到具体的`arn:aws:route53:::hostedzone/<ID>`。不要用管理员AccessKey——Console被攻破时，一个宽权限的AWS凭据会把爆炸半径从「机队」扩大到「整个AWS账户」

**关于依赖：`manual`是默认实现，Route 53是可选。**引入`aws-sdk-go-v2`会是本仓库目前最大的一次依赖扩张（现在`go.mod`只有pty、uuid、websocket、pgx、x/crypto、x/term六项）。做成接口意味着：系统第一天就能用`manual`模式跑通全流程，Route 53的接入可以随时做、也可以永远不做。这正好对应「不好接入也可以手动添加」的要求。

### 6.4 退役时必须删除DNS记录

**子域名接管风险：**节点退役、VPS释放后，云厂商会把那个IP分配给别人。若`api-07.example.com`的A记录还悬挂着指向该IP，拿到这个IP的人就**继承了你的子域名**——可以为它签发合法证书、可以架钓鱼页面，而浏览器地址栏显示的是你的域名。

因此：

- 「解除纳管」流水线**必须**包含`dns-teardown`步骤
- 删除DNS记录必须**先于**释放云主机，顺序不可颠倒
- Console定期巡检（§11.4）时校验每条已分配记录仍指向已知节点IP，不一致即告警

### 6.5 证书预算管理

Let's Encrypt的限额是**按注册域名**计的，`example.com`下所有子域名共享同一份预算。核实于2026-07-26（[LE官方文档](https://letsencrypt.org/docs/rate-limits/)，页面标注最后更新2025-06-12）：

| 限额 | 数值 | 回填速率 |
|---|---|---|
| 每注册域名证书数 | **50张 / 7天** | 每202分钟1张 |
| 完全相同标识符集合的重复证书 | **5张 / 7天** | 每34小时1张 |
| 每标识符授权失败 | **5次 / 小时**（每账户） | 每12分钟1次 |
| 每账户新订单 | 300 / 3小时 | 每36秒1次 |

**真实容量比直觉大得多。**Caddy对90天证书在第60天续期，即每节点约每8.6周消耗1张。N台节点的稳态续期速率约`N / 8.6`张/周。50张/周的预算在稳态下支撑**约400台节点**，而不是50台。

**真正会撞限的是三个场景：**

1. **开量突发**：单周新增超过50台节点
2. **反复重部署同一节点**：撞的是「重复证书5张/周」，这是最容易踩的一个——同一个域名反复重签，第6次就被挡
3. **大规模重签事件**：例如误删证书卷导致全机队重新签发

**因此定为硬规则：**

- **重新部署流水线禁止使用`docker compose down -v`**，`caddy-data`卷必须持久保留。丢卷＝丢证书＝重签
- **禁止启用Let's Encrypt短期（6天）证书。** 那会让每节点每周续期1次，`N`台＝`N`张/周，50台就撞顶。Caddy默认不启用，只要不主动配置即可，但须在Caddyfile模板加注释锁死这个决定
- **e2e测试必须走staging CA**（新增`CCW_ACME_CA`环境变量）。反复跑bootstrap验收会烧掉真实预算，且撞上重复证书限额后测试自身会失败
- **Console记账并预警**：`cert_issuances`表按zone滚动统计7天签发数，超过40张时后台告警，超过48张时**阻断新节点分配到该zone**（留余量给存量节点的续期，续期失败比新节点上不去严重得多）
- **CAA预检**：zone接入时检查CAA记录是否允许`letsencrypt.org`。若`example.com`设了限制其他CA的CAA，该zone下所有节点证书都会失败——一次性检查，避免逐台排查

**多zone的价值定位：**主要是突发容量与故障隔离（一个zone的DNS或CAA配错不影响其他zone），而不是「50台就得换域名」。按上面的数学，多zone在数百台规模前不是必需品——但schema和分配逻辑现在就按多zone设计，后面加zone不用改结构。

### 6.6 CDK查询页（`/connect`）

用户拿到CDK后需要知道连哪个域名。管理员签发时可以直接给出完整指引，但用户会弄丢——查询页是兜底。

**流程：**用户粘贴CDK → 页面显示对应的API域名 + 可直接复制的命令：

```
cclaude --api https://api-03.example.com
```

**安全设计（这是本页面的核心，不是附加项）：**

CDK格式是`ccw_<publicID>.<secret>`（`internal/auth/cdk.go:36`）。查询只需要`publicID`，`secret`绝不需要离开用户设备。

- **页面JS在浏览器本地切分CDK，只POST `publicID`给Console。**`secret`永不上网、不进Console日志、不进Console库
- Console侧`cdk_issues.public_id`（§10）本来就有这份数据，无需新增存储
- 页面必须**显式告知用户**只上传了前半段，并在输入框上标`autocomplete="off"`
- 服务端做防御性校验：收到的字符串若包含`.`（说明前端切分逻辑被绕过或用户直接调API），**立即拒绝并且不记录请求体**
- 限速：每IP每分钟10次。`publicID`是8字节即2^64空间，枚举不可行，但限速仍是必需的
- 查不到时统一返回「未找到」，不区分「不存在/已禁用/已撤销」——延伸CLAUDE.md的`invalid_cdk`统一错误规则

**这是唯一把Console放进用户路径的地方，且仅限首次配置。**配置完成后Console停机不影响任何已有用户（§13的A19）。

### 6.7 客户端寻址：域名不写死

**现状问题：**`cmd/cclaude/main.go:31`是`base := envOr("CCW_API", "https://ccw.example.com")`——一个写死的默认值。官网下载的二进制对所有人完全相同，写死域名在多节点下根本行不通。

**方案（用户2026-07-26确定）：**域名作为用户配置项，首次使用时输入，之后持久化本地。

**为什么这比「把节点码嵌进CDK」好**（我原先倾向的方案）：

1. **域名可变更而无需重签CDK**。节点迁移、换zone、甚至整体换域名，用户改一行配置即可；嵌进CDK则所有CDK作废
2. **Console不进登录路径**。嵌节点码需要客户端解析规则与Console的分配规则保持一致，任何不一致都是线上事故；用户直接持有域名则没有这层耦合
3. **CDK保持单一职责**：它是凭据，不是路由信息

代价是用户首次要输两样东西而不是一样。用`/connect`页面给出可复制的完整命令来抵消（§6.6）。

**实现要点：**

- **删除写死的默认值。**无配置时不再回落到`ccw.example.com`，而是进入首次配置流程
- 配置优先级：`--api`命令行参数 > `CCW_API`环境变量 > `~/.ccw/config.json` > 交互提示
- `~/.ccw/config.json`（0600）取代现在的单行文件`~/.ccw/cdk`（`cmd/cclaude/main.go:225`）：

  ```json
  {"api": "https://api-03.example.com", "cdk": "ccw_..."}
  ```

  **兼容迁移：**启动时若发现旧的`~/.ccw/cdk`且无`config.json`，自动读入并转换，转换后删除旧文件
- **CDK永远不做命令行参数。**命令行参数会进shell history，在Linux上还会出现在`ps`输出里被同机其他用户看到。CDK只接受交互输入（`term.ReadPassword`，现有代码已是如此）或`CCW_CDK`环境变量。**域名可以做参数**——它不是机密
- `cclaude --api https://api-05.example.com`显式指定时覆盖并重写本地配置（支持用户换节点）
- 新增`cclaude logout`：清除本地配置
- 首次交互提示的顺序是**先域名后CDK**：域名输错立刻能从连接失败看出来，CDK输错则要等到认证阶段

---

## 7. 多客户共用节点：隔离、账号与配额

> 本节为2026-07-26增补。用户确认的形态：**一台VPS部署一套服务、供多个客户共用**；账号模型为**「你提供Claude账号，客户共用一个上游池」**；容器隔离强度按**「客户可信」**处理。

### 7.1 三层隔离，现状只成立一层

「用容器隔离客户」这个说法要拆成三层看，三层的成立条件完全不同：

| 层 | 靠什么实现 | 现状 |
|---|---|---|
| **文件隔离** | 项目容器 + 每项目独立workspace卷 | ✅ 已成立（非root、有资源上限，Task 4） |
| **计量隔离** | 每客户独立的会话JSONL | ⚠️ 结构上已成立（`4093e3d`增加`<slug>-claude-projects`嵌套卷，§7.3），但**未在真实部署上验证**，且端到端计量仍依赖采集器接线（P0-1，未做） |
| **额度隔离** | 上游Claude账号 | ❌ 一个节点一个账号＝物理上一个池子，**代码层面无法隔离** |

第二层可以修（§7.3），第三层修不了——只能靠内部配额做公平分配（§7.4）。

### 7.2 账号模型：你提供账号，客户共用一个上游池

**已定（2026-07-26）。**你出Claude账号，节点上多个客户共用它的上游额度。

这个选择的直接后果：**内部额度闸门从「锦上添花」变成「不可缺少」**。它是唯一能阻止某个客户吃掉全机额度的机制。另外两个备选模型（客户自带账号、一客一机）都不需要闸门就能天然隔离，这个模型需要。

**一个被绕开的好处值得记录：**若选「客户自带账号」，spec风险R2（同账号多容器凭据互踢）根本不存在——不同账号之间没有凭据冲突面。选了共享账号，R2就仍在桌面上。§7.3给出了同样能绕开它的做法。

### 7.3 卷布局：分离JSONL而不触碰凭据

**本机实测的Claude HOME布局（2026-07-26核实）：**

```
~/.claude.json                                  配置（含登录态）
~/.claude/.credentials.json                     OAuth凭据（Linux容器内为文件；macOS走Keychain）
~/.claude/projects/<编码cwd>/<session>.jsonl    ★ 会话JSONL——采集器的数据源
~/.claude/history.jsonl                         命令历史
~/.claude/{shell-snapshots,sessions,file-history,todos,cache,...}/
```

**关键事实：凭据与JSONL位于不同路径，因此可以分别挂载。**

**采纳方案——共享HOME保持不变，只把`projects/`挂成每客户独立卷：**

```yaml
volumes:
  - claude-shared:/home/claude                                  # 不变：凭据继续共享
  - {{.Slug}}-claude-projects:/home/claude/.claude/projects     # 新增：JSONL按客户隔离
```

嵌套挂载，内层路径覆盖外层同路径——这是Docker的标准行为。

**为什么不是「每项目独立HOME卷」（我在本文档早先版本里的提法）：**

独立HOME要求把凭据复制进每个容器，各容器独立刷新OAuth token。若refresh token是一次性轮换的，A刷新后B手里的副本即作废 → 随机掉线。这正是spec风险R2。

而**R2从未被验证过**：实施计划Task 0 Step 1（24小时双登录验证）没做，`docs/STATUS.md`的P1-4记着`dual-login-24h.md`不存在，`docs/design-deviations.md`的D1原话是「**被绕过而非通过**」。

**订正一条记录在案的因果关系：**D1把共享卷的动机记为「规避风险R2（双容器同账号OAuth凭据互踢）」。但`deploy/Dockerfile.claude`的注释给出了当时故障的真实根因：

> 避免默认root:root导致claude无法写入`.credentials.json`（登录后`loggedIn=false`、**反复要求登录的根因**）

即：那次「反复要求登录」是**命名卷ownership问题**，与OAuth凭据轮换无关。两件事在commit `3caa271`里被同时处理，因而在D1中被归并成一个动机。**R2很可能从未真实发生过。**

这不改变本方案的选择（用户要求只授权一次，共享卷必须保留），但有两个后果：

1. `docs/design-deviations.md`的D1须据实订正，否则后续读者会继续把ownership问题当作凭据轮换的证据
2. **同一个ownership陷阱对新增的`projects/`卷完全适用**——见下方实施注意

本方案的价值在于：**凭据的存储位置与刷新路径与今天完全一致，一个字节都不动。**因此不引入任何未验证行为，也**不需要**先做24小时双登录验证。P0-2得以解除，而R2风险面根本没被触碰。

**硬约束（用户2026-07-26重申）：Claude账号在一台VPS上只授权一次，客户增加也不得增加授权次数。**本方案满足该约束，因为凭据所在的`/home/claude/.claude/.credentials.json`与`/home/claude/.claude.json`都留在共享卷里，挂载点`projects/`只是`.credentials.json`的**兄弟目录**，遮蔽范围不包含凭据文件。

**实施注意：**

- `Dockerfile.claude`必须预先`mkdir -p /home/claude/.claude/projects`并`chown claude:claude`。否则Docker以root初始化新命名卷，容器内的`claude`用户写不进去——这是命名卷初始化的经典陷阱。当前Dockerfile只建到`/home/claude/.claude`，须补一级
- 采集器改为**按卷读取**：worker-agent对每个项目只读它自己的`projects`卷。归属由挂载关系天然保证，不需要解析JSONL里的`cwd`字段

**尚需验证（不得当作已完成）：**

1. Claude Code是否会在`projects/`之外写入含usage的记录（`sessions/`目录用途未确认）
2. 嵌套命名卷在容器重建后ownership是否保持
3. 同账号多容器并发写**同一份**凭据文件的行为——今天的双项目部署已经在这么跑，但从未做过24小时以上的观察

第3点是现有部署就存在的状况，本方案不改变它；但它仍是未验证项，客户数上去后应补做观察。

### 7.4 配额、超卖与公平性

**现有数据模型正好对应这个场景，无需改动。**`internal/quota`已经同时计算：

- **项目级5h/7d** = 每个客户的配额
- **账号级池双窗口** = 整台机器上游账号的总容量（`accounts.upstream_pool`字段就是为此而设）

需要改的只是使用方式：`cmd/ccwadmin/main.go:47`现在硬编码`EnsureAccount(ctx, "default", "default-pool")`。多客户下应为**一个节点一个account**（代表该机器的Claude账号），每个客户一个project挂在它下面。

**超卖是可行的商业策略，但前提是闸门真的生效。**卖5个客户各`X/3`配额、账号总容量`X`，即1.67倍超卖。统计复用在多数时段没问题，两道闸门分别兜底：项目级防单客户失控，账号级池防总量被击穿。

**两道闸门2026-07-26已接线**（N1+N2；账号池上限此前无处存储、写死`1<<62`，也由`002`迁移补上）。**但只有单测证据、未真机验证**，且加权系数与限额处于"先记账、后校准"的第一阶段，实际不会拦人。

**因此定为硬约束：P0-1是「共享节点上线」的阻断项，不是技术债。**在它修好之前，一台机器只能服务一个客户。（P0-2已于`4093e3d`解除结构性障碍，但未在真实部署上验证。）否则没有任何机制阻止一个客户吃光额度，而受害的是同机所有客户——他们会同时断线、同时投诉。

后台需展示**超卖水位**：单节点 `Σ(各客户5h配额) / 账号池5h容量`。

### 7.5 隔离性残留缺陷

共享HOME意味着客户之间**互相可见**以下内容：

| 路径 | 内容 | 敏感度 |
|---|---|---|
| `~/.claude/history.jsonl` | 跨项目命令历史 | 中——可能含敏感命令与路径 |
| `~/.claude/shell-snapshots/` | shell环境快照 | 中——可能含环境变量 |
| `~/.claude/sessions/`、`file-history/`、`todos/` | 会话与文件历史 | 中 |
| `~/.claude.json` | 全局配置 | 低 |

用户已确认「客户是可信的」，故按可接受处理。**但建议至少把`history.jsonl`与`shell-snapshots/`也挂成独立卷**——成本是两行compose配置，收益是消除最可能含敏感内容的两处。

另外，`worker-agent`持有docker.sock等同宿主机root：任一项目容器逃逸即导致全机所有客户的数据泄露。用户已确认接受此风险，不做gVisor/Kata等技术加固——**因此必须写入风险清单（§14）与对客户的服务条款**，而不是默认它不存在。

### 7.6 节点容量规划与硬性上限

**产品规则（用户2026-07-26定，不可由代码或配置绕过）：**

| 规则 | 数值 | 强制点 |
|---|---|---|
| 单节点**项目容器**数上限 | **3** | 渲染器拒绝第4个；`ccwadmin init-project`拒绝第4个；Console前后端双校验 |
| 单项目磁盘配额上限 | **15 GiB** | `ccwadmin`参数校验；Console档位不得超过 |
| 单项目磁盘配额默认值 | **15 GiB** | **代码尚未跟进**：`cmd/ccwadmin/main.go:27`仍是`argInt(3, 20)` |

「3个容器」指的是**项目容器**（`ccw-<slug>`）。节点上另有`postgres`、`control-api`、`worker-agent`、`caddy`四个基础设施容器，不计入该上限。

**推论——节点磁盘规格核算（2026-07-26，估算未实测）：**

非workspace开销的构成（这一项此前被低估）：

| 项 | 占用 | 备注 |
|---|---|---|
| Ubuntu 24.04 + Docker Engine | ~8 GiB | |
| 运行镜像（postgres/caddy/ccw-claude/两个Go服务） | ~2 GiB | |
| **build stage镜像 + build cache** | **2–5 GiB** | 三个镜像都在节点本地build（`compose.yaml:28,44,79`），`control-api`与`worker-agent`用`golang:1.22-bookworm`做build stage（~840 MB），加Go module与npm缓存 |
| Postgres数据 | 1–2 GiB | 随`usage_events`增长 |
| **小计** | **13–17 GiB** | prune掉build cache后约11–13 GiB |

对照常见规格（80 GB标称盘实际约74.5 GiB）：

| 配置 | workspace | 余量 | 评价 |
|---|---|---|---|
| 80 GB盘 + 3×20 GiB | 60 GiB | **~2 GiB** | **过紧**，必须prune才能跑，余量吃不下Postgres一年的增长 |
| **80 GB盘 + 3×15 GiB** | **45 GiB** | **~17 GiB** | **← 已选定** |
| 80 GB盘 + 2×20 GiB | 40 GiB | ~22 GiB | 舒适，但每客户成本上升50% |
| 100 GB盘 + 3×20 GiB | 60 GiB | ~17 GiB | 未选：需改采购规格 |

**已定（用户2026-07-26）：采购80 GB盘，单项目配额15 GiB，每节点3个客户。**即上表第二行——workspace合计45 GiB，余量约17 GiB。选择依据：80 GB是既有采购规格，改配额数字比改采购便宜；15 GiB对代码仓库类工作负载足够。

**无论选哪种，bootstrap都应在`compose-up`之后执行`docker builder prune -f`**（可立即回收2–5 GiB），并把该步骤纳入§5.3流水线。更彻底的优化是改为中心构建 + registry分发，避免每个节点各build一次golang镜像——但那需要额外的registry基础设施，暂不做。

`probe`步骤应据实际选定的规格校验可用磁盘，不足即告警。

**必须说清的边界：15 GiB是逻辑配额，不是硬配额。**

按§4.4的决定（`plans/2026-07-26-compose-render-plan.md`），本阶段**不启用**loop-mount硬配额。因此该上限只对**走同步接口的文件**生效（由`internal/storage`记账），对**容器内直接写入**无效——客户在终端里`npm install`、堆构建缓存或直接`dd`，可以突破15 GiB并撑爆宿主机磁盘。

这不是设计缺陷而是明示的取舍，但有两个后果必须承担：

1. 对外**不得**把15 GiB表述为「保证不超过」，只能说「配额」——延伸CLAUDE.md「未验证的功能不写成已完成」与「不得越界承诺」的要求
2. **磁盘水位告警因此是必要功能而非可选项**，并入§11.5节点巡检

**其余容量约束（不构成硬性上限，但影响实际可售数量）：**

- **内存/CPU**：项目容器已有资源上限（Task 4已完成）
- **上游账号额度**：通常这才是真正的瓶颈，见§7.4的超卖水位

---

## 8. 管理后台安全模型

管理后台能SSH到机队任意机器、能改DNS，**权限等同全机队root**。安全要求高于现有任何组件。

### 8.1 认证

- `admin_users`：`username` + Argon2id `password_hash` + AES-GCM加密的`totp_secret_enc`
- 登录需**密码 + TOTP六位码**，缺一不可
- 认证失败统一返回`invalid_credentials`，**不区分**用户不存在/密码错/TOTP错——延伸CLAUDE.md的`invalid_cdk`统一错误规则
- 登录限速：每IP每分钟5次、每用户名每分钟5次（复用`internal/control/http.go:75`的滑动窗口思路），超限指数退避

### 8.2 会话

- 服务端会话表（`admin_sessions`），**不用无状态HMAC**
- 理由：管理面必须能**立即撤销**（设备丢失、人员变动）。无状态令牌在TTL内无法吊销——这与连接令牌的场景不同：那里选无状态是因为2分钟TTL够短且worker每次实时复查额度（CLAUDE.md已写明），管理会话两个前提都不成立
- Cookie：`HttpOnly` + `Secure` + `SameSite=Strict`，绝对超时12小时、空闲超时30分钟
- 所有写操作需CSRF token

### 8.3 网络层

Console主机的Caddyfile按域名分流：

```
{$CCW_SITE_DOMAIN} {
	# 公开站点、下载、CDK查询：任何人可访问
	reverse_proxy ccw-console:8090
}

{$CCW_ADMIN_DOMAIN} {
	@allowed remote_ip {$CCW_ADMIN_ALLOWLIST}
	handle @allowed {
		reverse_proxy ccw-console:8090
	}
	respond 404      # 白名单外：404而非403，不暴露后台存在
}
```

**双重校验：**应用层也必须独立检查客户端IP白名单，不能只依赖Caddy——反代配置改错时应用层要兜底。

**路径合同：**`CCW_ADMIN_DOMAIN`与`CCW_SITE_DOMAIN`必须是**不同域名**。同域名下用路径前缀隔离，在Caddy配置写错时会直接把后台暴露给公网，风险不对称。

### 8.4 凭据存储

| 数据 | 存储方式 |
|---|---|
| 目标服务器密码 | **永不落库**。仅存在于`harden`步骤的进程内存，用完置零 |
| 托管ed25519私钥 | AES-256-GCM信封加密存库，密钥来自`CCW_SECRET_KEY`（32字节hex） |
| AWS AccessKey（Route 53） | 同上；IAM权限收敛到单个hosted zone |
| TOTP secret | 同上 |
| 管理员密码 | Argon2id哈希（复用`internal/auth`） |
| 节点`CCW_TOKEN_KEY` | **Console完全不持有**（§5.3步骤7） |
| CDK明文 | **不落Console库**。签发后一次性渲染到浏览器，刷新即不可见 |
| CDK的secret部分 | **绝不接收**。`/connect`只收public-id（§6.6） |

`CCW_SECRET_KEY`遵循现有约定：`config.Load`缺失即硬失败，无默认值。

### 8.5 审计

所有管理动作写`audit_log`：actor、action、target、结果、客户端IP、时间（数据库`now()`、UTC，遵守CLAUDE.md）。审计写入失败时**动作也失败**——不允许存在无审计的特权操作。

---

## 9. 凭据生命周期（首次密码→托管密钥）

```
管理员填 IP/user/password
        │
        ▼
[connect] 密码登录，记录host key指纹
        │
        ▼
[harden] Console生成ed25519密钥对（内存中）
        ├─ 公钥 append 到节点 ~/.ccw/authorized_keys
        ├─ 用私钥重新拨号验证登录成功
        ├─ 验证通过 → 私钥 AES-GCM 加密落库
        └─ 密码缓冲区置零，永不落库、永不进日志
        │
        ▼
后续所有操作只用托管密钥
```

**密钥轮换：**后台提供「轮换密钥」操作——生成新密钥对、注入、验证、删旧公钥、更新库。失败时保留旧密钥可用（先加后删，绝不先删）。

**解除纳管：**移除托管公钥、删除DNS记录（§6.4）、删除Console侧凭据记录。节点上的用户数据一概不动。

### 9.1 sudo处理

若填写的用户非root，`harden`需要sudo。方案：

- 探测`sudo -n true`是否免密
- 若需密码，用首登密码经stdin喂给`sudo -S`（同样不落库）
- 若该用户不在sudoers，流水线在`probe`步就失败并给出明确提示，不做任何猜测性修复

### 9.2 支持的发行版

**白名单：**Ubuntu 22.04 / 24.04、Debian 12。`probe`步检测到白名单外的发行版立即失败，提示不支持。

**理由：**Docker安装、防火墙工具、init系统在各发行版差异巨大，宣称「通用」而实际只在两三个发行版上测过，属于CLAUDE.md禁止的「未验证的功能不写成已完成」。

---

## 10. 数据模型（Console独立库）

**关于「迁移只有一份源」规则：**CLAUDE.md规定迁移只有`internal/store/migrations/`一份、禁止在仓库别处复制第二份。本设计新增`internal/consolestore/migrations/`——这**不是复制**，是另一个服务的另一个数据库的独立迁移集。两者schema无交集，不存在双写同一张表的风险。实施时须在CLAUDE.md把该规则改写为「每个库的迁移各有唯一一份源，禁止同一套迁移出现两份拷贝」。

```sql
-- 001_console_initial.sql

CREATE TABLE admin_users (
  id UUID PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,           -- Argon2id
  totp_secret_enc BYTEA NOT NULL,        -- AES-256-GCM
  totp_nonce BYTEA NOT NULL,
  disabled_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE admin_sessions (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES admin_users(id),
  token_hash TEXT NOT NULL UNIQUE,       -- 只存哈希，cookie里是明文
  client_ip TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ
);

-- DNS zone：支持多个域名（§6.2）
CREATE TABLE dns_zones (
  id UUID PRIMARY KEY,
  domain TEXT NOT NULL UNIQUE,           -- example.com
  provider TEXT NOT NULL,                -- manual|route53
  provider_ref TEXT NOT NULL DEFAULT '', -- route53的hosted zone id
  credential_enc BYTEA,                  -- AES-256-GCM；manual模式为NULL
  credential_nonce BYTEA,
  subdomain_prefix TEXT NOT NULL DEFAULT 'api',  -- api-01 里的 "api"
  next_seq INT NOT NULL DEFAULT 1,       -- 单调递增，永不回收（§6.2）
  caa_ok BOOLEAN,                        -- CAA预检结果（§6.5）
  caa_checked_at TIMESTAMPTZ,
  accepting_new BOOLEAN NOT NULL DEFAULT TRUE,  -- 证书预算超限时置false
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE nodes (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,                    -- 公网IP
  ssh_port INT NOT NULL DEFAULT 22,
  ssh_user TEXT NOT NULL,
  host_key_fp TEXT,                      -- TOFU固定
  status TEXT NOT NULL,                  -- new|provisioning|ready|degraded|unreachable|host_key_changed
  os_release TEXT, arch TEXT,
  stack_version TEXT,                    -- 已部署的产物版本
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE node_credentials (
  node_id UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  private_key_enc BYTEA NOT NULL,        -- AES-256-GCM；密码永不出现在本表
  nonce BYTEA NOT NULL,
  public_key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  rotated_at TIMESTAMPTZ
);

-- 子域名分配：seq永不回收，退役后记录保留并标记（§6.2、§6.4）
CREATE TABLE node_domains (
  id UUID PRIMARY KEY,
  zone_id UUID NOT NULL REFERENCES dns_zones(id),
  seq INT NOT NULL,
  fqdn TEXT NOT NULL UNIQUE,             -- api-03.example.com
  node_id UUID REFERENCES nodes(id) ON DELETE SET NULL,  -- 退役后置NULL但保留行
  target_ip TEXT NOT NULL,
  record_state TEXT NOT NULL,            -- pending|insync|removed|orphaned
  dns_verified_at TIMESTAMPTZ,
  cert_issuer TEXT,
  cert_expires_at TIMESTAMPTZ,
  released_at TIMESTAMPTZ,               -- 退役时间；seq仍不可复用
  UNIQUE (zone_id, seq)
);

-- 证书签发记账，用于预算水位（§6.5）
CREATE TABLE cert_issuances (
  id BIGSERIAL PRIMARY KEY,
  zone_id UUID NOT NULL REFERENCES dns_zones(id),
  fqdn TEXT NOT NULL,
  issuer TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),  -- 巡检发现证书序列号变化时记一笔
  serial TEXT NOT NULL
);
CREATE INDEX cert_issuances_window ON cert_issuances (zone_id, observed_at);

CREATE TABLE provision_runs (
  id UUID PRIMARY KEY,
  node_id UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,                    -- bootstrap|redeploy|domain|rotate-key|decommission|diagnose
  status TEXT NOT NULL,                  -- running|succeeded|failed|cancelled
  triggered_by UUID REFERENCES admin_users(id),
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);

CREATE TABLE provision_steps (
  run_id UUID NOT NULL REFERENCES provision_runs(id) ON DELETE CASCADE,
  seq INT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL,                  -- pending|running|succeeded|skipped|failed
  exit_code INT,
  log_path TEXT,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  PRIMARY KEY (run_id, seq)
);

-- 节点上项目的镜像副本，非权威（权威在节点自己的库）
CREATE TABLE node_projects (
  id UUID PRIMARY KEY,
  node_id UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  slug TEXT NOT NULL,
  remote_project_id TEXT NOT NULL,       -- 节点库里的UUID
  disk_limit_bytes BIGINT NOT NULL,
  five_hour_limit BIGINT NOT NULL,
  seven_day_limit BIGINT NOT NULL,
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (node_id, slug)
);

-- 只记签发事件，绝不存CDK明文、哈希或secret部分
CREATE TABLE cdk_issues (
  id UUID PRIMARY KEY,
  node_project_id UUID NOT NULL REFERENCES node_projects(id) ON DELETE CASCADE,
  public_id TEXT NOT NULL UNIQUE,        -- 可公开部分；供 /connect 查询与对账（§6.6）
  issued_by UUID REFERENCES admin_users(id),
  issued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);

CREATE TABLE releases (
  version TEXT PRIMARY KEY,
  notes TEXT NOT NULL DEFAULT '',
  published_at TIMESTAMPTZ,              -- NULL=已构建未发布，下载页不展示
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE release_artifacts (
  version TEXT NOT NULL REFERENCES releases(version) ON DELETE CASCADE,
  os TEXT NOT NULL, arch TEXT NOT NULL,
  filename TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  sha256 TEXT NOT NULL,
  PRIMARY KEY (version, os, arch)
);

CREATE TABLE audit_log (
  id BIGSERIAL PRIMARY KEY,
  actor UUID REFERENCES admin_users(id),
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  result TEXT NOT NULL,                  -- ok|denied|error
  detail JSONB NOT NULL DEFAULT '{}',    -- 写入前必经redact
  client_ip TEXT NOT NULL,
  at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**`/connect`查询的实现：**`cdk_issues.public_id` → `node_projects` → `nodes` → `node_domains.fqdn`。全在Console库内，不需要访问节点。

---

## 11. 对现有代码的改动

### 11.1 `cmd/ccwadmin`扩展（节点侧，前置依赖）

Console通过SSH调用它，因此必须机器可读。当前只有`init-project`且输出是人类可读文本（`cmd/ccwadmin/main.go:64`）。

新增子命令，全部支持`--json`：

| 命令 | 用途 |
|---|---|
| `init-project --slug --disk-gib --five-hour --seven-day --json` | 建项目（幂等：已存在则返回现有信息而非报错）；**强制§7.6的3容器与15 GiB上限** |
| `list-projects --json` | 列项目及配额 |
| `issue-cdk --slug --json` | 为已有项目签发新CDK；输出含`public_id`供Console入库 |
| `rotate-cdk --slug [--grace 24h\|--revoke-now] --json` | **轮换CDK**：签发新的并给旧的设定失效时间，见§11.1.1 |
| `disable-cdk --public-id` | 立即禁用指定CDK |
| `list-cdks --slug --json` | 列该项目全部CDK的`public_id`/签发时间/失效状态（**不含明文，明文不可再取**） |
| `status --json` | 栈健康、磁盘、容器状态、证书到期时间 |

**幂等性是硬要求：**`init-project`当前遇到重复slug会因UNIQUE约束失败。流水线重跑必须能安全再执行，所以要改成upsert语义。

**`issue-cdk`必须回传`public_id`**：Console靠它建立`cdk_issues`记录，而`/connect`查询依赖这张表。CDK明文只在SSH响应里出现一次，转发到浏览器后即丢弃，绝不入Console库。

**上限强制点：**`init-project`必须在建项目前查询该节点现有项目数，达到3即拒绝并返回明确错误；`--disk-gib`超过15即拒绝。**校验放在`ccwadmin`而非只放在Console**——Console是调用方，把约束只放在调用方等于没有约束（SSH直连节点仍可绕过）。

#### 11.1.1 CDK轮换

**现状：读路径已完备，只缺写入端。**核实于2026-07-26：

| 能力 | 状态 |
|---|---|
| `cdks`表有`disabled_at`与`expires_at`列 | ✅ 已有（`001_initial.sql`） |
| `ResolveCDK`校验二者且用数据库`now()` | ✅ 已有（`internal/store/postgres.go:92`） |
| 一个项目可挂多张CDK | ✅ schema无唯一约束限制 |
| 客户端认证失败时清本地缓存 | ✅ 已有（`cmd/cclaude/main.go:44`的`clearCDK()`） |
| **写入`disabled_at`的代码** | ❌ **全仓无任何引用**——这是唯一缺口 |

因此轮换的实现量很小：`store`加一个方法 + CLI子命令。

**轮换语义（两种，默认宽限）：**

| 模式 | 行为 | 适用 |
|---|---|---|
| `--grace 24h`（默认） | 签发新CDK；旧CDK设`expires_at = now() + 24h` | 例行轮换。客户有24小时切换，期间新旧都能用 |
| `--revoke-now` | 签发新CDK；旧CDK立即`disabled_at = now()` | **凭据泄露应急**。旧CDK当场失效 |

宽限期利用现有的`expires_at`语义**自动生效**，不需要定时任务清理——`ResolveCDK`每次查询都会比对，过期即不返回。

**轮换后的客户体验：**旧CDK失效后，`cclaude`的exchange返回`invalid_cdk`，客户端清除本地缓存并提示重新输入。**已连接的tmux现场不受影响**（会话在项目容器里），客户输入新CDK重连即恢复。

**必须遵守的既有规则：**

- 新CDK明文**只显示一次**，与`init-project`一致；Console转发到浏览器后即丢弃，不入Console库
- 轮换失败一律返回统一错误，不泄露「项目不存在/CDK不存在/已禁用」的区别
- 轮换是特权操作，Console侧必须写`audit_log`；节点侧`ccwadmin`的输出与日志**不得**出现任何CDK明文

**Console侧配套：**`cdk_issues`表新增一行记录新`public_id`，旧行写`revoked_at`。`/connect`查询页在宽限期内对旧`public_id`**仍应返回域名**（客户可能正在迁移中），撤销后返回「未找到」。

### 11.2 `cmd/cclaude`寻址改造

见§6.7。改动集中在：

- `cmd/cclaude/main.go:31`：删除写死默认值`https://ccw.example.com`
- `cmd/cclaude/main.go:201-240`：`loadOrPromptCDK`扩展为`loadOrPromptConfig`，读写`~/.ccw/config.json`，含旧`~/.ccw/cdk`的自动迁移
- 新增`--api`参数与`logout`子命令
- CDK仍只走交互输入或`CCW_CDK`环境变量，**不接受命令行参数**

### 11.3 `deploy/compose.yaml`模板化

现在硬编码`project-a`/`project-b`。Console要支持任意slug与数量，改为Go模板渲染后上传。

**P0-2部分已于`4093e3d`落地**（模板化本身仍未做），按§7.3的方案——**共享HOME卷保持不变**，只额外把`.claude/projects`挂成每客户独立卷：

```yaml
volumes:
  - {{.Slug}}-workspace:/workspace
  - claude-shared:/home/claude                                  # 不变：凭据继续共享
  - {{.Slug}}-claude-projects:/home/claude/.claude/projects     # 新增：JSONL按客户隔离
  # 建议同时隔离（§7.5）：
  # - {{.Slug}}-claude-history:/home/claude/.claude/shell-snapshots
```

**与本文档早先版本的差异：**早先提的是「每项目独立HOME卷」，代价是每个客户各自登录一次Claude。**该方案已废弃**——它要求凭据副本在各容器独立刷新，撞上从未验证过的spec风险R2。现方案凭据行为与今天完全一致，既解除P0-2，又不需要先做24小时双登录验证。理由详见§7.3。

**配套改动：**`Dockerfile.claude`须预建`/home/claude/.claude/projects`并`chown claude:claude`，否则新命名卷会以root初始化导致容器内写不进去。`docs/design-deviations.md`的D1须据此更新（从「待决策二选一」改为「已定方案」）。

**Caddyfile模板同步：**节点Caddyfile需要接受可选的别名域名（§6.2的CNAME场景），且必须加注释锁死「不启用短期证书」的决定（§6.5）。

### 11.4 `internal/config`新增变量

`CCW_SECRET_KEY`、`CCW_SITE_DOMAIN`、`CCW_ADMIN_DOMAIN`、`CCW_ADMIN_ALLOWLIST`、`CCW_DIST_DIR`、`CCW_CONSOLE_DATABASE_URL`、`CCW_CONSOLE_LISTEN_ADDR`、`CCW_ACME_CA`（节点侧，e2e用staging）。全部沿用「缺失即硬失败、无默认值」，`CCW_ACME_CA`例外（留空＝生产CA）。

### 11.5 节点巡检

Console后台goroutine，每5分钟对每个`ready`节点执行`ccwadmin status --json`，更新`nodes.last_seen_at`与`stack_version`，失败三次标记`unreachable`。

巡检同时做四件事：

- 记录证书序列号，变化即写`cert_issuances`（§6.5预算水位的数据来源）
- 校验`node_domains`每条记录仍指向已知节点IP，不一致标记`orphaned`并告警（§6.4）
- 证书30天内到期且未续期时告警
- **CDK对账**：跑`ccwadmin list-cdks --json`，与Console的`cdk_issues`比对

**为什么需要CDK对账：**Console库里的`cdk_issues`是**镜像不是权威**——权威是节点自己的`cdks`表。若有人SSH直连节点执行`ccwadmin issue-cdk`（绕过Console），Console不会知道这张CDK存在，后果是`/connect`查不到、后台显示的有效CDK数量偏少、撤销时会漏掉。对账负责发现这类漂移并告警。

对账**只比对`public_id`集合与失效状态**，不传输任何明文或哈希——这与§8.4的凭据存储边界一致。

**并发上限与超时是必需的**：机队规模上去后，无限并发SSH会打满Console的fd与目标机的`MaxStartups`。默认并发5、单次超时30秒。

---

## 12. 实施计划

### 12.1 任务分解

按TDD推进（CLAUDE.md：先写失败测试再实现）。SSH、Docker、DNS用假实现单测，真实纳管放`tests/e2e`。

| # | 任务 | 依赖 | 说明 |
|---|---|---|---|
| C1 | console骨架：配置、独立库迁移、`/healthz` | — | |
| C2 | 信封加密工具（AES-256-GCM + `CCW_SECRET_KEY`） | C1 | 单测覆盖密文不可预测、篡改检出 |
| C3 | 管理员认证：Argon2id + TOTP + 会话表 + 限速 + IP白名单 | C1,C2 | 统一错误、CSRF、可撤销会话 |
| C4 | SSH执行层：拨号、host key TOFU、流式命令、超时 | C1 | 用内存SSH server做单测 |
| C5 | 日志脱敏`redact()` | — | **独立单测，覆盖密码/私钥/CDK/token/AccessKey五类** |
| C6 | 凭据生命周期：密钥生成、注入、验证、密码销毁、轮换 | C2,C4 | |
| C7 | 流水线引擎：步骤定义、precheck跳过、断点续跑、DB记账 | C4,C5 | 假步骤单测幂等与恢复 |
| C8 | **DNS Provider接口 + `manual`实现 + 子域名分配器** | C1 | 分配器单测：序号单调、永不回收、保留名单 |
| C9 | **Route 53实现**（可选，可推迟） | C2,C8 | 假HTTP层单测UPSERT幂等与INSYNC轮询 |
| C10 | **证书预算记账与阻断逻辑** | C8 | 单测7天滚动窗口与阈值行为 |
| C11 | bootstrap流水线12步实现 | C6,C7,C8 | |
| C12 | `ccwadmin`扩展 + 幂等化（节点侧） | — | 可与C1–C11并行 |
| C13 | compose渲染（`ccwadmin render-compose`） | C12 | **详见`plans/2026-07-26-compose-render-plan.md`**；`projects/`独立卷部分已于07-26落地 |
| ~~**N1**~~ | ~~**用量采集接线**~~ | — | **2026-07-26已实施**，未真机验证。见`plans/2026-07-26-usage-wiring-plan.md` |
| ~~**N2**~~ | ~~**`modeFor`查额度**~~ | — | **2026-07-26已实施**（fail closed：查询失败也降级），未真机验证 |
| **N3** | **account模型改造**：一节点一account，客户为其下project；`ccwadmin`不再硬编码`default-pool` | C12 | §7.4 |
| **N4** | **磁盘失控防线**：Docker`data-root`指向独立分区 + Postgres数据移出data-root + 磁盘水位告警 | C11 | **取代原「硬配额接入流程」**，见下方说明 |
| C14 | **`cclaude`寻址改造 + 配置迁移** | — | 独立性强，可早做 |
| C15 | 管理后台UI：总览、向导、详情、SSE日志 | C3,C7 | |
| C16 | 域名管理UI：zone列表、预算水位、记录状态 | C8,C10,C15 | |
| C17 | 项目与CDK管理UI（明文一次性展示）+ **超卖水位展示** | C12,C15,N3 | §7.4、§7.6 |
| C18 | **`/connect`查询页**（前端切分、只收public-id） | C1,C12 | 单测：含`.`的输入必须被拒且不记录 |
| C19 | 审计日志 + 节点巡检（含证书与DNS漂移检查） | C3,C4,C10 | |
| C20 | 用户端站点 + 发布流水线 + 下载页 | C1 | 可早做，独立性强 |
| C21 | Console自身部署：compose、Caddyfile、备份恢复runbook | C1–C20 | |
| C22 | e2e：对一台真实空VPS走完整bootstrap（**staging CA**） | C21 | **不允许用`t.Skip`充数** |

#### N4改写说明（2026-07-26）

原N4是「把`deploy/quota-setup.sh`纳入bootstrap」。**该任务在当前卷布局下达不成它自己的目的，已废弃。**

原因：`quota-setup.sh`创建的是bind到loop挂载点的卷`<slug>-workspace`，而compose实际使用的卷带项目前缀（`deploy_<slug>-workspace`）——**两者不是同一个卷**。脚本会正常退出并打印成功，容器却仍挂着不受约束的普通命名卷，且没有任何报错。要让它生效必须改卷布局（渲染计划§4.2方案B或§4.3方案C），而用户2026-07-26已定**沿用现状的普通命名卷**（渲染计划§4.4）。

因此N4改为在**不动卷布局**的前提下收敛爆炸半径，三项内容：

1. `install-docker`步骤把Docker`data-root`指向独立分区/盘——客户写满的是该盘，宿主机根分区与SSH救援能力不受影响
2. **Postgres数据移出data-root**：`deploy/compose.yaml`的`ccw-pg`现在是普通命名卷，落在`/var/lib/docker/volumes`即data-root上，客户写满盘会连数据库一起拖死。须改为bind到data-root之外的宿主机目录（如`/var/lib/ccw/pgdata`）。**已有部署改此项需要迁移数据**（`pg_dump`后恢复，或停栈后拷贝卷内容），不可直接改配置重启
3. 磁盘水位告警并入§11.5节点巡检——按§7.6，这在方案A下是必要功能而非可选项

**未被N4解决、且不打算解决的：**同一节点上客户之间的磁盘互相影响。逻辑配额只统计走同步接口的文件，客户在终端里`npm install`、堆构建缓存或直接`dd`可以突破15 GiB上限，撑爆后同机全部客户一并受影响。这是方案A的**已接受取舍**，不是待修缺陷——不得在STATUS.md或本文档中记为待办。若商业上不能接受，须回到渲染计划§4.2重新选型，代价是渲染器加`--workspace-mode`、bootstrap多一条硬顺序约束、已有部署迁卷。

### 12.2 建议顺序

1. **C20 + C14**（用户端站点 + 客户端寻址改造）：独立性最强、风险最低、马上有可见产出，且C14是多节点的前提
2. **N1 + N2**（采集接线、`modeFor`查额度）：**共享节点的上线闸门，见§12.3**。N1原标为依赖C13，该依赖已随`4093e3d`（JSONL按项目分卷）解除，**现在即可开工**，不必等模板化
3. **C12 + C13 + N3**（`ccwadmin`扩展、模板化、account模型）：所有后台功能的前置
4. **C1–C7**（Console核心：认证、SSH、流水线）
5. **C8 + C10 + C11 + N4**（DNS分配、证书预算、bootstrap全流程含磁盘失控防线）——先用`manual`模式跑通
6. **C15–C19**（UI、查询页、审计巡检、超卖水位）
7. **C9**（Route 53自动化）——此时`manual`已能用，接不接是纯效率问题
8. **C21–C22**（部署与真实验收）

### 12.3 与STATUS.md现有缺口的关系：账号模型改变了优先级

Console会把节点从1台变成N台，且每台服务多个客户。STATUS里的P0-1、P1-1、P1-2原本是「一台机器上的洞」，现在变成「N台机器上的洞」，且每台都通过后台一键复制出厂。

**「你提供账号、客户共用池」这个选择，把P0-1从技术债变成了上线阻断项。**理由见§7.4：共享上游额度时，内部闸门是**唯一**能阻止某个客户吃光全机额度的机制；闸门空转意味着同机客户会同时断线、同时投诉。

因此定为硬闸门：

| 闸门 | 内容 | 阻断什么 |
|---|---|---|
| ~~**N1**~~ | ~~采集接线~~ | **代码已完成（2026-07-26），但闸门未经真机验证、也未校准**——在真机验证前，这道闸门不能算数，一台机器仍只应服务一个客户 |
| **N2** | `modeFor`查额度 | 未完成前，超额客户仍能上传，配额形同虚设 |
| **N4** | 磁盘失控防线（data-root独立分区 + pg数据外移 + 水位告警） | 未完成前，单客户写满盘会连宿主机根分区与数据库一起拖死，**无法SSH救援**；完成后爆炸半径收敛为「同机客户一并降级，但机器可救、数据不丢」 |
| P1-2 | `openat2`路径解析 | 不阻断共享上线（单管理员场景风险有限），但仍是spec的「必须」欠账 |

**N4的闸门含义与其他三条不同，不要误读：**N1/N2/N3完成后对应的洞就补上了；N4完成后**同机客户之间的磁盘互相影响依然存在**（原因见§12.1的N4改写说明）。N4只把后果从「整台机器死亡且救不回来」降到「客户一起降级、管理员能登机处理」。把它当成「做完就隔离好了」会导致对外承诺越界——按§7.6，15 GiB对外只能称「配额」，不得表述为「保证不超过」。

**单客户独占的节点不受这些闸门约束**，可以先上线——这给了一条「先服务少量客户、边跑边补闸门」的渐进路径。

---

## 13. 验收标准

| # | 标准 |
|---|---|
| A1 | 公开站点可访问，落地页无外部CDN依赖 |
| A2 | 下载页六平台产物齐全，SHA256与实际文件一致，`shasum -c SHA256SUMS`通过 |
| A3 | `cclaude --version`输出与下载的版本号一致 |
| A4 | 管理后台无TOTP无法登录；密码错、TOTP错、用户不存在三种情况返回**完全相同**的错误 |
| A5 | 白名单外IP访问管理域名得404，且应用层日志确认请求未进入业务处理 |
| A6 | 管理员会话可从后台立即撤销，撤销后原cookie立刻失效 |
| A7 | 对一台全新Ubuntu 24.04 VPS，只填IP/用户名/密码并选zone，走完bootstrap得到可用节点 |
| A8 | bootstrap全过程日志在浏览器实时可见 |
| A9 | 中断bootstrap后从失败步重跑成功，已完成步骤标记`skipped` |
| A10 | bootstrap完成后，目标服务器密码在Console库与全部日志文件中**零出现**（自动化grep验证） |
| A11 | 日志与审计里搜不到私钥、CDK明文、`CCW_TOKEN_KEY`、`POSTGRES_PASSWORD`、AWS AccessKey |
| A12 | 子域名由Console自动分配，序号单调递增；退役节点的序号不再分配给新节点 |
| A13 | `manual`模式下DNS未生效时`dns-allocate`阻断流水线，不触发任何Let's Encrypt请求 |
| A14 | Route 53模式下A记录自动创建，`GetChange`到`INSYNC`后流水线才继续；重跑该步不报错（UPSERT幂等） |
| A15 | 域名配置完成后443证书就绪，后台显示签发者与有效期 |
| A16 | zone本周签发数超阈值时，新节点无法分配到该zone，且后台明确提示原因 |
| A17 | 解除纳管后：托管公钥从节点`authorized_keys`移除，**DNS记录已删除**，节点用户数据完好 |
| A18 | 后台签发CDK后，用该CDK在本地`cclaude`登录成功并进入终端 |
| A19 | **Console停机期间**，已配置好的`cclaude`终端与同步完全不受影响 |
| A20 | `/connect`页面：抓包确认只有public-id离开浏览器；直接POST完整CDK被服务端拒绝且请求体不入日志 |
| A21 | CDK明文刷新页面后不可再见，且Console库中查无此明文 |
| A22 | 全新`cclaude`首次运行无任何写死域名；`--api`指定后写入`~/.ccw/config.json`（0600） |
| A23 | 旧版`~/.ccw/cdk`单行文件能自动迁移为`config.json`且不丢CDK |
| A24 | `ps aux`与shell history中查不到CDK（验证CDK不做命令行参数） |
| A25 | host key变更时连接中止，节点标记`host_key_changed` |
| A26 | **`projects/`独立卷后，两个客户的会话JSONL分别落在各自卷中**（P0-2解除的直接证据，§7.3） |
| A27 | **同一节点上两个客户各自登录一次即可用**，且切换客户不触发重新登录（验证凭据共享行为未被改变） |
| A28 | **采集器把两个客户的用量分别记入各自`project_id`**，`usage_events`无交叉归属（N1完成的直接证据） |
| A29 | **客户A跑满自己的5h配额后被闸门拦截，客户B在同一节点上不受影响**（§7.4公平性的核心证据） |
| A30 | 账号级池用尽时，同节点全部客户一并降级为cleanup模式，且`/usage`门户如实显示原因 |
| A31 | 后台显示单节点超卖水位＝`Σ客户5h配额 / 账号池5h容量`，与库中数据一致 |
| A34 | **建第4个项目被拒绝**，且`ccwadmin`与Console两处都拒（绕过Console直连节点同样拒绝，§7.6） |
| A35 | **`--disk-gib 16`被拒绝**；不传该参数时默认值为15 |
| A36 | **CDK轮换（默认宽限）**：新CDK立即可用，旧CDK在宽限期内仍可用，宽限期过后自动失效且无需任何定时任务 |
| A37 | **CDK轮换（`--revoke-now`）**：旧CDK当场失效；持旧CDK的`cclaude`收到`invalid_cdk`并清除本地缓存，输入新CDK后**原tmux现场完好** |
| A38 | 轮换后`list-cdks`能看到新旧两张的状态，且**任何输出与日志中都不含CDK明文** |
| A32a | 客户在容器内`dd`撑爆workspace所在盘后：宿主机SSH可登录、Postgres仍可查询、Console巡检仍能读到该节点状态（N4的data-root隔离与pg数据外移生效的证据） |
| A32b | 可用空间跌破阈值时后台告警，且告警**先于**撑爆发生（水位告警生效的证据） |
| A33 | 备份恢复演练：Console库恢复后，机队列表、凭据、域名分配可用，能继续操作节点 |

**诚实表述要求（CLAUDE.md）：**上述任何一条未真实跑过，就不得在STATUS.md标为完成。e2e里未实现的断言用`t.Skip`，不用空断言。

---

## 14. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| **Console被攻破等于全机队沦陷** | 最高 | 独立域名 + IP白名单 + TOTP + 可撤销会话 + 全量审计 + 节点`CCW_TOKEN_KEY`不经Console |
| **AWS凭据权限过宽** | 最高 | IAM策略收敛到单个hosted zone的三个动作；绝不用管理员AccessKey（§6.3） |
| **退役后DNS记录悬挂→子域名被接管** | 高 | `dns-teardown`步骤强制；删记录先于释放主机；巡检检测漂移（§6.4） |
| 首登密码阶段被MITM | 高 | host key TOFU展示指纹供带外核对；建议管理员在可信网络下首次纳管 |
| `CCW_SECRET_KEY`丢失 | 高 | 全部托管私钥与AWS凭据不可解，需对每台机器重新纳管。部署runbook必须写明该密钥的备份要求 |
| **重复部署撞「重复证书5张/周」** | 中 | 重部署禁用`down -v`、`caddy-data`卷持久；e2e走staging CA（§6.5） |
| **误启用LE短期证书导致预算枯竭** | 中 | Caddyfile模板注释锁死；巡检发现证书有效期<30天即告警（§6.5） |
| 单周开量超50台 | 中 | 多zone分流；Console在48张时阻断分配并提示换zone |
| zone设了CAA挡住Let's Encrypt | 中 | zone接入时一次性CAA预检（§6.5） |
| **闸门空转下开放共享节点→单客户吃光全机额度** | **最高** | §12.3的硬闸门：N1未完成前一台机器只服务一个客户 |
| **超卖过度→高峰期集体降级** | 高 | 后台超卖水位可见（A31）；账号级池闸门兜底；初期超卖倍率宜保守 |
| **容器逃逸→同机全部客户数据泄露** | 高 | 用户已确认按「客户可信」接受，**不做技术加固**；必须写入服务条款并在风险清单长期保留（§7.5） |
| 客户间可见命令历史与shell快照 | 中 | 建议额外隔离`history.jsonl`与`shell-snapshots/`（§7.5），成本两行compose |
| 同账号多容器并发写凭据的长期稳定性未验证 | 中 | 现有部署已在此形态运行；客户数上去后补做24小时以上观察（§7.3待验证第3点） |
| 发行版差异导致流水线半途失败 | 中 | 白名单只收Ubuntu 22.04/24.04与Debian 12，白名单外`probe`即失败 |
| 缺口随机队复制放大 | 中 | §12.3：C13前先补N2（`modeFor`） |
| 用户弄丢自己的API域名 | 低 | `/connect`查询页兜底（§6.6） |
| Console单点无HA | 低 | Console不在数据面上，宕机不影响已部署节点（A19验证） |

---

## 15. 开放问题

**已关闭：**Console主机独立一台（07-26确认）；一节点多客户共用（07-26确认，子域名因此是**节点维度**，同域名下靠CDK区分客户）。

1. 首个zone是否需要`api.example.com`这个无序号的CNAME别名？若要，节点Caddyfile需同时声明两个名字（§6.2）。
2. **单节点客户数上限定多少？**受§7.6三个上限约束，但商业上还要定超卖倍率。建议初期保守（≤1.5倍）并观察实际峰值。
3. **客户配额售卖档位如何定义？**`five_hour_limit`/`seven_day_limit`/`disk_limit_bytes`三个数值需要产品化为档位，Console建项目时选档而非填数字。
4. 客户额度用尽时的产品行为：仅降级为cleanup（现设计），还是提供加购？后者需要计费，会推向多租户。
5. 用户端站点是否需要多语言（中/英）？当前设计只做中文。
6. 是否需要「用户自助注册领取CDK」？当前设计是**纯管理员签发**——自助注册会把系统推向多租户，与非目标冲突。
7. 节点软件升级策略：现在是「重新部署」全量重来。是否需要滚动升级与回滚？
8. `releases`是否需要支持渠道（stable/beta）？

---

**Sources（证书限额核实）：** [Let's Encrypt Rate Limits](https://letsencrypt.org/docs/rate-limits/)
