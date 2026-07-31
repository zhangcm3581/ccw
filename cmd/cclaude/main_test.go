package main

import (
	"errors"
	"io"
	"net/http"
	"os"
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

// 尺寸探测必须先问 stdout。
//
// Windows 上 term.GetSize 走 GetConsoleScreenBufferInfo，只接受屏幕缓冲区句柄
// （stdout/stderr），传 stdin 必然失败。原来只问 stdin，于是 Windows 上
// resize 一次都没发出去，PTY 停在服务端默认尺寸——界面只占窗口左上角一块。
func TestTermSizeTriesStdoutFirst(t *testing.T) {
	const (
		fdOut = 1
		fdErr = 2
		fdIn  = 0
	)
	var asked []int
	// 模拟 Windows：只有 stdout/stderr 能拿到尺寸，stdin 报错
	win := func(fd int) (int, int, error) {
		asked = append(asked, fd)
		if fd == fdIn {
			return 0, 0, errors.New("invalid handle")
		}
		return 200, 50, nil
	}
	cols, rows, ok := termSize(win, fdOut, fdErr, fdIn)
	if !ok || cols != 200 || rows != 50 {
		t.Fatalf("got %d×%d ok=%v，want 200×50", cols, rows, ok)
	}
	if len(asked) != 1 || asked[0] != fdOut {
		t.Errorf("应先问 stdout 且拿到就停，实际问了 %v", asked)
	}

	// stdout 不可用时退到 stderr
	asked = nil
	outBad := func(fd int) (int, int, error) {
		asked = append(asked, fd)
		if fd == fdOut {
			return 0, 0, errors.New("no")
		}
		return 120, 40, nil
	}
	if c, r, ok := termSize(outBad, fdOut, fdErr, fdIn); !ok || c != 120 || r != 40 {
		t.Errorf("应退到 stderr，got %d×%d ok=%v", c, r, ok)
	}

	// 全部失败（管道/重定向）：ok=false，且不能把 0×0 当成有效尺寸发出去
	none := func(int) (int, int, error) { return 0, 0, errors.New("no") }
	if _, _, ok := termSize(none, fdOut, fdErr, fdIn); ok {
		t.Error("三个都拿不到时应为 false")
	}
	// 返回 0 但 err==nil 也不算数——那会把 PTY 缩成 0 列
	zero := func(int) (int, int, error) { return 0, 0, nil }
	if _, _, ok := termSize(zero, fdOut, fdErr, fdIn); ok {
		t.Error("0×0 不是有效尺寸")
	}
}

// 探测顺序里 stdout 必须排第一——Windows 上只有它（和 stderr）能拿到尺寸。
// 这一条守的是调用点：termSize 本身写得再对，顺序改回 stdin-first 就又坏了。
func TestSizeFDsStdoutFirst(t *testing.T) {
	fds := sizeFDs()
	if len(fds) == 0 || fds[0] != int(os.Stdout.Fd()) {
		t.Fatalf("探测顺序应以 stdout 打头，got %v（stdout=%d）", fds, os.Stdout.Fd())
	}
	if len(fds) < 2 {
		t.Error("stdout 不可用时应还有退路（stderr）")
	}
}
