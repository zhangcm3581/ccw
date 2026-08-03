package control

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"strings"
	stdsync "sync"
	"time"

	"ccw/internal/project"
	"ccw/internal/quota"
	"ccw/internal/storage"
	"ccw/internal/token"
)

//go:embed templates
var templatesFS embed.FS

const connTokenTTL = 2 * time.Minute // 短期连接令牌（审查§2.3）

type ConnectionResponse struct {
	ProjectID     string `json:"project_id"`
	ProjectSlug   string `json:"project_slug"`
	TerminalURL   string `json:"terminal_url"`
	SyncURL       string `json:"sync_url"`
	TerminalToken string `json:"terminal_token"`
	SyncToken     string `json:"sync_token"`
	SyncMode      string `json:"sync_mode"` // "rw"或"cleanup"（超额/磁盘满时只许下载、删除、缩小）
	DiskUsed      int64  `json:"disk_used"`
	DiskLimit     int64  `json:"disk_limit"`
	FiveHourUsed  int64  `json:"five_hour_used"`
	FiveHourLimit int64  `json:"five_hour_limit"`
	SevenDayUsed  int64  `json:"seven_day_used"`
	SevenDayLimit int64  `json:"seven_day_limit"`
	Over          bool   `json:"over"`
	OverReason    string `json:"over_reason,omitempty"`
}

type Server struct {
	Resolver   project.Resolver
	GetProject func(context.Context, string) (project.Project, error) // 一律查库，无进程内会话状态（审查§5.4）
	Key        []byte
	Quota      quota.Service
	Index      storage.Index
	// LimitsFor返回error：池上限来自数据库，读不到时不能假装拿到了限额。
	// 此前它无error且从环境变量读，与worker-agent的口径不一致（见quota.Assemble的说明）。
	LimitsFor func(context.Context, project.Project) (quota.Limits, error)
	// WindowsFor返回该账号当前的窗口起点（对齐 Claude 的 resets_at）。
	// **可为 nil**：那时用零值 Windows，Check 会退回滚动窗口——
	// 与 2026-08-02 之前的行为一致，是安全的降级。
	WindowsFor func(context.Context, string) quota.Windows
	AgentBase  string

	MaxAuthAttempts int // 每分钟每客户端的exchange尝试上限（0=默认20）

	rlMu     stdsync.Mutex
	attempts map[string][]time.Time
}

func New(r project.Resolver, getProject func(context.Context, string) (project.Project, error),
	key []byte, q quota.Service, idx storage.Index,
	limitsFor func(context.Context, project.Project) (quota.Limits, error), agentBase string) *Server {
	return &Server{Resolver: r, GetProject: getProject, Key: key, Quota: q, Index: idx,
		LimitsFor: limitsFor, AgentBase: agentBase, attempts: map[string][]time.Time{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/exchange", s.exchange)
	mux.HandleFunc("GET /v1/connection", s.connection)
	mux.HandleFunc("GET /usage", s.usagePage)
	return mux
}

// allowAuth：按客户端key做每分钟滑动窗口限速（审查§3.1，IP维度；
// public-id维度可在key后追加，此处以IP为主控暴力尝试）。
func (s *Server) allowAuth(key string) bool {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	max := s.MaxAuthAttempts
	if max <= 0 {
		max = 20
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

func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	if !s.allowAuth(clientIP(r)) {
		httpErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var body struct {
		CDK string `json:"cdk"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		httpErr(w, http.StatusUnauthorized, "invalid_cdk")
		return
	}
	p, err := s.Resolver.ResolveCDK(r.Context(), body.CDK)
	if err != nil {
		httpErr(w, http.StatusUnauthorized, "invalid_cdk") // 统一错误：不泄露CDK是否存在/过期/禁用
		return
	}
	tok, err := token.Mint(s.Key, p.ID, token.AudSession, 15*time.Minute, time.Now())
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, map[string]string{
		"session_token": tok, "project_id": p.ID, "project_slug": p.Slug,
	})
}

// windowsFor取窗口起点；未注入时返回零值＝滚动窗口。
func (s *Server) windowsFor(ctx context.Context, accountID string) quota.Windows {
	if s.WindowsFor == nil {
		return quota.Windows{}
	}
	return s.WindowsFor(ctx, accountID)
}

func (s *Server) authed(r *http.Request) (project.Project, bool) {
	h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := token.Verify(s.Key, h, token.AudSession, time.Now())
	if err != nil {
		return project.Project{}, false
	}
	p, err := s.GetProject(r.Context(), c.ProjectID) // 一律查库，重启后仍有效
	return p, err == nil
}

func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authed(r)
	if !ok {
		httpErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	now := time.Now()
	lim, err := s.LimitsFor(r.Context(), p)
	if err != nil {
		// 读不到限额时返回500，而不是退化成"无限额"或"已超额"：
		// 前者会放行本该被拦的请求，后者会对客户谎称额度耗尽。
		httpErr(w, http.StatusInternalServerError, "internal")
		return
	}
	d, err := s.Quota.Check(r.Context(), p.ID, p.AccountID, lim, now, s.windowsFor(r.Context(), p.AccountID))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "internal")
		return
	}
	disk, _ := s.Index.DiskUsed(r.Context(), p.ID)
	resp := ConnectionResponse{
		ProjectID: p.ID, ProjectSlug: p.Slug,
		TerminalURL: s.AgentBase + "/terminal", SyncURL: s.AgentBase + "/sync",
		DiskUsed: disk, DiskLimit: p.DiskLimit,
		// **限额必须来自 lim，不能读 p 的原始列**：挂了档位的项目，
		// 判定用的是档位折算后的限额（lim），而 p.FiveHourLimit 是没折算的绝对值。
		// 两者混用的效果是客户端打印 "7d:8132044/10000000 项目受限"——
		// 数字自己就否定了那句提示，看的人只会以为系统坏了（2026-08-03 真机）。
		FiveHourUsed: d.FiveHourUsed, FiveHourLimit: lim.FiveHour,
		SevenDayUsed: d.SevenDayUsed, SevenDayLimit: lim.SevenDay,
		Over: d.Over, OverReason: d.Reason,
	}
	if disk >= p.DiskLimit && !resp.Over {
		resp.Over, resp.OverReason = true, "disk_limit"
	}
	// 超额/磁盘满：不发终端令牌，但sync降级为cleanup模式照发（仍能下载/删除/缩小，审查§7）
	resp.SyncMode = "rw"
	if resp.Over {
		resp.SyncMode = "cleanup"
	} else {
		resp.TerminalToken, _ = token.Mint(s.Key, p.ID, token.AudTerminal, connTokenTTL, now)
	}
	resp.SyncToken, _ = token.Mint(s.Key, p.ID, token.AudSync, connTokenTTL, now)
	writeJSON(w, resp)
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authed(r)
	if !ok {
		httpErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	t := template.Must(template.ParseFS(templatesFS, "templates/usage.html"))
	lim, err := s.LimitsFor(r.Context(), p)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "internal")
		return
	}
	d, _ := s.Quota.Check(r.Context(), p.ID, p.AccountID, lim, time.Now(), s.windowsFor(r.Context(), p.AccountID))
	disk, _ := s.Index.DiskUsed(r.Context(), p.ID)
	t.Execute(w, map[string]any{"Project": p, "Decision": d, "DiskUsed": disk})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
