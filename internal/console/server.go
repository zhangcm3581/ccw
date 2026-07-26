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

	rlMu     stdsync.Mutex
	attempts map[string][]time.Time

	tmpl map[string]*template.Template
}

func New(store SiteStore, distDir string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{Store: store, DistDir: distDir, Logf: logf,
		attempts: map[string][]time.Time{}, tmpl: map[string]*template.Template{}}
	for _, page := range []string{"home.html", "download.html", "quickstart.html", "connect.html",
		"admin_login.html", "admin_home.html"} {
		s.tmpl[page] = template.Must(template.ParseFS(tmplFS, "templates/layout.html", "templates/"+page))
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page("home.html", func(*http.Request) (any, error) { return nil, nil }))
	mux.HandleFunc("GET /quickstart", s.page("quickstart.html", func(*http.Request) (any, error) { return nil, nil }))
	mux.HandleFunc("GET /connect", s.page("connect.html", func(*http.Request) (any, error) { return nil, nil }))
	mux.HandleFunc("GET /download", s.download)
	mux.HandleFunc("GET /download/{os}/{arch}", s.downloadRedirect)
	mux.HandleFunc("GET /dist/SHA256SUMS", s.sums) // 字面量路由优先于{filename}通配
	mux.HandleFunc("GET /dist/{filename}", s.dist)
	mux.HandleFunc("POST /v1/resolve", s.resolve)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	if s.Auth != nil {
		s.registerAdmin(mux)
	}
	return mux
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl[page].ExecuteTemplate(w, "layout", data); err != nil {
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

type artifactView struct {
	consolestore.Artifact
	SizeHuman string
}

type downloadData struct {
	Has     bool
	Release consolestore.Release
	Arts    []artifactView
	Rec     *artifactView
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	rel, arts, err := s.Store.LatestPublished(r.Context())
	if err != nil && !errors.Is(err, consolestore.ErrNotFound) {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	d := downloadData{Has: err == nil, Release: rel}
	for _, a := range arts {
		d.Arts = append(d.Arts, artifactView{Artifact: a, SizeHuman: sizeHuman(a.SizeBytes)})
	}
	if osName, arch, ok := detectPlatform(r.UserAgent()); ok {
		if v := pick(d.Arts, osName, arch); v != nil {
			d.Rec = v
		} else if v := pick(d.Arts, osName, "amd64"); v != nil {
			d.Rec = v // 架构探测不到时退到amd64
		}
	}
	s.render(w, "download.html", d)
}

func pick(arts []artifactView, osName, arch string) *artifactView {
	for i := range arts {
		if arts[i].OS == osName && arts[i].Arch == arch {
			return &arts[i]
		}
	}
	return nil
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
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
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

func sizeHuman(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// detectPlatform从User-Agent猜平台，仅用于“为你推荐”展示；完整表格始终可见。
// macOS的UA不区分Intel/Apple Silicon，默认推荐arm64（当前主流），Intel用户从表格选。
func detectPlatform(ua string) (osName, arch string, ok bool) {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "windows"):
		osName = "windows"
	case strings.Contains(l, "mac os") || strings.Contains(l, "macintosh"):
		return "darwin", "arm64", true
	case strings.Contains(l, "linux") || strings.Contains(l, "x11"):
		osName = "linux"
	default:
		return "", "", false
	}
	arch = "amd64"
	if strings.Contains(l, "arm64") || strings.Contains(l, "aarch64") {
		arch = "arm64"
	}
	return osName, arch, true
}
