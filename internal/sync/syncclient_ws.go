package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// wsTransport用WebSocket实现SyncTransport，对接worker-agent的ServeSync。
type wsTransport struct {
	conn *websocket.Conn
	mode string
}

var errAuthHandshake = errors.New("sync: auth handshake failed")

// DialSync连接同步端点：令牌放Authorization头（禁URL参数），读首帧auth_ok拿mode。
func DialSync(ctx context.Context, url, token string) (*wsTransport, error) {
	header := http.Header{"Authorization": {"Bearer " + token}}
	// hresp 与下面的 wsResp 不是一回事：这是 HTTP 握手响应。
	// 握手被拒时 gorilla 只给 `websocket: bad handshake`，服务端写在响应体里的
	// 原因全被丢掉——把状态码与原因带出来，否则排查只能靠猜。
	conn, hresp, err := websocket.DefaultDialer.DialContext(ctx, url, header)
	if err != nil {
		if hresp != nil {
			defer hresp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(hresp.Body, 512))
			if reason := strings.TrimSpace(string(b)); reason != "" {
				return nil, fmt.Errorf("同步握手被拒（HTTP %d）：%s", hresp.StatusCode, reason)
			}
			return nil, fmt.Errorf("同步握手被拒（HTTP %d）", hresp.StatusCode)
		}
		return nil, err
	}
	var resp wsResp
	if err := conn.ReadJSON(&resp); err != nil || resp.Op != "auth_ok" {
		conn.Close()
		return nil, errAuthHandshake
	}
	return &wsTransport{conn: conn, mode: resp.Mode}, nil
}

func (t *wsTransport) Close() error { return t.conn.Close() }

// Workspaces列出本项目在云端的全部副本（「管理云端」用）。
func (t *wsTransport) Workspaces() ([]WorkspaceInfo, error) {
	if err := t.conn.WriteJSON(wsReq{Op: "workspaces"}); err != nil {
		return nil, err
	}
	var resp wsResp
	if err := t.conn.ReadJSON(&resp); err != nil {
		return nil, err
	}
	if resp.Op != "workspaces" {
		return nil, fmt.Errorf("sync: 服务端拒绝列出云端副本（%s）", resp.Reason)
	}
	return resp.Workspaces, nil
}

// Purge删掉一个云端副本，返回释放的字节数。
func (t *wsTransport) Purge(ws string) (int64, error) {
	if err := t.conn.WriteJSON(wsReq{Op: "purge", WS: ws}); err != nil {
		return 0, err
	}
	var resp wsResp
	if err := t.conn.ReadJSON(&resp); err != nil {
		return 0, err
	}
	if resp.Op != "purged" {
		return 0, fmt.Errorf("sync: 删除云端副本失败（%s）", resp.Reason)
	}
	return resp.Freed, nil
}

func (t *wsTransport) Hello(device, ws string) (string, error) {
	if err := t.conn.WriteJSON(wsReq{Op: "hello", Device: device, WS: ws}); err != nil {
		return "", err
	}
	return t.mode, nil
}

func (t *wsTransport) Manifest() ([]FileEntry, error) {
	if err := t.conn.WriteJSON(wsReq{Op: "manifest"}); err != nil {
		return nil, err
	}
	var resp wsResp
	if err := t.conn.ReadJSON(&resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

func (t *wsTransport) Put(entry LocalEntry, content io.Reader) (int64, string, error) {
	req := wsReq{
		Op:           "put",
		Entry:        FileEntry{Path: entry.Path, SHA256: entry.CurrentSHA256, Size: entry.Size},
		BaseRevision: entry.BaseRevision,
	}
	if err := t.conn.WriteJSON(req); err != nil {
		return 0, "", err
	}
	w, err := t.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, "", err
	}
	if _, err := io.Copy(w, content); err != nil {
		w.Close()
		return 0, "", err
	}
	if err := w.Close(); err != nil {
		return 0, "", err
	}
	var resp wsResp
	if err := t.conn.ReadJSON(&resp); err != nil {
		return 0, "", err
	}
	if resp.Op == "reject" {
		return 0, resp.Reason, nil
	}
	return resp.Revision, "", nil
}

func (t *wsTransport) Get(path string) (FileEntry, io.ReadCloser, error) {
	if err := t.conn.WriteJSON(wsReq{Op: "get", Path: path}); err != nil {
		return FileEntry{}, nil, err
	}
	var resp wsResp
	if err := t.conn.ReadJSON(&resp); err != nil {
		return FileEntry{}, nil, err
	}
	if resp.Op != "file" || resp.Entry == nil {
		return FileEntry{}, nil, errors.New("sync: get rejected")
	}
	mt, data, err := t.conn.ReadMessage()
	if err != nil {
		return FileEntry{}, nil, err
	}
	if mt != websocket.BinaryMessage {
		return FileEntry{}, nil, errors.New("sync: expected file content frame")
	}
	return *resp.Entry, io.NopCloser(bytes.NewReader(data)), nil
}

func (t *wsTransport) Delete(entry LocalEntry) (int64, string, error) {
	req := wsReq{Op: "delete", Entry: FileEntry{Path: entry.Path}, BaseRevision: entry.BaseRevision}
	if err := t.conn.WriteJSON(req); err != nil {
		return 0, "", err
	}
	var resp wsResp
	if err := t.conn.ReadJSON(&resp); err != nil {
		return 0, "", err
	}
	if resp.Op == "reject" {
		return 0, resp.Reason, nil
	}
	return resp.Revision, "", nil
}
