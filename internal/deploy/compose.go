package deploy

import (
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// ComposeInput是渲染的全部输入。渲染是纯函数：不读数据库、不读环境、
// 不含时间戳与随机数——同一输入必须产出字节级相同的输出（I7，渲染计划§3.4）。
type ComposeInput struct {
	Projects    []string // 项目slug列表；渲染前按字典序归一，书写顺序不影响输出
	ClaudeImage string   // 项目容器镜像；空值取ccw-claude:latest（与手写文件一致）
}

const defaultClaudeImage = "ccw-claude:latest"

// RenderCompose渲染完整的compose.yaml（渲染计划§5的模板契约I1–I7 + 用量接线计划§9的I8）。
// 校验失败即拒绝整次渲染，不做「跳过非法项继续」。
func RenderCompose(in ComposeInput) (string, error) {
	if err := ValidateSlugs(in.Projects); err != nil {
		return "", err
	}
	image := in.ClaudeImage
	if image == "" {
		image = defaultClaudeImage
	}
	if err := validateImageRef(image); err != nil {
		return "", err
	}

	slugs := append([]string(nil), in.Projects...)
	sort.Strings(slugs) // 字典序归一（B4）：输出不依赖--projects的书写顺序

	data := composeData{ClaudeImage: image, SlugsCSV: strings.Join(slugs, ",")}
	for i, s := range slugs {
		data.Projects = append(data.Projects, composeProject{
			Slug: s, First: i == 0, FirstSlug: slugs[0],
		})
	}
	var b strings.Builder
	if err := composeTmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("deploy: render compose: %w", err)
	}
	return b.String(), nil
}

type composeData struct {
	ClaudeImage string
	SlugsCSV    string
	Projects    []composeProject
}

type composeProject struct {
	Slug      string
	First     bool   // 字典序第一个项目负责build镜像，其余复用
	FirstSlug string // 非first项目depends_on它，确保镜像先构建
}

// validateImageRef对镜像引用做轻量校验：它会被原样写进YAML，
// 拒绝空白与引号等能破坏YAML结构的字符。
func validateImageRef(image string) error {
	for _, r := range image {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == ':' || r == '/' || r == '@':
		default:
			return fmt.Errorf("deploy: 镜像引用%q含非法字符%q", image, r)
		}
	}
	if image == "" {
		return fmt.Errorf("deploy: 镜像引用为空")
	}
	return nil
}

// 模板与deploy/compose.yaml的手写版本语义一致（验收B1/B10）；
// 手写文件自身即由本模板渲染产生（渲染计划R6）。
// 缩进固定两空格、输出以恰好一个\n结尾、无TAB（I7）。
var composeTmpl = template.Must(template.New("compose").Parse(`# 远程Claude工作空间——部署编排。
#
# 本文件由 ccwadmin render-compose 生成，手工编辑会在下次渲染时被覆盖。
# 重新生成：ccwadmin render-compose --projects {{.SlugsCSV}} --out compose.yaml
#
# 对外只暴露Caddy的443/80；control-api与worker-agent不映射宿主机端口，
# 仅在内部ccw网络内被Caddy访问（等效于"只监听内网"）。
# 项目容器以sleep infinity运行，等待管理员登录与worker-agent附着。
# 文件系统硬配额已决定不做（quota-setup.sh在本布局下不生效，勿执行），
# 部署前的自查步骤与已知取舍见 DEPLOY.md 第11节。

services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: ccw
      POSTGRES_PASSWORD: "${POSTGRES_PASSWORD:?set in .env}"
      POSTGRES_DB: ccw
    volumes:
      - ccw-pg:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ccw"]
      interval: 5s
      timeout: 3s
      retries: 10
    networks: [ccw]
    restart: unless-stopped

  control-api:
    build:
      context: ..
      dockerfile: deploy/Dockerfile.control-api
    environment:
      CCW_DATABASE_URL: postgres://ccw:${POSTGRES_PASSWORD}@postgres:5432/ccw
      CCW_TOKEN_KEY: "${CCW_TOKEN_KEY:?generate with openssl rand -hex 32}"
      CCW_WORKSPACE_ROOT: /srv/ccw
      CCW_LISTEN_ADDR: 0.0.0.0:8080
      CCW_AGENT_WS_BASE: wss://${CCW_DOMAIN}/ws
    depends_on:
      postgres:
        condition: service_healthy
    networks: [ccw]
    restart: unless-stopped

  worker-agent:
    build:
      context: ..
      dockerfile: deploy/Dockerfile.worker-agent
    environment:
      CCW_DATABASE_URL: postgres://ccw:${POSTGRES_PASSWORD}@postgres:5432/ccw
      CCW_TOKEN_KEY: ${CCW_TOKEN_KEY}
      CCW_WORKSPACE_ROOT: /srv/ccw
      CCW_AGENT_LISTEN_ADDR: 0.0.0.0:8081
      # 用量采集：JSONL按项目挂在这个根下（<usage_root>/<slug>），只读。
      CCW_USAGE_ROOT: /srv/ccw/usage
      # 四种token折算为"内部额度单位"的权重：input,output,cache_read,cache_write。
      # 当前取值是估算的起点，不是官方口径；跑够真实数据后校准（见用量接线计划§3.1）。
      CCW_USAGE_WEIGHTS: ${CCW_USAGE_WEIGHTS:-1,5,1,1}
    volumes:
      # 挂docker.sock以exec项目容器；本服务因此等同宿主机高权限，不对公网暴露。
      - /var/run/docker.sock:/var/run/docker.sock
{{- range .Projects}}
      - {{.Slug}}-workspace:/srv/ccw/{{.Slug}}
{{- end}}
      # 会话JSONL（用量采集的数据源）：只读挂进来，采集器只读文件、游标写PG。
      # 缺这些行的后果是采集器安静地扫一个空目录——不报错、usage_events永远为空，
      # 极难排查（模板契约I8，见用量接线计划§2.2与§9）。
{{- range .Projects}}
      - {{.Slug}}-claude-projects:/srv/ccw/usage/{{.Slug}}:ro
{{- end}}
    depends_on:
      postgres:
        condition: service_healthy
    networks: [ccw]
    restart: unless-stopped

  caddy:
    image: caddy:2.8
    ports:
      - "80:80"
      - "443:443"
    environment:
      CCW_DOMAIN: ${CCW_DOMAIN}
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config
    depends_on: [control-api, worker-agent]
    networks: [ccw]
    restart: unless-stopped
{{range .Projects}}
  {{.Slug}}:
{{- if .First}}
    build:
      context: ..
      dockerfile: deploy/Dockerfile.claude
      args:
        CLAUDE_CODE_VERSION: ${CLAUDE_CODE_VERSION:-}
    image: {{$.ClaudeImage}}
{{- else}}
    image: {{$.ClaudeImage}}
    depends_on: [{{.FirstSlug}}]{{/* 镜像由字典序第一个项目构建，其余复用 */}}
{{- end}}
    container_name: ccw-{{.Slug}}
    volumes:
      - {{.Slug}}-workspace:/workspace
{{- if .First}}
      # 共享授权：全部项目共用同一个 /home/claude 卷，整机登录一次即全部可用（I3）。
      # 挂整个 home（而非仅 .claude）以同时持久化 .claude.json 与 .claude/.credentials.json。
      - claude-shared:/home/claude
      # 会话 JSONL 按项目隔离：嵌套挂载只遮蔽 .claude/projects 一个子目录，
      # 凭据文件 .claude/.credentials.json 是它的兄弟节点，仍在共享卷里——
      # 因此账号全机仍只授权一次，而用量可以按项目归属（设计§7.3，I4）。
{{- else}}
      - claude-shared:/home/claude          # 同一个共享卷：与全部项目共用登录凭据（I3）
{{- end}}
      - {{.Slug}}-claude-projects:/home/claude/.claude/projects
      - {{.Slug}}-sync:/var/lib/cclaude-sync
    environment:
      # Fullscreen 渲染的两个开关（官方文档 code.claude.com/docs/en/fullscreen）。
      #
      # ALT_SCREEN_FULL_REPAINT 是**官方针对本问题的修复**：fullscreen 只发生变化的
      # 单元格，而 Windows Terminal 这类 ConPTY 宿主会错误合并这些定位写入，
      # 把上一帧的片段留在屏幕上——2026-07-31 真机上滚动后左侧一列残留旧字，
      # 就是文档里「Stale or misplaced text」那一条。置 1 后每帧重画所有单元格。
      #
      # NO_FLICKER 让 fullscreen 明确开启，不依赖 ~/.claude/settings.json——
      # 那份文件在 claude-shared 卷里，**一次重置就没了**，而这里跟着部署走。
      # 用户仍可用 /tui default 切回经典渲染（官方说 /tui 会清掉这个变量）。
      #
      # 代价：每帧全量重绘的字节数高于增量，而我们这条链路是远程 WebSocket。
      # 先要正确，如果实测觉得卡再权衡。
      CLAUDE_CODE_NO_FLICKER: "1"
      CLAUDE_CODE_ALT_SCREEN_FULL_REPAINT: "1"
    networks: [ccw]
    restart: unless-stopped
{{end}}
volumes:
  ccw-pg: {}
  caddy-data: {}
  caddy-config: {}
  claude-shared: {}          # 全部项目共享的 Claude HOME（授权一次、共用凭据）
{{- range .Projects}}
  {{.Slug}}-workspace: {}
  {{.Slug}}-claude-projects: {}   # 项目{{.Slug}}的会话 JSONL（用量归属的数据源）
  {{.Slug}}-sync: {}
{{- end}}

networks:
  ccw: {}
`))
