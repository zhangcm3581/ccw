package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 握手被拒时必须把服务端给的原因带出来。
//
// 2026-07-30 真机：旧版客户端连新部署的节点，服务端在 upgrade 之前
// 以 400 "workspace required" 拒掉，而 gorilla 只给
// `websocket: bad handshake`——用户看到这一句，既不知道是版本错配，
// 也不知道 CDK 其实是好的（额度行已经打出来了），于是反复重输 CDK。
func TestWSDialErrorExplainsVersionSkew(t *testing.T) {
	base := errors.New("websocket: bad handshake")

	// 旧客户端：400 + workspace → 必须说清"升级客户端"且"CDK 不用换"
	resp := &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("workspace required\n"))}
	msg := wsDialError(base, resp).Error()
	for _, want := range []string{"旧", "安装", "CDK 不用换"} {
		if !strings.Contains(msg, want) {
			t.Errorf("缺少 %q：%s", want, msg)
		}
	}
	if strings.Contains(msg, "bad handshake") {
		t.Errorf("不该把原文抛给用户：%s", msg)
	}

	// 其他被拒：原样带出状态码与原因，别自己编
	resp = &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("quota exhausted"))}
	msg = wsDialError(base, resp).Error()
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "quota exhausted") {
		t.Errorf("应带出状态码与服务端原因：%s", msg)
	}

	// 没有响应（纯网络错误）时不能吞掉原始错误
	if got := wsDialError(base, nil); got != base {
		t.Errorf("无响应时应原样返回，got %v", got)
	}
}
