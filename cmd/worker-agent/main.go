package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	stdsync "sync"
	"time"

	"github.com/creack/pty"

	"ccw/internal/config"
	"ccw/internal/quota"
	"ccw/internal/storage"
	"ccw/internal/store"
	syncpkg "ccw/internal/sync"
	"ccw/internal/terminal"
	"ccw/internal/token"
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

func projectSlug(ctx context.Context, st *store.Store, projectID string) string {
	if p, err := st.GetProjectByID(ctx, projectID); err == nil && p.Slug != "" {
		return p.Slug
	}
	return projectID
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

	// 每个项目一把锁，串行化其同步写（防并发上传 TOCTOU，审查§15.2）。
	var projectLocks stdsync.Map
	lockFor := func(pid string) *stdsync.Mutex {
		m, _ := projectLocks.LoadOrStore(pid, &stdsync.Mutex{})
		return m.(*stdsync.Mutex)
	}
	// 同步会话工厂：绑定 PG 存储、项目 workspace 目录、磁盘配额门、项目锁。
	sessionFactory := func(projectID, device, mode string) *syncpkg.SyncSession {
		root := filepath.Join(cfg.WorkspaceRoot, projectSlug(ctx, st, projectID))
		os.MkdirAll(root, 0o755)
		var limit int64 = 1 << 40
		if p, err := st.GetProjectByID(ctx, projectID); err == nil && p.DiskLimit > 0 {
			limit = p.DiskLimit
		}
		gate := storage.Gate{Limit: limit}
		return &syncpkg.SyncSession{
			ProjectID: projectID, Device: device, Mode: mode,
			Store: st, Dir: syncpkg.NewDirStore(root),
			MaxBytes: 1 << 30, AllowQuota: gate.Allow, Lock: lockFor(projectID),
		}
	}
	// 同步模式：TODO(12-4) 查项目额度决定 cleanup；当前默认 rw。
	modeFor := func(projectID string) string { return "rw" }

	mux := http.NewServeMux()
	registry := terminal.NewConnRegistry()
	mux.HandleFunc("GET /v1/terminal", func(w http.ResponseWriter, r *http.Request) {
		claims, verr := token.Verify(cfg.TokenSigningKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), token.AudTerminal, time.Now())
		if verr != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		unreg := registry.Add(claims.ProjectID, cancel)
		defer unreg()
		defer cancel()
		terminal.Serve(cctx, w, r, cfg.TokenSigningKey, startPTY)
	})
	mux.HandleFunc("GET /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		syncpkg.ServeSync(w, r, cfg.TokenSigningKey, (1<<30)+(64<<10), modeFor, sessionFactory)
	})

	// 只监听回环/内网（审查§3.1）；公网由反向代理的443统一入口。
	// WebSocket升级后连接被Hijack，读写deadline由terminal.Serve自行维护；
	// 这里设ReadHeaderTimeout防slowloris。
	// 额度主动执行（审计§9.3）：每30秒检查活跃项目，超额即关闭其全部终端连接。
	quotaSvc := quota.Service{Reader: st}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, pid := range registry.ActiveProjects() {
					p, perr := st.GetProjectByID(ctx, pid)
					if perr != nil {
						continue
					}
					lim := quota.Limits{FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit, PoolFiveHour: 1 << 62, PoolSevenDay: 1 << 62}
					if d, qerr := quotaSvc.Check(ctx, pid, p.AccountID, lim, time.Now()); qerr == nil && d.Over {
						registry.CloseProject(pid)
					}
				}
			}
		}
	}()

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
