package console

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"ccw/internal/consolestore"
)

// 在后台里给节点授权 Claude 账号。
//
// 此前只能 SSH 上机手敲 docker exec + tmux（DEPLOY.md 的 A7）。这里把那套
// 搬进后台：Console 在节点上起一个跑 claude 的 tmux 会话，把终端画面原样取回来
// 显示，管理员把 Claude 给出的授权码粘回输入框，Console 送进那个会话。
//
// **不解析 Claude 的输出**：登录提示、URL 形态、步骤数都会随客户端版本变，
// 写死解析等于把后台绑死在某个版本上。画面原样展示，判断交给看的人。
//
// 整台节点只需授权一次（全部项目共用 claude-shared 卷），所以入口放在节点详情页。

func (s *Server) registerClaudeAuth(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/nodes/{id}/claude/start", s.Auth.requireAdmin(s.claudeAuthStart))
	mux.HandleFunc("POST /admin/nodes/{id}/claude/refresh", s.Auth.requireAdmin(s.claudeAuthRefresh))
	mux.HandleFunc("POST /admin/nodes/{id}/claude/key", s.Auth.requireAdmin(s.claudeAuthKey))
	mux.HandleFunc("POST /admin/nodes/{id}/claude/code", s.Auth.requireAdmin(s.claudeAuthCode))
	mux.HandleFunc("POST /admin/nodes/{id}/claude/cancel", s.Auth.requireAdmin(s.claudeAuthCancel))
}

var errNoService = errors.New("要重建的服务不属于本节点；只能重建本节点的项目容器")

// nodeAction是节点级远程操作的公共骨架：CSRF → 取节点与容器 → 执行 → 审计 → 回节点页。
//
// 与cdkAction同款，只是作用域是整台节点。诊断、重建、同步项目、授权都走它。
func (s *Server) nodeAction(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession,
	action string, do func(nodeID string, containers []string) (string, error)) {
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	nodeID := r.PathValue("id")
	node, err := s.Fleet.Store.GetNode(ctx, nodeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.Fleet.Orchestrator == nil {
		s.redirectNode(w, r, nodeID, "", "机队编排器未启用，无法远程执行")
		return
	}

	notice, derr := do(nodeID, s.nodeContainers(ctx, nodeID))
	result := "ok"
	if derr != nil {
		result = "error"
	}
	if aerr := s.Auth.audit(ctx, consolestore.AuditEntry{
		Actor: sess.UserID, Action: action, Target: node.Name, Result: result, ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败: %v", aerr)
	}
	if derr != nil {
		s.redirectNode(w, r, nodeID, "", derr.Error())
		return
	}
	if notice != "" {
		http.Redirect(w, r, "/admin/nodes/"+nodeID+"?ok="+urlQueryEscape(notice), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/nodes/"+nodeID, http.StatusFound)
}

// nodeContainers列出本节点全部项目容器名（容器名恒为 ccw-<slug>，I5契约）。
func (s *Server) nodeContainers(ctx context.Context, nodeID string) []string {
	all, err := s.Fleet.Store.ListNodeProjects(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range all {
		if p.NodeID == nodeID {
			out = append(out, "ccw-"+p.Slug)
		}
	}
	return out
}

// claudeAuthAction是四个动作的公共骨架：CSRF → 取节点与容器 → 执行 → 回节点页。
//
// 画面（可能含登录URL，但不含凭据）经查询串带回页面。授权码**只往节点去**，
// 不回显、不进审计详情——它是一次性凭据。
func (s *Server) claudeAuthAction(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession,
	action string, do func(nodeID, container string) (string, error)) {
	if !s.Auth.checkCSRF(r) {
		http.Error(w, "csrf", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	nodeID := r.PathValue("id")
	node, err := s.Fleet.Store.GetNode(ctx, nodeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 容器检查排在编排器之前：它是纯数据条件，与远程执行能力无关，
	// 而且"还没有项目容器"是管理员能据以行动的提示，
	// "编排器未启用"不是——顺序反了会用后者盖住前者。
	container, ok := s.firstContainer(ctx, nodeID)
	if !ok {
		s.redirectNode(w, r, nodeID, "",
			"本节点还没有项目容器，无法授权；等 init-projects 跑完再来")
		return
	}
	if s.Fleet.Orchestrator == nil {
		s.redirectNode(w, r, nodeID, "", "机队编排器未启用，无法远程执行")
		return
	}

	screen, derr := do(nodeID, container)
	result := "ok"
	if derr != nil {
		result = "error"
	}
	if aerr := s.Auth.audit(ctx, consolestore.AuditEntry{
		Actor: sess.UserID, Action: action, Target: node.Name, Result: result, ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败: %v", aerr)
	}
	if derr != nil {
		s.redirectNode(w, r, nodeID, "", derr.Error())
		return
	}
	s.redirectNode(w, r, nodeID, screen, "")
}

func (s *Server) claudeAuthStart(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.claudeAuthAction(w, r, sess, "claude.auth.start", func(nodeID, container string) (string, error) {
		return s.Fleet.Orchestrator.ClaudeAuthStart(r.Context(), nodeID, container)
	})
}

func (s *Server) claudeAuthRefresh(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.claudeAuthAction(w, r, sess, "claude.auth.refresh", func(nodeID, container string) (string, error) {
		return s.Fleet.Orchestrator.ClaudeAuthCapture(r.Context(), nodeID, container)
	})
}

// claudeAuthKey送一个按键。首次运行的前两屏是选单（主题、登录方式），
// 只有方向键与回车能走——实测见 internal/provision/claudeauth.go 顶部。
func (s *Server) claudeAuthKey(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	key := r.PostFormValue("key")
	s.claudeAuthAction(w, r, sess, "claude.auth.key", func(nodeID, container string) (string, error) {
		return s.Fleet.Orchestrator.ClaudeAuthSendKey(r.Context(), nodeID, container, key)
	})
}

func (s *Server) claudeAuthCode(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	code := r.PostFormValue("code")
	s.claudeAuthAction(w, r, sess, "claude.auth.code", func(nodeID, container string) (string, error) {
		// 授权码只往节点去：不回显、不入审计详情、不进日志。
		return s.Fleet.Orchestrator.ClaudeAuthSendCode(r.Context(), nodeID, container, code)
	})
}

func (s *Server) claudeAuthCancel(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.claudeAuthAction(w, r, sess, "claude.auth.cancel", func(nodeID, container string) (string, error) {
		if err := s.Fleet.Orchestrator.ClaudeAuthCancel(r.Context(), nodeID, container); err != nil {
			return "", err
		}
		s.Fleet.putScreen(nodeID, "") // 会话结束了，别留着上一屏让人以为还在
		return "", nil
	})
}

// firstContainer取该节点上任意一个项目容器名。授权是整机一次的操作
// （全部项目共用 claude-shared 卷），用哪个都一样。
func (s *Server) firstContainer(ctx context.Context, nodeID string) (string, bool) {
	all, err := s.Fleet.Store.ListNodeProjects(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range all {
		if p.NodeID == nodeID {
			return "ccw-" + p.Slug, true
		}
	}
	return "", false
}

// redirectNode回到节点页。
//
// **终端画面走内存缓存而不是查询串**：一屏中文输出百分号编码后接近 10 KB，
// 超过常见反代 8192 的请求行上限——那会变成 414 而不是页面。
// 错误信息短，仍走查询串。
func (s *Server) redirectNode(w http.ResponseWriter, r *http.Request, nodeID, screen, errMsg string) {
	u := "/admin/nodes/" + nodeID
	if errMsg != "" {
		u += "?err=" + urlQueryEscape(errMsg)
	} else {
		s.Fleet.putScreen(nodeID, strings.TrimRight(screen, "\n "))
	}
	http.Redirect(w, r, u, http.StatusFound)
}
