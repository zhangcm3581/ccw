package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"ccw/internal/token"
)

var syncUpgrader = websocket.Upgrader{ReadBufferSize: 64 << 10, WriteBufferSize: 64 << 10}

const (
	writeWait = 10 * time.Second
	pongWait  = 60 * time.Second
)

type wsReq struct {
	Op     string `json:"op"`
	Device string `json:"device,omitempty"`
	// WS是工作区键：按本地目录隔离云端workspace（见workspace.go）。
	// hello之外的帧不看它——工作区在会话建立时定死，中途不允许换。
	WS           string    `json:"ws,omitempty"`
	Path         string    `json:"path,omitempty"`
	Entry        FileEntry `json:"entry,omitempty"`
	BaseRevision int64     `json:"base_revision,omitempty"`
}

type wsResp struct {
	Op       string      `json:"op"`
	Mode     string      `json:"mode,omitempty"`
	Entries  []FileEntry `json:"entries,omitempty"`
	Entry    *FileEntry  `json:"entry,omitempty"`
	Path     string      `json:"path,omitempty"`
	Revision int64       `json:"revision,omitempty"`
	Reason   string      `json:"reason,omitempty"`
}

// SessionFactory由worker注入：按project+device+mode构造SyncSession（绑定PG store、DirStore、配额）。
type SessionFactory func(projectID, device, mode string) *SyncSession

// ServeSync处理一条同步WebSocket连接。
// 令牌从Authorization头读取（AudSync，禁URL参数）。
// 帧协议：hello/manifest/put(+binary)/get(→file+binary)/delete；
// 完整定义见docs/superpowers/plans/2026-07-19-remote-claude-workspace-plan.md的Task 12。
// maxMessage限制单帧（含文件内容）大小。
func ServeSync(w http.ResponseWriter, r *http.Request, key []byte, maxMessage int64,
	modeFor func(projectID string) string, factory SessionFactory) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := token.Verify(key, raw, token.AudSync, time.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := syncUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxMessage)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(pongWait)) })

	mode := modeFor(claims.ProjectID)
	sess := factory(claims.ProjectID, "", mode)
	writeSyncJSON(conn, wsResp{Op: "auth_ok", Mode: mode})
	// 工作区在hello里确定。**hello之前不接受任何读写帧**：
	// 没有工作区就落盘等于回到"全项目一个平铺目录"，
	// 那正是不同本地目录互相污染的成因。
	ready := false

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			continue // 控制消息必须是 text；binary 只在 put 的紧随帧出现
		}
		var req wsReq
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		if req.Op != "hello" && !ready {
			writeSyncJSON(conn, wsResp{Op: "reject", Reason: "hello_required"})
			continue
		}
		switch req.Op {
		case "hello":
			sess.Device = req.Device
			// 老客户端不发ws：拒绝而不是退回平铺目录。静默兼容会让升级后的
			// 服务端继续制造污染，且没人会发现。
			if !ValidWorkspace(req.WS) {
				writeSyncJSON(conn, wsResp{Op: "reject", Reason: "workspace_required"})
				return
			}
			if err := sess.SetWorkspace(req.WS); err != nil {
				writeSyncJSON(conn, wsResp{Op: "reject", Reason: "workspace_required"})
				return
			}
			ready = true

		case "manifest":
			entries, err := sess.HandleManifest(r.Context())
			if err != nil {
				writeSyncJSON(conn, wsResp{Op: "reject", Reason: "internal"})
				continue
			}
			writeSyncJSON(conn, wsResp{Op: "manifest", Entries: entries})

		case "put":
			// 紧随一个 binary 帧为文件内容，流式传给 HandlePut（不全缓冲进内存）
			mt2, reader, err := conn.NextReader()
			if err != nil {
				return
			}
			if mt2 != websocket.BinaryMessage {
				writeSyncJSON(conn, wsResp{Op: "reject", Path: req.Entry.Path, Reason: "internal"})
				continue
			}
			res := sess.HandlePut(r.Context(), req.Entry.Path, req.BaseRevision, req.Entry.SHA256, reader)
			writeSyncJSON(conn, putResp(req.Entry.Path, res))

		case "get":
			entry, rc, err := sess.HandleGet(r.Context(), req.Path)
			if err != nil {
				writeSyncJSON(conn, wsResp{Op: "reject", Path: req.Path, Reason: "not_found"})
				continue
			}
			e := entry
			writeSyncJSON(conn, wsResp{Op: "file", Entry: &e, Path: entry.Path})
			wc, werr := conn.NextWriter(websocket.BinaryMessage)
			if werr == nil {
				io.Copy(wc, rc)
				wc.Close()
			}
			rc.Close()

		case "delete":
			res := sess.HandleDelete(r.Context(), req.Entry.Path, req.BaseRevision)
			writeSyncJSON(conn, putResp(req.Entry.Path, res))
		}
	}
}

func putResp(path string, r PutResult) wsResp {
	if r.OK {
		return wsResp{Op: "ack", Path: path, Revision: r.Revision}
	}
	return wsResp{Op: "reject", Path: path, Reason: r.Reason}
}

func writeSyncJSON(conn *websocket.Conn, v wsResp) {
	b, _ := json.Marshal(v)
	conn.SetWriteDeadline(time.Now().Add(writeWait))
	conn.WriteMessage(websocket.TextMessage, b)
}
