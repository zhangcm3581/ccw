package sshexec

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// 内存SSH server（设计C4要求的单测方式）：不依赖外部sshd，
// 在本进程内起一个真实的SSH服务端，覆盖握手、host key、命令执行与退出码。

type fakeServer struct {
	ln       net.Listener
	signer   ssh.Signer
	password string
	// handler返回stdout/stderr/exitCode；nil时回显命令本身。
	handler func(cmd string) (string, string, int)
	wg      sync.WaitGroup
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, signer: signer, password: "correct-password"}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *fakeServer) addr() (host string, port int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func (s *fakeServer) fingerprint() string { return ssh.FingerprintSHA256(s.signer.PublicKey()) }

func (s *fakeServer) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(c)
	}
}

func (s *fakeServer) handleConn(c net.Conn) {
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == s.password {
				return nil, nil
			}
			return nil, fmt.Errorf("auth failed")
		},
	}
	cfg.AddHostKey(s.signer)
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, creqs)
	}
}

func (s *fakeServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		ssh.Unmarshal(req.Payload, &payload)
		req.Reply(true, nil)

		out, errOut, code := payload.Command, "", 0
		if s.handler != nil {
			out, errOut, code = s.handler(payload.Command)
		}
		io.WriteString(ch, out)
		io.WriteString(ch.Stderr(), errOut)
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}

func dialTest(t *testing.T, s *fakeServer, fp string, auth ssh.AuthMethod) (*Client, error) {
	t.Helper()
	host, port := s.addr()
	return Dial(context.Background(), Target{Host: host, Port: port, User: "ccw", KnownFingerprint: fp},
		[]ssh.AuthMethod{auth}, 5*time.Second)
}

// TOFU：首次连接接受任意host key并回报指纹（供管理员带外核对，§5.2）。
func TestDialTOFURecordsFingerprint(t *testing.T) {
	s := newFakeServer(t)
	c, err := dialTest(t, s, "", ssh.Password(s.password))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Fingerprint != s.fingerprint() {
		t.Errorf("指纹=%s，want %s", c.Fingerprint, s.fingerprint())
	}
	if !strings.HasPrefix(c.Fingerprint, "SHA256:") {
		t.Errorf("应为SHA256形式便于与ssh-keygen -lf比对，got %s", c.Fingerprint)
	}
}

// 指纹匹配则连接；不匹配必须中止（A25）——这是MITM防护，不允许放行。
func TestDialHostKeyPinning(t *testing.T) {
	s := newFakeServer(t)
	c, err := dialTest(t, s, s.fingerprint(), ssh.Password(s.password))
	if err != nil {
		t.Fatalf("指纹匹配应连上: %v", err)
	}
	c.Close()

	_, err = dialTest(t, s, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ssh.Password(s.password))
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("指纹不符必须返回ErrHostKeyChanged，got %v", err)
	}
}

func TestDialAuthFailure(t *testing.T) {
	s := newFakeServer(t)
	_, err := dialTest(t, s, "", ssh.Password("wrong-password"))
	if err == nil {
		t.Fatal("错误密码应失败")
	}
	// 错误信息不得带出密码
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("错误信息泄漏了密码: %v", err)
	}
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	s := newFakeServer(t)
	s.handler = func(cmd string) (string, string, int) {
		if cmd == "fail" {
			return "partial\n", "boom\n", 3
		}
		return "ok: " + cmd + "\n", "", 0
	}
	c, err := dialTest(t, s, "", ssh.Password(s.password))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := c.Run(context.Background(), "docker --version")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "ok: docker --version\n" {
		t.Errorf("结果错误: %+v", res)
	}

	// 非0退出码不是error：precheck靠它判断"是否已满足"
	res, err = c.Run(context.Background(), "fail")
	if err != nil {
		t.Fatalf("非0退出码不应返回error: %v", err)
	}
	if res.ExitCode != 3 || res.Stderr != "boom\n" {
		t.Errorf("退出码/stderr错误: %+v", res)
	}
}

// 输出里的凭据在最靠近数据源处就被脱敏（§5.4）：调用方拿到的即可安全落盘与推流。
func TestRunRedactsOutput(t *testing.T) {
	s := newFakeServer(t)
	secret := "S3cr3t-Value-In-Output"
	s.handler = func(string) (string, string, int) {
		return "POSTGRES_PASSWORD=" + secret + "\n", "password=" + secret + "\n", 0
	}
	c, _ := dialTest(t, s, "", ssh.Password(s.password))
	defer c.Close()

	res, err := c.Run(context.Background(), "cat .env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, secret) || strings.Contains(res.Stderr, secret) {
		t.Errorf("命令输出未脱敏: %+v", res)
	}
	if !strings.Contains(res.Stdout, "[REDACTED]") {
		t.Errorf("应留下脱敏标记: %q", res.Stdout)
	}
}

func TestStreamLinesAreRedacted(t *testing.T) {
	s := newFakeServer(t)
	secret := "AnotherSecretValue123"
	s.handler = func(string) (string, string, int) {
		return "step 1\nCCW_TOKEN_KEY=" + secret + "\nstep 2\n", "warn: password=" + secret + "\n", 7
	}
	c, _ := dialTest(t, s, "", ssh.Password(s.password))
	defer c.Close()

	var mu sync.Mutex
	var lines []string
	code, err := c.Stream(context.Background(), "bootstrap", func(stream, line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, stream+": "+line)
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Errorf("退出码=%d，want 7", code)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, secret) {
		t.Errorf("推流的行未脱敏:\n%s", joined)
	}
	for _, want := range []string{"stdout: step 1", "stdout: step 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("干净的行应原样保留 %q，实际:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "stderr: ") {
		t.Error("stderr应被单独标记")
	}
}

// ctx取消必须能中断长命令：一台卡住的机器不能把巡检goroutine挂死（§11.5）。
func TestRunRespectsContextCancel(t *testing.T) {
	s := newFakeServer(t)
	block := make(chan struct{})
	s.handler = func(string) (string, string, int) {
		<-block // 永不返回，直到测试结束
		return "", "", 0
	}
	t.Cleanup(func() { close(block) })

	c, _ := dialTest(t, s, "", ssh.Password(s.password))
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Run(ctx, "sleep forever"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应返回DeadlineExceeded，got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("超时未及时生效")
	}
}

// 失控输出不得吃光Console内存：超过上限即截断并标注。
func TestRunTruncatesHugeOutput(t *testing.T) {
	s := newFakeServer(t)
	s.handler = func(string) (string, string, int) {
		return strings.Repeat("A", 3<<20), "", 0 // 3 MiB
	}
	c, _ := dialTest(t, s, "", ssh.Password(s.password))
	defer c.Close()

	res, err := c.Run(context.Background(), "cat huge")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) > maxCapture+128 {
		t.Errorf("输出应被截断，实际%d字节", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "已截断") {
		t.Error("截断应有明确标注（no silent caps）")
	}
}

// 源码级守卫：本包的**代码**里绝不允许调用InsecureIgnoreHostKey（§5.2硬规则）。
//
// 用AST而不是文本匹配：注释里提到这个名字是有价值的（说明为什么不能用），
// 文本匹配会把注释也算成违规，逼着后人删掉那句解释。
func TestNoInsecureHostKeyCallback(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0) // 0＝不保留注释
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "InsecureIgnoreHostKey" {
					t.Errorf("%s 调用了InsecureIgnoreHostKey——任何情况下都不允许（设计§5.2）", name)
				}
				return true
			})
		}
	}
}
