package sync

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"
)

// CloudManager是「管理云端」这一屏需要的能力。
// 单独一个连接、单独一个接口：它与常规同步循环无关，
// 混进 SyncTransport 会让每个假传输都得实现两个用不到的方法。
type CloudManager interface {
	Workspaces() ([]WorkspaceInfo, error)
	Purge(ws string) (int64, error)
	Close() error
}

// DialCloud建一条只用来管理云端副本的同步连接。
//
// hello 仍要报一个工作区键——服务端要求它，且拒绝空值。这里报当前工作区，
// 但后续两个 op 都是项目级的，与报的是哪个副本无关。
func DialCloud(ctx context.Context, url, token, device, ws string) (CloudManager, error) {
	header := http.Header{"Authorization": {"Bearer " + token}}
	conn, hresp, err := websocket.DefaultDialer.DialContext(ctx, url, header)
	if err != nil {
		if hresp != nil {
			hresp.Body.Close()
		}
		return nil, err
	}
	t := &wsTransport{conn: conn}
	var resp wsResp
	if err := conn.ReadJSON(&resp); err != nil || resp.Op != "auth_ok" {
		conn.Close()
		return nil, errAuthHandshake
	}
	if _, err := t.Hello(device, ws); err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}
