// Package console实现ccw-console的公开站点（console-fleet-design §3、§6.6）：
// 落地页、下载分发、CDK查询页。管理后台（/admin/*）在C3/C15实施，本包暂不注册
// 任何admin路由——没有认证之前不上任何管理页面。
package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	stdsync "sync"
	texttemplate "text/template"
	"time"

	"ccw/internal/consolestore"
)

//go:embed templates
var tmplFS embed.FS

// SiteStore是站点需要的存储能力面；生产实现是*consolestore.Store，测试用假实现。
type SiteStore interface {
	LatestPublished(ctx context.Context) (consolestore.Release, []consolestore.Artifact, error)
	ArtifactByFilename(ctx context.Context, filename string) (consolestore.Artifact, error)
	ResolveAPIDomain(ctx context.Context, publicID string) (string, error)
}

type Server struct {
	Store   SiteStore
	DistDir string
	// Logf是唯一日志出口；**任何handler都不得把请求体写进它**——
	// /v1/resolve的请求体可能被误粘贴成完整CDK（设计§6.6：不记录请求体）。
	Logf             func(format string, a ...any)
	MaxResolvePerMin int // 0=默认10（设计§6.6）

	// Auth为nil时**不注册任何/admin路由**：没有认证就不上管理页面。
	Auth *Auth
	// Fleet为nil时不注册机队管理页（需要SSH执行层与信封加密齐备才有意义）。
	Fleet *Fleet
	// AdminHost是管理后台的域名。设置后按Host分流：管理路由只在该域名上存在，
	// 官网路由只在其它域名上存在。留空＝不分流（本地开发）。
	AdminHost string

	rlMu     stdsync.Mutex
	attempts map[string][]time.Time

	tmpl map[string]*template.Template
	// text是纯文本模板（一键安装脚本）。**不能用html/template**：
	// 它会把引号转义成&#34;，脚本直接跑不起来。
	text map[string]*texttemplate.Template
}

func New(store SiteStore, distDir string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{Store: store, DistDir: distDir, Logf: logf,
		attempts: map[string][]time.Time{}, tmpl: map[string]*template.Template{},
		text: map[string]*texttemplate.Template{}}
	// 公开站点与管理后台是两套外壳：站点是可读的文档页，后台是操作台。
	// 用不同的 layout 而不是同一个套两种样式——它们的信息密度与导航模型不同。
	for _, page := range []string{"home.html", "connect.html"} {
		s.tmpl[page] = template.Must(template.ParseFS(tmplFS, "templates/layout.html", "templates/"+page))
	}
	// pct把万分之一转成百分数。档位比例存整数 bp（限额要参与 ">=" 比较，
	// 浮点会带来边界抖动），但界面上必须显示成人看的百分比。
	adminFuncs := template.FuncMap{
		"pct": func(bp int) float64 { return float64(bp) / 100 },
	}
	for _, page := range []string{"admin_dashboard.html", "admin_nodes.html",
		"admin_node_new.html", "admin_node.html", "admin_run.html",
		"admin_cdks.html", "admin_domains.html", "admin_audit.html", "admin_usage.html"} {
		s.tmpl[page] = template.Must(template.New("admin_layout.html").Funcs(adminFuncs).
			ParseFS(tmplFS, "templates/admin_layout.html", "templates/"+page))
	}
	// 登录页没有侧边栏（此时还没登录），自带完整文档结构。
	s.tmpl["admin_login.html"] = template.Must(template.ParseFS(tmplFS, "templates/admin_login.html"))
	// 一键安装脚本：纯文本，不套任何layout。用text/template而不是html/template——
	// 目标是shell与PowerShell，HTML转义会把引号变成&#34;，脚本直接跑不起来。
	for _, name := range []string{"install.sh", "install.ps1"} {
		b, err := tmplFS.ReadFile("templates/" + name)
		if err != nil {
			panic(err)
		}
		s.text[name] = texttemplate.Must(texttemplate.New(name).Parse(string(b)))
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page("home.html", func(*http.Request) (any, error) { return nil, nil }))
	// /connect 是唯一的上手页：粘 CDK → 拿到填好域名的两条命令。
	// 原先的 /quickstart 把同样的步骤又写了一遍，而且只能给占位符域名
	// （`--api https://api-01.example.com`，还要读的人自己替换）——
	// 有了 CDK 就能直接给真域名，那个页面没有存在的必要。旧链接重定向过来。
	mux.HandleFunc("GET /quickstart", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/connect", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /connect", s.page("connect.html", func(r *http.Request) (any, error) {
		return map[string]any{"Site": siteURL(r)}, nil
	}))
	// 下载页已于 2026-08-01 移除：安装命令在 /connect 上，而且那里能填好域名。
	// **重定向而不是 404**：导航与外部链接都指过它。
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/connect", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /download/{os}/{arch}", s.downloadRedirect)
	mux.HandleFunc("GET /install.sh", s.installScript("install.sh", "text/x-shellscript"))
	mux.HandleFunc("GET /install.ps1", s.installScript("install.ps1", "text/plain"))
	mux.HandleFunc("GET /dist/SHA256SUMS", s.sums) // 字面量路由优先于{filename}通配
	mux.HandleFunc("GET /dist/{filename}", s.dist)
	mux.HandleFunc("POST /v1/resolve", s.resolve)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	if s.Auth != nil {
		s.registerAdmin(mux)
		if s.Fleet != nil {
			s.registerFleet(mux)
		}
	}
	return s.hostRouter(mux)
}

// hostRouter按Host把两个域名的路由面切开。
//
// **为什么必须在应用层做**：Caddy的两个站点块转发到同一个后端进程，
// 后端只按路径分发的话，/admin/* 在官网域名上照样可达——
// 「后台单独一个域名」就成了摆设，Caddy 上那份 IP 白名单也绕过去了
// （它只挂在管理域名的站点块上）。
//
// 分流规则：
//   - 管理域名：只有 /admin/* 与 /healthz；根路径重定向到登录页；其余404
//   - 其它域名：官网全部路由；/admin/* 一律404
//
// AdminHost为空时不分流——本地开发没有域名，且此时Caddy也没在前面。
func (s *Server) hostRouter(next http.Handler) http.Handler {
	if s.AdminHost == "" {
		return next
	}
	admin := strings.ToLower(s.AdminHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		onAdmin := host == admin
		adminPath := r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/")

		switch {
		case onAdmin && r.URL.Path == "/":
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		case onAdmin && !adminPath && r.URL.Path != "/healthz":
			http.NotFound(w, r) // 管理域名上不提供官网内容
			return
		case !onAdmin && adminPath:
			http.NotFound(w, r) // 管理路由只在管理域名上存在
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	root := "layout"
	if page == "admin_login.html" {
		root = "login"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl[page].ExecuteTemplate(w, root, data); err != nil {
		s.Logf("console: render %s: %v", page, err)
	}
}

func (s *Server) page(name string, data func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := data(r)
		if err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		s.render(w, name, d)
	}
}

// ---- 下载页与产物分发 ----

// siteURL从请求还原出用户正在访问的站点地址。
// 一键安装命令与脚本里的下载地址都要指回这里，写死域名会在换域名/本地开发时失效。
func siteURL(r *http.Request) string {
	scheme := "https"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS == nil {
		scheme = "http" // 本地开发
	}
	return scheme + "://" + r.Host
}

// installScript渲染一键安装脚本。
//
// **校验和内嵌在脚本里**：脚本与产物来自同一个站点，再让脚本去同一个站点取一份
// SHA256SUMS，校验不了任何东西——能改产物的人也能改那个文件。内嵌之后，
// 用户在浏览器里读一遍脚本，看到的就是他将要安装的那份二进制的指纹。
//
// 站点地址从请求的Host推出：这个脚本要被`curl | sh`执行，里面的下载地址
// 必须指回用户正在访问的这个站点，不能写死。
func (s *Server) installScript(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel, arts, err := s.Store.LatestPublished(r.Context())
		if err != nil {
			// 没有已发布版本时给出可读的提示而不是空脚本——
			// `curl | sh` 执行一个空文件会静默成功，那更难排查。
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "echo 'cclaude: 本站尚未发布任何客户端版本，请联系管理员' >&2; exit 1")
			return
		}
		data := map[string]any{
			"Site":    siteURL(r),
			"Version": rel.Version,
			"Arts":    arts,
		}
		w.Header().Set("Content-Type", contentType+"; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store") // 版本变了要立刻拿到新的
		if err := s.text[name].Execute(w, data); err != nil {
			s.Logf("console: render %s: %v", name, err)
		}
	}
}

func (s *Server) downloadRedirect(w http.ResponseWriter, r *http.Request) {
	osName, arch := r.PathValue("os"), r.PathValue("arch")
	_, arts, err := s.Store.LatestPublished(r.Context())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	for _, a := range arts {
		if a.OS == osName && a.Arch == arch {
			http.Redirect(w, r, "/dist/"+a.Filename, http.StatusFound)
			return
		}
	}
	http.NotFound(w, r)
}

// SHA256SUMS从数据库生成（当前已发布版本），与下载表同源——
// 不读磁盘上构建脚本生成的文件，避免两处口径漂移。格式兼容shasum -c。
func (s *Server) sums(w http.ResponseWriter, r *http.Request) {
	_, arts, err := s.Store.LatestPublished(r.Context())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, a := range arts {
		fmt.Fprintf(w, "%s  %s\n", a.SHA256, a.Filename)
	}
}

// distFilenameRe：产物文件名只可能是register-release登记的形态；
// 其它一律404。这里不做路径拼接之外的任何解释（无目录分量、无“..”可乘之机）。
var distFilenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// dist只发**已发布版本登记过的**文件（设计§3.2：下载页从表渲染、不扫目录）；
// 磁盘上多出来的任何文件——半成品、临时文件——都不可达。
func (s *Server) dist(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("filename")
	if !distFilenameRe.MatchString(name) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	if _, err := s.Store.ArtifactByFilename(r.Context(), name); err != nil {
		http.NotFound(w, r) // 未登记/未发布：统一404
		return
	}
	f, err := os.Open(filepath.Join(s.DistDir, name))
	if err != nil {
		// 库里有、盘上没有＝发布流程漂移，值得记日志（只记文件名，无用户输入）。
		s.Logf("console: 产物已登记但磁盘缺失: %s", name)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// ---- /v1/resolve（设计§6.6，安全设计是核心不是附加项）----

var publicIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	if !s.allowResolve(clientIP(r)) {
		jsonErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var body struct {
		PublicID string `json:"public_id"`
	}
	// 请求体只可能是{"public_id":"16位hex"}；1KB上限，解析失败不记录任何内容。
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// 含"."说明前端切分被绕过或用户直接调API贴了完整CDK：
	// 立即拒绝，且**不记录请求体**（它此刻可能含secret）。
	if strings.Contains(body.PublicID, ".") {
		jsonErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !publicIDRe.MatchString(body.PublicID) {
		jsonErr(w, http.StatusNotFound, "not_found") // 格式不对≈查不到，统一口径
		return
	}
	fqdn, err := s.Store.ResolveAPIDomain(r.Context(), body.PublicID)
	if err != nil {
		if errors.Is(err, consolestore.ErrNotFound) {
			// 统一「未找到」：不区分不存在/已撤销/已退役（延伸invalid_cdk规则）。
			jsonErr(w, http.StatusNotFound, "not_found")
			return
		}
		s.Logf("console: resolve查询失败: %v", err) // 基础设施错误，不含用户输入
		jsonErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"api": "https://" + fqdn})
}

// allowResolve：每IP每分钟滑动窗口限速（默认10次，设计§6.6）。
// public-id是2^64空间、枚举不可行，限速仍是必需的兜底。
// 与internal/control/http.go的allowAuth同构；出现第三个使用者时再抽公共包。
func (s *Server) allowResolve(key string) bool {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	max := s.MaxResolvePerMin
	if max <= 0 {
		max = 10
	}
	cutoff := time.Now().Add(-time.Minute)
	kept := s.attempts[key][:0]
	for _, t := range s.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		s.attempts[key] = kept
		return false
	}
	s.attempts[key] = append(kept, time.Now())
	return true
}

// clientIP：Console跑在Caddy后面，RemoteAddr是反代地址；
// 取X-Forwarded-For第一跳（Caddy会设置），没有时退回RemoteAddr。
// clientIP取真实客户端IP，用于IP白名单、限速与审计。
//
// **取X-Forwarded-For的最后一段，不是第一段。**Caddy是**追加**而不是覆盖：
// 客户端自己发的XFF会被保留，真实IP追加在后面。取第一段等于直接采信
// 客户端伪造的值，后果是白名单被绕过、每IP限速被轮换伪造IP规避、
// 审计日志记录假地址。最后一段是我们自己的反代写进去的，不可伪造。
//
// 这个取法的前提是**恰好一层受信反代**（本项目的部署形态就是如此：
// Console只经自己那台Caddy暴露）。若将来在前面再加一层CDN或LB，
// 必须改为按受信代理层数从右往左跳过对应个数的条目——否则会把
// 中间那层的IP当成客户端。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
