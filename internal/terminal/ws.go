package terminal

import (
	syncpkg "ccw/internal/sync"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"ccw/internal/token"
)

const (
	maxMessageBytes = 1 << 20 // 单条消息上限，防止内存放大
	writeWait       = 10 * time.Second
	pongWait        = 60 * time.Second
	pingPeriod      = 30 * time.Second
)

var upgrader = websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}

type Resizer interface{ Resize(rows, cols uint16) error }

type ctrlMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Serve验证terminal令牌后把WebSocket与PTY互相转发。
//
// start由worker-agent注入：为该项目准备并附着PTY（docker exec -it tmux attach）。
// 断开只关闭PTY附着进程与WebSocket，绝不kill tmux会话。
// 调用方在调用本函数前必须已实时复查项目额度（审查§3.1：令牌不豁免其后发生的超额）。
func Serve(ctx context.Context, w http.ResponseWriter, r *http.Request, key []byte,
	start func(projectID, ws string) (io.ReadWriteCloser, error)) {
	// 令牌只从Authorization头读取（2分钟短期令牌，可重连）；
	// URL查询参数会进代理日志，禁止使用。
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := token.Verify(key, raw, token.AudTerminal, time.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// 工作区键从请求头取。**不放URL查询参数**（会进反代日志，与令牌同款约束），
	// 也不放令牌——签发令牌的control-api根本不知道客户端在哪个本地目录。
	//
	// **缺失或非法一律拒绝**，与/ws/sync的hello同一口径。此前这里是"退回
	// legacy的/workspace"，那会造出最坏的一种组合：老客户端的终端能用、
	// 同步被拒，用户看到一个正常的终端，文件却一个都没同步，还没有任何提示。
	ws := r.Header.Get("X-CCW-Workspace")
	if !syncpkg.ValidWorkspace(ws) {
		http.Error(w, "workspace required", http.StatusBadRequest)
		return
	}

	// 校验必须排在Upgrade之前：升级会劫持连接，之后再调http.Error写不出任何东西。
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	conn.SetReadLimit(maxMessageBytes)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	pty, err := start(claims.ProjectID, ws)
	if err != nil {
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "pty start failed"),
			time.Now().Add(writeWait))
		return
	}
	defer pty.Close() // 只关PTY附着进程；tmux会话继续存活

	done := make(chan struct{})
	go func() { // PTY→客户端
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				conn.SetWriteDeadline(time.Now().Add(writeWait))
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				conn.Close()
				return
			}
		}
	}()

	go func() { // 心跳
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	for { // 客户端→PTY；Text帧是控制消息，Binary帧是终端字节
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.TextMessage {
			var m ctrlMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				if rz, ok := pty.(Resizer); ok {
					rz.Resize(m.Rows, m.Cols)
				}
			}
			continue
		}
		if _, err := pty.Write(data); err != nil {
			return
		}
	}
}
