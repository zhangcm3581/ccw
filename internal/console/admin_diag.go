package console

import (
	"net/http"
	"slices"
	"strings"

	"ccw/internal/consolestore"
	"ccw/internal/provision"
)

// 节点诊断与维护：把 docs/admin-login-runbook.md 与 claude-auth-quickref.md 里
// 那些「登机敲一遍」的命令搬进后台。
//
// 覆盖的手动步骤：
//   - docker ps（容器在不在跑）
//   - docker exec … claude auth status（每个项目登没登录）
//   - ls -l 凭据文件（卷权限，登录写不进去的最常见原因）
//   - df -h / + docker info（磁盘水位与 data-root）
//   - docker compose up -d --force-recreate（重建后凭据仍在＝卷持久）
//   - ccwadmin list-projects（把项目清单同步进 Console 镜像）

func (s *Server) registerDiag(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/nodes/{id}/diag", s.Auth.requireAdmin(s.adminNodeDiag))
	mux.HandleFunc("POST /admin/nodes/{id}/recreate", s.Auth.requireAdmin(s.adminNodeRecreate))
	mux.HandleFunc("POST /admin/nodes/{id}/sync-projects", s.Auth.requireAdmin(s.adminNodeSyncProjects))
	mux.HandleFunc("POST /admin/nodes/{id}/reset", s.Auth.requireAdmin(s.adminNodeReset))
}

// adminNodeReset把节点擦回「装了Docker的干净机器」。
//
// 与「解除纳管」正交：
//   - 重置：擦远端，Console 的账留着——擦完点「继续 / 重新部署」就是一次全新部署
//   - 解除纳管：清 Console 的账，远端一动不动
//
// 要求把节点名原样打一遍。**审计先于动作**：擦除不可撤销，事后写不进去
// 也已经擦了，那还不如先把"谁要擦哪台"记下来（§8.5）。
func (s *Server) adminNodeReset(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
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
	if strings.TrimSpace(r.PostFormValue("confirm")) != node.Name {
		s.redirectNode(w, r, nodeID, "", "确认文本与节点名不一致，未执行重置")
		return
	}
	if s.Fleet.Orchestrator == nil {
		s.redirectNode(w, r, nodeID, "", "机队编排器未启用，无法远程执行")
		return
	}
	if aerr := s.Auth.audit(ctx, consolestore.AuditEntry{
		Actor: sess.UserID, Action: "node.reset", Target: node.Name, Result: "ok",
		Detail: map[string]any{"host": node.Host}, ClientIP: clientIP(r),
	}); aerr != nil {
		s.Logf("console: 审计写入失败，重置中止: %v", aerr)
		s.redirectNode(w, r, nodeID, "", "服务暂时不可用，请稍后再试")
		return
	}

	out, rerr := s.Fleet.Orchestrator.ResetNode(ctx, nodeID)
	if rerr != nil {
		s.redirectNode(w, r, nodeID, "", rerr.Error())
		return
	}
	// 擦除结果原文显示在诊断区：容器/卷/源码树各剩多少，一眼能确认擦干净了。
	s.Fleet.putDiag(nodeID, []provision.DiagSection{{Title: "重置结果", Output: out}})
	http.Redirect(w, r, "/admin/nodes/"+nodeID+"?ok="+
		urlQueryEscape("已擦除远端环境；点「继续 / 重新部署」开始一次全新部署"), http.StatusFound)
}

func (s *Server) adminNodeDiag(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.nodeAction(w, r, sess, "node.diagnose", func(nodeID string, containers []string) (string, error) {
		// **没有项目容器时照跑**：节点级检查（docker ps、磁盘、data-root）
		// 恰恰是这种时候最需要——纳管卡在 init-projects 之前，你想知道的
		// 正是"到底起来了什么"。只有逐容器的那几段会被跳过。
		secs, err := s.Fleet.Orchestrator.Diagnose(r.Context(), nodeID, containers)
		if err != nil {
			return "", err
		}
		s.Fleet.putDiag(nodeID, secs)
		return "", nil
	})
}

func (s *Server) adminNodeRecreate(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	service := strings.TrimSpace(r.PostFormValue("service"))
	s.nodeAction(w, r, sess, "node.recreate", func(nodeID string, containers []string) (string, error) {
		// 只允许重建本节点已知的项目服务。名字虽然经shellQuote不会注入，
		// 但把 postgres / caddy / control-api 也变成一个可点的入口不是本意——
		// 那几个的重建后果与项目容器完全不同，该有各自的设计。
		if !slices.Contains(containers, "ccw-"+service) {
			return "", errNoService
		}
		if _, err := s.Fleet.Orchestrator.RecreateContainer(r.Context(), nodeID, service); err != nil {
			return "", err
		}
		return "已重建 " + service + "；数据在命名卷里不受影响，但附着中的终端会断开", nil
	})
}

// adminNodeSyncProjects把节点上的项目清单同步进Console镜像。
//
// 镜像只在纳管跑init-projects时写入。早于该功能上线的节点、或那次写失败的，
// 镜像会是空的——/connect解析不到、CDK页也是空的。有了这个就能补齐，
// 不必为了补一行记录去重跑一遍部署。
func (s *Server) adminNodeSyncProjects(w http.ResponseWriter, r *http.Request, sess consolestore.AdminSession) {
	s.nodeAction(w, r, sess, "node.sync-projects", func(nodeID string, _ []string) (string, error) {
		rows, err := s.Fleet.Orchestrator.ListProjectsOnNode(r.Context(), nodeID)
		if err != nil {
			return "", err
		}
		for _, p := range rows {
			if _, err := s.Fleet.Store.UpsertNodeProject(r.Context(), nodeID, p.Slug, p.ProjectID,
				p.DiskGiB<<30, p.FiveHour, p.SevenDay); err != nil {
				return "", err
			}
		}
		return "已从节点同步 " + itoa(len(rows)) + " 个项目", nil
	})
}
