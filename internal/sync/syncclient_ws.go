package sync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/gorilla/websocket"
)

// wsTransport用WebSocket实现SyncTransport，对接worker-agent的ServeSync。
type wsTransport struct {
	conn *websocket.Conn
	mode string
}

// DialSync连接同步端点：令牌放Authorization头（禁URL参数），读首帧auth_ok拿mode。
func DialSync(ctx context.Context, url, token string) (*wsTransport, error) {
	header := http.Header{"Authorization": {"Bearer " + token}}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, header)
	if err != nil {
		return nil, err
	}
	var resp wsResp
	if err := conn.ReadJSON(&resp); err != nil || resp.Op != "auth_ok" {
		conn.Close()
		return nil, errors.New("sync: auth handshake failed")
	}
	return &wsTransport{conn: conn, mode: resp.Mode}, nil
}

func (t *wsTransport) Close() error { return t.conn.Close() }

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
