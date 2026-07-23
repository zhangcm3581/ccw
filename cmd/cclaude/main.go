// Command cclaude是本地CLI：输入CDK→同步→附着云端终端→状态栏。
//
// 跨平台（Windows/macOS/Linux）：终端用golang.org/x/term做raw mode，
// 窗口尺寸用GetSize轮询（避免依赖Unix专属的SIGWINCH），字节流转发。
// 真实终端与同步的端到端验证在Task 12（需运行中的control-api与worker-agent）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"ccw/internal/control"
	syncpkg "ccw/internal/sync"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	base := envOr("CCW_API", "https://ccw.example.com")
	cdk := os.Getenv("CCW_CDK") // 仅测试用；正常从终端隐式读取
	if cdk == "" {
		fmt.Fprint(os.Stderr, "CDK: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd())) // 不回显、不进shell历史
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read cdk:", err)
			os.Exit(1)
		}
		cdk = string(b)
	}

	c := control.Client{Base: base}
	cwd, _ := os.Getwd()

	// session token在内存中随时可用CDK重新换取（审查§5.3）；CDK不落盘、不入日志。
	sessionToken, err := exchangeSession(ctx, c, cdk)
	if err != nil {
		fmt.Fprintln(os.Stderr, err) // Client保证错误不含CDK
		os.Exit(1)
	}

	for ctx.Err() == nil {
		conn, err := c.Connection(ctx, sessionToken)
		if err != nil {
			// session可能过期：用内存中的CDK重新exchange后重试
			if sessionToken, err = exchangeSession(ctx, c, cdk); err != nil {
				fmt.Fprintln(os.Stderr, err)
				sleep(ctx, 5*time.Second)
			}
			continue
		}
		printStatus(conn)

		if conn.Over {
			// 审查§2.8：超额/磁盘满绝不退出——否则cleanup永远不可达。
			// 不开终端，仅以cleanup模式同步（服务端只允许下载/删除/缩小），
			// 每60秒重新Connection探测窗口恢复，恢复后自动回到正常模式。
			fmt.Fprintf(os.Stderr, "项目受限（%s）：cleanup模式，可下载、删除、缩小文件；额度恢复后自动回到正常模式。\n", conn.OverReason)
			runSync(ctx, cwd, conn) // 阻塞到窗口恢复或断开
			sleep(ctx, 60*time.Second)
			continue
		}

		// 正常：后台同步 + 前台终端。任一返回（断开）后回到循环重连。
		syncDone := make(chan struct{})
		go func() { defer close(syncDone); runSync(ctx, cwd, conn) }()
		if err := runTerminal(ctx, conn); err != nil {
			fmt.Fprintln(os.Stderr, "terminal:", err)
		}
		<-syncDone
		sleep(ctx, time.Second) // 退避后重连
	}
}

func exchangeSession(ctx context.Context, c control.Client, cdk string) (string, error) {
	ex, err := c.Exchange(ctx, cdk)
	if err != nil {
		return "", err
	}
	return ex.SessionToken, nil
}

func printStatus(conn control.ConnectionResponse) {
	fmt.Printf("[%s] 5h:%d/%d 7d:%d/%d disk:%d/%d mode:%s\n", conn.ProjectSlug,
		conn.FiveHourUsed, conn.FiveHourLimit, conn.SevenDayUsed, conn.SevenDayLimit,
		conn.DiskUsed, conn.DiskLimit, conn.SyncMode)
}

// runTerminal附着云端PTY：令牌放Authorization头（无?token=），raw mode双向转发，
// GetSize轮询发resize。断开即返回，由外层循环重连。
func runTerminal(ctx context.Context, conn control.ConnectionResponse) error {
	header := http.Header{"Authorization": {"Bearer " + conn.TerminalToken}}
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, conn.TerminalURL, header)
	if err != nil {
		return err
	}
	defer ws.Close()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), oldState)
	}

	go func() { // PTY→本地
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				ws.Close()
				return
			}
			os.Stdout.Write(data)
		}
	}()

	go resizeLoop(ctx, ws) // 尺寸变化→resize控制帧

	buf := make([]byte, 32<<10)
	for { // 本地→PTY
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
}

func resizeLoop(ctx context.Context, ws *websocket.Conn) {
	var lastR, lastC int
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
			if err != nil || (rows == lastR && cols == lastC) {
				continue
			}
			lastR, lastC = rows, cols
			msg, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
			if ws.WriteMessage(websocket.TextMessage, msg) != nil {
				return
			}
		}
	}
}

// runSync：每2秒一轮三方同步。每轮连sync端点→拉服务端清单→与本地基线Diff→
// 执行上传/下载/删除/冲突副本→持久化新基线。令牌放Authorization头（禁URL参数）。
// ctx取消（终端断开/退出）时停止；cleanup模式下同步仍执行下载/删除，只跳过上传。
func runSync(ctx context.Context, root string, conn control.ConnectionResponse) {
	idx := syncpkg.LocalIndex{Root: root}
	client := &syncpkg.SyncClient{
		Root:   root,
		Device: deviceName(),
		Notify: func(m string) { fmt.Fprintln(os.Stderr, "[sync]", m) },
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		tr, err := syncpkg.DialSync(ctx, conn.SyncURL, conn.SyncToken)
		if err == nil {
			newBase, serr := client.SyncOnce(ctx, tr)
			tr.Close()
			if serr == nil {
				idx.Save(newBase)
			}
		}
		// token过期或断线：由外层循环重新Connection换新token后再起本函数。
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func deviceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "cclaude"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
