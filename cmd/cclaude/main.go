// Command cclaude是本地CLI：输入CDK→同步→附着云端终端→状态栏。
//
// 跨平台（Windows/macOS/Linux）：终端用golang.org/x/term做raw mode，
// 窗口尺寸用GetSize轮询（避免依赖Unix专属的SIGWINCH），字节流转发。
// 真实终端与同步的端到端验证在tests/e2e（需运行中的control-api与worker-agent），
// 目前仍是skip状态，见docs/STATUS.md。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"ccw/internal/control"
	syncpkg "ccw/internal/sync"
)

// buildVersion由发布流水线经-ldflags "-X main.buildVersion=..."注入（设计§3.2）；
// cclaude --version输出它（验收A3：与下载页版本号一致）。
var buildVersion = "dev"

func main() {
	// 域名可以做参数（不是机密）；CDK绝不做参数——参数会进shell history与ps输出（A24）。
	apiFlag := flag.String("api", "", "服务端API地址（如 https://api-01.example.com）；显式指定时写入本地配置")
	// --dir 跳过选择器直接同步某个目录：脚本、CI 与"我就是要同步这里"的用法。
	dirFlag := flag.String("dir", "", "直接同步指定目录，跳过项目选择器")
	// -d：以 Bypass Permissions 模式启动云端的 Claude（跳过权限确认）。
	// 官方说这个模式只应在可随时恢复的沙箱容器里用——本项目的容器正是那种。
	bypass := flag.Bool("d", false, "以 Bypass Permissions 模式启动（跳过权限确认；仅在新建云端会话时生效）")
	showVersion := flag.Bool("version", false, "输出版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println("cclaude", buildVersion)
		return
	}

	dir := configDir()
	if flag.Arg(0) == "logout" {
		if err := clearLocalConfig(dir); err != nil {
			fmt.Fprintln(os.Stderr, "logout:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "已清除本地配置（API地址与CDK）")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 首次配置的顺序刻意是先域名后CDK：域名输错立刻能从连接失败看出来，
	// CDK输错则要等到认证阶段（设计§6.7）。
	base, err := resolveAPI(dir, *apiFlag, os.Getenv, promptLine)
	if err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
	cdk, cerr := resolveCDK(dir, os.Getenv, promptSecret)
	if cerr != nil {
		fmt.Fprintln(os.Stderr, "read cdk:", cerr)
		os.Exit(1)
	}

	c := control.Client{Base: base}

	// session token在内存中随时可用CDK重新换取（审查§5.3）；CDK不入日志。
	//
	// **排在选择器之前**：选择器里的「管理云端」要连服务端列副本，
	// 没有令牌就开不了那一屏。认证失败也该在弹界面之前就说清楚。
	sessionToken, err := exchangeSession(ctx, c, cdk)
	if err != nil {
		// 缓存的CDK可能已失效（如被轮换/撤销）：清CDK但保留API，下次只需重输CDK。
		// 环境变量提供的CDK不动本地缓存。
		if os.Getenv("CCW_CDK") == "" {
			clearCDKField(dir)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 同步根目录（2026-07-31）：不再同步"你碰巧所在的那个目录"。
	// resolveWorkDir 决定这次要同步哪个项目——已经在某个项目里就直接用它，
	// 否则弹选择器。--dir 显式指定时一切照旧，脚本与 CI 不受影响。
	cwd, err := resolveWorkDir(dir, *dirFlag, func() error {
		return openCloudManager(ctx, c, sessionToken, dir)
	})
	if err != nil {
		if err == errUserQuit {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	bypassNoticeShown := false
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
			runSync(ctx, cwd, c, cdk, sessionToken, conn) // 阻塞到窗口恢复或断开
			sleep(ctx, 60*time.Second)
			continue
		}

		if *bypass && !bypassNoticeShown {
			bypassNoticeShown = true
			// 只说一句：详细提示由服务端在 tmux 状态行里给。
			// 这里打的东西会被 Claude 的 alt screen 立刻清掉，不能指望它被看到。
			fmt.Fprintln(os.Stderr, "已请求 Bypass Permissions 模式（仅在新建云端会话时生效）")
		}

		// 正常：后台同步 + 前台终端。任一返回（断开）后回到循环重连。
		syncDone := make(chan struct{})
		go func() { defer close(syncDone); runSync(ctx, cwd, c, cdk, sessionToken, conn) }()
		if err := runTerminal(ctx, conn, syncpkg.WorkspaceKey(cwd), permMode(*bypass)); err != nil && !isExpectedClose(err) {
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

// isExpectedClose判断这是不是一次"意料之中"的断开。
//
// 服务端因为额度用尽主动关连接时，客户端这边只会看到
// `use of closed network connection` 之类的原始网络错误——把它原样打出来
// 毫无信息量，而且**紧接着下一轮 Connection 就会返回真正的原因**
// （项目受限 + reason），那条消息才是该看的。
//
// 所以这类断开不打原始错误，让下一轮的说明来解释。真正的异常（DNS、拒绝连接、
// 证书）不在此列，仍要打出来。
func isExpectedClose(err error) bool {
	if err == nil {
		return true
	}
	s := err.Error()
	for _, quiet := range []string{
		"use of closed network connection",
		"websocket: close sent",
		"connection reset by peer",
		"EOF",
	} {
		if strings.Contains(s, quiet) {
			return true
		}
	}
	return false
}

// wsDialError把握手失败翻成能定位的错误。
//
// gorilla 在握手被拒时只给 `websocket: bad handshake`——服务端明明在响应体里
// 写了原因（如 "workspace required"），却全被丢掉。2026-07-30 真机上就是这样：
// 旧版客户端连新节点，用户看到一句 bad handshake，既不知道是版本错配，
// 也不知道 CDK 其实是好的（额度那行已经打出来了）。
func wsDialError(err error, resp *http.Response) error {
	if resp == nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	reason := strings.TrimSpace(string(body))
	// 400 + workspace 相关＝这个客户端太旧，没报工作区键。说清怎么修，
	// 而不是让人去猜或反复重输 CDK（CDK 是好的）。
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(reason, "workspace") {
		return fmt.Errorf("服务端拒绝了终端连接：这个 cclaude 版本比节点旧（没有上报工作区键）。\n" +
			"请重新执行一次安装命令升级客户端；CDK 不用换，认证已经通过了")
	}
	if reason != "" {
		return fmt.Errorf("终端握手被拒（HTTP %d）：%s", resp.StatusCode, reason)
	}
	return fmt.Errorf("终端握手被拒（HTTP %d）", resp.StatusCode)
}

// runTerminal附着云端PTY：令牌放Authorization头（无?token=），raw mode双向转发，
// GetSize轮询发resize。断开即返回，由外层循环重连。
// permMode把 -d 翻成协议里的取值。
func permMode(bypass bool) string {
	if bypass {
		return "bypass"
	}
	return ""
}

func runTerminal(ctx context.Context, conn control.ConnectionResponse, wsKey, mode string) error {
	// 工作区键随连接一起报上去：云端据此决定 tmux 会话名与工作目录，
	// 与同步的落盘位置保持一致。不放URL参数——那会进反代日志。
	header := http.Header{
		"Authorization":   {"Bearer " + conn.TerminalToken},
		"X-CCW-Workspace": {wsKey},
		// 报自己的 TERM：`docker exec -it` 会写死 TERM=xterm（只有 8 色），
		// tmux 据此低估外层终端的能力，重绘会留残影。
		// Windows 上通常没有 TERM 这个环境变量，用默认值兜住。
		"X-CCW-Term": {clientTerm()},
	}
	if mode != "" {
		header.Set("X-CCW-Perm-Mode", mode)
	}
	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, conn.TerminalURL, header)
	if err != nil {
		return wsDialError(err, resp)
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

// clientTerm返回本机的 TERM。Windows 通常不设这个变量，而现代 Windows
// 终端（Windows Terminal / PowerShell 7）都支持 256 色与真彩色，
// 所以缺省报 xterm-256color 而不是让服务端退到 docker 写死的 xterm。
func clientTerm() string {
	if t := os.Getenv("TERM"); t != "" && t != "dumb" {
		return t
	}
	return "xterm-256color"
}

// termSize取当前终端尺寸，按 stdout → stderr → stdin 依次试。
//
// **顺序是关键，不是随手写的**：Windows 上 term.GetSize 走
// GetConsoleScreenBufferInfo，那个 API 只接受**屏幕缓冲区**句柄（stdout/stderr），
// 传 stdin 一定失败。原来只问 stdin，于是 Windows 上 resize 帧一次都发不出去，
// PTY 永远停在服务端建会话时的默认尺寸——界面只占窗口左上角一块
// （2026-07-30 真机上就是这样）。Unix 的 TIOCGWINSZ 对三个都有效，
// 所以这个顺序两边都对。
//
// 三个都拿不到就返回 ok=false（管道/重定向，本来就没有尺寸可言）。
func termSize(getSize func(int) (int, int, error), fds ...int) (cols, rows int, ok bool) {
	for _, fd := range fds {
		c, r, err := getSize(fd)
		if err == nil && c > 0 && r > 0 {
			return c, r, true
		}
	}
	return 0, 0, false
}

// sizeFDs是探测顺序。**stdout 必须排第一**（原因见 termSize）。
// 做成变量只为可测——真正会被改回去的是这个顺序，而不是 termSize 本身。
var sizeFDs = func() []int {
	return []int{int(os.Stdout.Fd()), int(os.Stderr.Fd()), int(os.Stdin.Fd())}
}

func currentSize() (cols, rows int, ok bool) {
	return termSize(term.GetSize, sizeFDs()...)
}

func resizeLoop(ctx context.Context, ws *websocket.Conn) {
	var lastR, lastC int
	send := func() bool {
		cols, rows, ok := currentSize()
		if !ok || (rows == lastR && cols == lastC) {
			return true
		}
		lastR, lastC = rows, cols
		msg, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
		return ws.WriteMessage(websocket.TextMessage, msg) == nil
	}

	// **先发一次再进轮询**：等第一个 tick 意味着头 500ms 用的是服务端默认尺寸，
	// 而 Claude 的欢迎界面恰好在附着的瞬间就画完了——晚发就是画错一次再重画。
	if !send() {
		return
	}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}

// runSync：每2秒一轮三方同步。每轮连sync端点→拉服务端清单→与本地基线Diff→
// 执行上传/下载/删除/冲突副本→持久化新基线。令牌放Authorization头（禁URL参数）。
// ctx取消（终端断开/退出）时停止；cleanup模式下同步仍执行下载/删除，只跳过上传。
func runSync(ctx context.Context, root string, c control.Client, cdk, sessionToken string, conn control.ConnectionResponse) {
	idx := syncpkg.LocalIndex{Root: root}
	client := &syncpkg.SyncClient{
		Root:   root,
		Device: deviceName(),
		// 工作区键按本地目录算：换个目录跑就是另一个云端工作区，
		// 两边的文件互不可见（2026-07-29修的那个跨目录污染）。
		WS:     syncpkg.WorkspaceKey(root),
		Notify: func(m string) { fmt.Fprintln(os.Stderr, "[sync]", m) },
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		tr, err := syncpkg.DialSync(ctx, conn.SyncURL, conn.SyncToken)
		if err != nil {
			// 令牌2分钟过期：自行刷新，不再依赖外层循环（终端连着时外层不刷新）。
			nc, cerr := c.Connection(ctx, sessionToken)
			if cerr != nil {
				if st, eerr := exchangeSession(ctx, c, cdk); eerr == nil {
					sessionToken = st
					nc, cerr = c.Connection(ctx, sessionToken)
				}
			}
			if cerr == nil {
				conn = nc
			}
		} else {
			newBase, serr := client.SyncOnce(ctx, tr)
			tr.Close()
			if serr == nil {
				idx.Save(newBase)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// promptLine读一行普通输入（API地址等非机密）。
func promptLine(msg string) (string, error) {
	fmt.Fprint(os.Stderr, msg)
	s, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(s) == "" {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// promptSecret读机密输入（CDK）：term.ReadPassword不回显、不进history。
func promptSecret(msg string) (string, error) {
	fmt.Fprint(os.Stderr, msg)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func deviceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "cclaude"
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
