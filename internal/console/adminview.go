package console

import (
	"time"

	"ccw/internal/consolestore"
	"ccw/internal/provision"
)

// 管理后台的视图模型：把存储层的原始状态翻成页面直接能渲染的形状。
//
// 状态在这套系统里是主要信息（节点、运行、每个步骤各有一套状态机），
// 所以「状态 → 中文文案 + 语义色」的映射集中在这里一处，
// 避免同一个 degraded 在三个页面上显示成三种说法。

// adminBase是所有管理页共用的外壳数据（侧边栏要用）。
type adminBase struct {
	Nav       string // 当前高亮的导航项
	Username  string
	Initial   string
	CSRF      string
	NodeCount int
}

func (s *Server) baseFor(nav string, sess consolestore.AdminSession, csrf string, nodeCount int) adminBase {
	initial := "?"
	if r := []rune(sess.Username); len(r) > 0 {
		initial = string(r[0])
	}
	return adminBase{
		Nav: nav, Username: sess.Username, Initial: initial,
		CSRF: csrf, NodeCount: nodeCount,
	}
}

// nodeTone把节点状态翻成中文与语义色。
// 语义色只有五种，与徽标的形状一一对应（形状+颜色双编码，不只靠颜色）。
func nodeTone(status string) (text, tone string) {
	switch status {
	case "new":
		return "待部署", "idle"
	case "provisioning":
		return "部署中", "run"
	case "ready":
		return "就绪", "ok"
	case "degraded":
		return "异常", "bad"
	case "unreachable":
		return "失联", "warn"
	case "host_key_changed":
		return "host key 已变更", "bad"
	}
	return status, "idle"
}

func runTone(status string) (text, tone string) {
	switch status {
	case "running":
		return "进行中", "run"
	case "succeeded":
		return "成功", "ok"
	case "failed":
		return "失败", "bad"
	case "cancelled":
		return "已取消", "idle"
	}
	return status, "idle"
}

func stepTone(status string) (text, tone, class string) {
	switch status {
	case "succeeded":
		return "完成", "ok", "done"
	case "skipped":
		return "跳过", "ok", "skipped"
	case "running":
		return "进行中", "run", "running"
	case "failed":
		return "失败", "bad", "failed"
	}
	return "未开始", "idle", "pending"
}

// runView是运行在列表里的紧凑呈现：一条步骤轨 + 当前步骤 + 进度。
//
// 为什么用步骤轨而不是百分比或计数：bootstrap 的 12 步是**真实的顺序**，
// 而运维时真正要回答的问题是「跑到哪一步、卡在哪一步」——
// 一条分段的轨道一眼就能答，一个百分比答不了。
type runView struct {
	ID, NodeID, NodeName string
	Kind                 string
	KindText             string // 中文类型：首次部署 / 补跑
	StatusText, Tone     string
	Date, Time           string   // 开始时间拆两行显示，列宽不被撑开
	Segments             []string // 每段的 CSS 类：done/skip/run/fail/（空＝未开始）
	TrackClass           string   // 整条轨的色调：失败的运行整条是红的
	StepLabel            string   // 当前（或失败）的步骤名
	Progress             string   // "7/12"
}

// stepNames是 bootstrap 流水线的规范步骤序列。
// 直接取自 provision.BootstrapStepNames，UI 与流水线不会各写一份而漂移。
var stepNames = provision.BootstrapStepNames()

// kindText把运行类型翻成中文。首次纳管与断点续跑在页面上是两件事，
// 都显示成"bootstrap"等于没说。
func kindText(kind string) string {
	switch kind {
	case "bootstrap":
		return "首次部署"
	case "resume":
		return "补跑"
	}
	return kind
}

func makeRunView(run consolestore.RunSummary, nodeName string) runView {
	text, tone := runTone(run.Status)
	local := run.StartedAt.Local()
	v := runView{
		ID: run.ID, NodeID: run.NodeID, NodeName: nodeName,
		Kind: run.Kind, KindText: kindText(run.Kind),
		StatusText: text, Tone: tone,
		Date: local.Format("2006-01-02"), Time: local.Format("15:04:05"),
	}
	// 失败/异常的运行整条轨染成状态色：不用再去对照右边的状态列，
	// 扫一眼表格就知道哪几次跑挂了。
	switch tone {
	case "bad":
		v.TrackClass = "track-t-bad"
	case "warn":
		v.TrackClass = "track-t-warn"
	}

	byName := map[string]string{}
	for _, st := range run.Steps {
		byName[st.Name] = st.Status
	}
	done := 0
	for _, name := range stepNames {
		switch byName[name] {
		case "succeeded":
			v.Segments = append(v.Segments, "done")
			done++
		case "skipped":
			v.Segments = append(v.Segments, "skip")
			done++
		case "running":
			v.Segments = append(v.Segments, "run")
			v.StepLabel = name
		case "failed":
			v.Segments = append(v.Segments, "fail")
			v.StepLabel = name
		default:
			v.Segments = append(v.Segments, "")
		}
	}
	if v.StepLabel == "" {
		switch {
		case done >= len(stepNames):
			v.StepLabel = "全部完成"
		case done == 0:
			v.StepLabel = "尚未开始"
		default:
			v.StepLabel = "已停在第 " + itoa(done) + " 步之后"
		}
	}
	v.Progress = itoa(done) + "/" + itoa(len(stepNames))
	return v
}

// stepView是运行详情页左栏的一行。未执行到的步骤也会列出来（灰显），
// 这样「还剩哪几步」和「卡在哪一步」同样一目了然。
type stepView struct {
	Seq                int
	Name               string
	Label, Tone, Class string
}

func makeStepViews(run consolestore.RunSummary) ([]stepView, string) {
	byName := map[string]string{}
	for _, st := range run.Steps {
		byName[st.Name] = st.Status
	}
	out := make([]stepView, 0, len(stepNames))
	done := 0
	for i, name := range stepNames {
		label, tone, class := stepTone(byName[name])
		if byName[name] == "succeeded" || byName[name] == "skipped" {
			done++
		}
		out = append(out, stepView{Seq: i + 1, Name: name, Label: label, Tone: tone, Class: class})
	}
	return out, itoa(done) + "/" + itoa(len(stepNames))
}

// stepNote给新增节点页的步骤清单配一句说明——让人知道每步会在机器上做什么，
// 而不是看到一串英文命令名。
var stepNotes = map[string]string{
	"probe":          "探测发行版与磁盘，白名单外当场停",
	"harden":         "生成并注入托管密钥，验证通过后丢弃密码",
	"install-docker": "装 Docker CE 与 compose 插件",
	"dns-allocate":   "分配子域名，等你添加 A 记录",
	"push-source":    "推送源码包，节点靠它构建镜像",
	"push-artifacts": "写入按项目列表渲染的 compose.yaml",
	"render-env":     "在节点本地生成密钥，明文不经 Console",
	"compose-up":     "构建镜像并起栈",
	"cert-wait":      "等待 HTTPS 证书就绪",
	"healthcheck":    "确认容器全部运行、API 可达",
	"init-projects":  "建项目并签发 CDK",
	"disk-guard":     "报告 data-root 位置与磁盘水位",
}

type plannedStep struct {
	Seq        int
	Name, Note string
}

func plannedSteps() []plannedStep {
	out := make([]plannedStep, 0, len(stepNames))
	for i, name := range stepNames {
		out = append(out, plannedStep{Seq: i + 1, Name: name, Note: stepNotes[name]})
	}
	return out
}

func humanWhen(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// itoa避免为几个小整数引入strconv（本文件只做视图拼装）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
