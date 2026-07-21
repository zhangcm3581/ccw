package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"

	"ccw/internal/config"
	"ccw/internal/store"
	"ccw/internal/terminal"
)

// ptySession把宿主机上的docker exec附着进程包成io.ReadWriteCloser。
// Close只结束附着进程，不触碰容器内的tmux会话。
type ptySession struct {
	f   *os.File
	cmd *exec.Cmd
}

func (p *ptySession) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptySession) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *ptySession) Close() error {
	p.f.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill() // 杀的是docker exec附着进程，不是tmux
	}
	return p.cmd.Wait()
}

func (p *ptySession) Resize(rows, cols uint16) error {
	return pty.Setsize(p.f, &pty.Winsize{Rows: rows, Cols: cols})
}

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL) // 内部Ping，失败即非零退出
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker-agent:", err)
		os.Exit(1)
	}

	// 从projectID查项目的container_name（终端附着的目标容器）。
	containerFor := func(projectID string) string {
		if p, err := st.GetProjectByID(ctx, projectID); err == nil && p.ContainerName != "" {
			return p.ContainerName
		}
		return "ccw-" + projectID
	}

	startPTY := func(projectID string) (io.ReadWriteCloser, error) {
		container := containerFor(projectID)
		// 附着前先准备会话：has-session失败才new-session -d（审计§4.1）
		cmds := terminal.EnsureSessionCmds(container, projectID)
		if err := exec.Command(cmds[0][0], cmds[0][1:]...).Run(); err != nil {
			if err := exec.Command(cmds[1][0], cmds[1][1:]...).Run(); err != nil {
				return nil, err
			}
		}
		args := terminal.AttachCmd(container, projectID)
		cmd := exec.Command(args[0], args[1:]...)
		f, err := pty.Start(cmd)
		if err != nil {
			return nil, err
		}
		return &ptySession{f: f, cmd: cmd}, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/terminal", func(w http.ResponseWriter, r *http.Request) {
		terminal.Serve(w, r, cfg.TokenSigningKey, startPTY)
	})

	// 只监听回环/内网（审查§3.1）；公网由反向代理的443统一入口。
	// WebSocket升级后连接被Hijack，读写deadline由terminal.Serve自行维护；
	// 这里设ReadHeaderTimeout防slowloris。
	srv := &http.Server{
		Addr:              cfg.AgentListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Println("worker-agent listening on", cfg.AgentListenAddr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "worker-agent:", err)
		os.Exit(1)
	}
}
