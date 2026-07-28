// Package sshexec是Console到节点的唯一执行通道（console-fleet-design §2.2、C4）。
//
// 为什么只走SSH：备选方案「节点跑一个admin API」需要新端口、新认证体系、新令牌，
// 攻击面净增；而SSH本来就必须开着（否则管理员救不了机器）。SSH-only意味着
// Console被攻破的爆炸半径**等于**管理员SSH私钥泄露的爆炸半径——没有变大。
//
// host key处理是TOFU（§5.2）：首次连接记录指纹供带外核对，之后不匹配即中止。
// **任何情况下都不允许ssh.InsecureIgnoreHostKey()**——那等于把中间人攻击的门敞开。
package sshexec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"ccw/internal/redact"
)

// ErrHostKeyChanged表示服务器指纹与记录不符：可能是重装、也可能是中间人。
// 流水线必须中止并把节点标记为host_key_changed，需管理员显式确认（§5.2、A25）。
var ErrHostKeyChanged = errors.New("sshexec: host key changed")

// Target是一个目标节点的连接信息。
type Target struct {
	Host string
	Port int
	User string
	// KnownFingerprint为空＝TOFU首次连接（接受并回填Fingerprint）；
	// 非空＝必须精确匹配，否则ErrHostKeyChanged。
	KnownFingerprint string
}

func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Host, fmt.Sprint(port))
}

// Client是一条已建立的SSH连接。
type Client struct {
	conn *ssh.Client
	// Fingerprint是本次连接实际看到的host key指纹（SHA256形式）。
	// 首次纳管时由调用方存进nodes.host_key_fp并展示给管理员带外核对。
	Fingerprint string
}

// Dial建立连接。auth由调用方提供（首登用密码、之后用托管密钥）。
//
// 超时是必需的：机队规模上去后，一台失联机器不能把巡检goroutine挂死（§11.5）。
func Dial(ctx context.Context, t Target, auth []ssh.AuthMethod, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	var seen string
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: auth,
		// TOFU：首次接受并记录，之后必须一致。绝不使用InsecureIgnoreHostKey。
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			seen = ssh.FingerprintSHA256(key)
			if t.KnownFingerprint == "" {
				return nil // 首次使用即信任；调用方负责把指纹给管理员核对
			}
			if seen != t.KnownFingerprint {
				return ErrHostKeyChanged
			}
			return nil
		},
		Timeout: timeout,
	}

	d := net.Dialer{Timeout: timeout}
	netConn, err := d.DialContext(ctx, "tcp", t.addr())
	if err != nil {
		return nil, fmt.Errorf("sshexec: dial %s: %w", t.addr(), err)
	}
	// 握手也要受ctx约束：DialContext只覆盖TCP连接阶段。
	if dl, ok := ctx.Deadline(); ok {
		netConn.SetDeadline(dl)
	} else {
		netConn.SetDeadline(time.Now().Add(timeout))
	}
	sc, chans, reqs, err := ssh.NewClientConn(netConn, t.addr(), cfg)
	if err != nil {
		netConn.Close()
		if errors.Is(err, ErrHostKeyChanged) {
			return nil, ErrHostKeyChanged
		}
		// 认证失败的错误信息可能带出用户名与方法，但**绝不会带出密码**；
		// 仍然过一遍脱敏，防止上游把凭据拼进了错误。
		return nil, fmt.Errorf("sshexec: handshake %s: %s", t.addr(), redact.String(err.Error()))
	}
	netConn.SetDeadline(time.Time{}) // 握手完成后取消deadline，长命令不该被它掐断
	return &Client{conn: ssh.NewClient(sc, chans, reqs), Fingerprint: seen}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Result是一次命令执行的结果。
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// maxCapture限制单次命令捕获的输出量：节点上一条失控的命令
// （如cat大文件）不该把Console的内存吃光。超出部分丢弃并在末尾标注。
const maxCapture = 1 << 20 // 1 MiB

// RunStdin执行命令并把input喂给它的stdin。
//
// 用stdin而不是把内容拼进命令行：命令行在节点上对**所有用户**可见（`ps aux`），
// 且长度有上限。推送源码包这类大内容必须走这里。
//
// 输出同样在源头脱敏；退出码非0不算错误（与Run一致）。
func (c *Client) RunStdin(ctx context.Context, cmd string, input io.Reader) (Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("sshexec: new session: %w", err)
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	var out, errOut strings.Builder
	sess.Stdout = &limitedWriter{w: &out, limit: maxCapture}
	sess.Stderr = &limitedWriter{w: &errOut, limit: maxCapture}

	if err := sess.Start(cmd); err != nil {
		stdin.Close()
		return Result{}, fmt.Errorf("sshexec: start: %w", err)
	}
	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, input)
		// 必须关stdin：远端命令（tar/base64）要靠EOF知道输入结束，否则双方互等。
		stdin.Close()
		copyErr <- err
	}()

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		sess.Close()
		return Result{}, ctx.Err()
	case werr := <-done:
		if cerr := <-copyErr; cerr != nil {
			return Result{}, fmt.Errorf("sshexec: 写入stdin失败: %w", cerr)
		}
		res := Result{Stdout: redact.String(out.String()), Stderr: redact.String(errOut.String())}
		var ee *ssh.ExitError
		switch {
		case werr == nil:
			return res, nil
		case errors.As(werr, &ee):
			res.ExitCode = ee.ExitStatus()
			return res, nil
		default:
			return res, fmt.Errorf("sshexec: wait: %w", werr)
		}
	}
}

// Run执行一条命令并捕获输出。
//
// **输出在这里就过脱敏**（§5.4）：调用方拿到的Stdout/Stderr已经是可以安全落盘
// 与推流的内容。凭据可能出现在节点命令的输出里（例如误打印的.env），
// 让脱敏发生在最靠近数据源的地方，比指望每个调用方记得脱敏可靠。
//
// 退出码非0**不算错误**：precheck步骤靠退出码判断"是否已满足"，
// 把它当error会让每个调用点都要拆包。返回error只表示连接/通道层面的失败。
func (c *Client) Run(ctx context.Context, cmd string) (Result, error) {
	res, _, err := c.run(ctx, cmd)
	return res, err
}

// RunCapturingSecret与Run执行相同，但**额外返回未脱敏的stdout原文**。
//
// 为什么需要它：`ccwadmin init-project`把新签发的CDK明文打在stdout上，
// 而Run会在最靠近数据源处把它抹成[REDACTED]——那条规则对日志是对的，
// 但这里的明文有一条合法去处：经内存中转在浏览器上显示一次（设计§8.4）。
// 脱敏发生在拿到值之前，明文就永远到不了管理员手里。
//
// **调用方的义务**：raw只能用于解析，**绝不能进日志、错误信息或数据库**。
// 返回的Result仍是脱敏过的，出错时请用它来拼错误信息。
// 全仓只应有一个调用点（provision的init-projects步骤）。
func (c *Client) RunCapturingSecret(ctx context.Context, cmd string) (res Result, raw string, err error) {
	return c.run(ctx, cmd)
}

func (c *Client) run(ctx context.Context, cmd string) (Result, string, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, "", fmt.Errorf("sshexec: new session: %w", err)
	}
	defer sess.Close()

	var out, errOut strings.Builder
	sess.Stdout = &limitedWriter{w: &out, limit: maxCapture}
	sess.Stderr = &limitedWriter{w: &errOut, limit: maxCapture}

	done := make(chan error, 1)
	if err := sess.Start(cmd); err != nil {
		return Result{}, "", fmt.Errorf("sshexec: start: %w", err)
	}
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		sess.Close()
		return Result{}, "", ctx.Err()
	case werr := <-done:
		raw := out.String()
		res := Result{Stdout: redact.String(raw), Stderr: redact.String(errOut.String())}
		var ee *ssh.ExitError
		switch {
		case werr == nil:
			return res, raw, nil
		case errors.As(werr, &ee):
			res.ExitCode = ee.ExitStatus() // 非0退出码是正常返回值，不是error
			return res, raw, nil
		default:
			return res, raw, fmt.Errorf("sshexec: wait: %w", werr)
		}
	}
}

// Stream执行命令并把输出逐行推给onLine（流水线的实时日志，§5.4）。
//
// 每一行都先经过脱敏再交给回调——回调会同时写盘与经SSE推给浏览器，
// 那是凭据最容易泄漏出去的路径。
func (c *Client) Stream(ctx context.Context, cmd string, onLine func(stream, line string)) (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 0, fmt.Errorf("sshexec: new session: %w", err)
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := sess.Start(cmd); err != nil {
		return 0, fmt.Errorf("sshexec: start: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanLines(stdout, "stdout", onLine) }()
	go func() { defer wg.Done(); scanLines(stderr, "stderr", onLine) }()

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		sess.Close()
		wg.Wait()
		return 0, ctx.Err()
	case werr := <-done:
		wg.Wait()
		var ee *ssh.ExitError
		switch {
		case werr == nil:
			return 0, nil
		case errors.As(werr, &ee):
			return ee.ExitStatus(), nil
		default:
			return 0, fmt.Errorf("sshexec: wait: %w", werr)
		}
	}
}

func scanLines(r io.Reader, stream string, onLine func(string, string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // 容忍长行（构建输出常有）
	for sc.Scan() {
		onLine(stream, redact.String(sc.Text()))
	}
}

// limitedWriter在超过limit后停止累积，只记一次截断标记——
// 避免节点上的失控输出把Console内存吃光。
type limitedWriter struct {
	w         *strings.Builder
	limit     int
	n         int
	truncated bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n < l.limit {
		room := l.limit - l.n
		if len(p) <= room {
			l.w.Write(p)
			l.n += len(p)
		} else {
			l.w.Write(p[:room])
			l.n = l.limit
		}
	}
	if l.n >= l.limit && !l.truncated {
		l.truncated = true
		l.w.WriteString("\n...[输出超过1 MiB，已截断]")
	}
	return len(p), nil // 始终报告全部写入：否则ssh会话会因短写而报错中断
}
