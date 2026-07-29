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

	// 用量采集是worker-agent独有的职责，配置缺失即拒绝启动：
	// 带着空UsageRoot或零权重跑起来，采集器看上去一切正常但usage_events恒为空，
	// 与接线前的现象无法区分。
	if err := cfg.RequireUsage(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 统一的日志出口：凭据与JSONL内容绝不进日志（CLAUDE.md）。
	logln := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "worker-agent: "+format+"\n", a...)
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

	startPTY := func(projectID, ws string) (io.ReadWriteCloser, error) {
		container := containerFor(projectID)
		// 附着前先准备会话：has-session失败才建目录并new-session -d（审计§4.1）。
		// 会话名与工作目录都跟着工作区走，与同步的落盘位置保持一致。
		cmds := terminal.EnsureSessionCmds(container, projectID, ws)
		if err := exec.Command(cmds[0][0], cmds[0][1:]...).Run(); err != nil {
			for _, c := range cmds[1:] {
				if err := exec.Command(c[0], c[1:]...).Run(); err != nil {
					return nil, err
				}
			}
		}
		args := terminal.AttachCmd(container, projectID, ws)
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
		// Root是项目根；工作区子目录由SyncSession.SetWorkspace在hello时建，
		// 因为工作区键要等客户端报上来才知道。
		root := filepath.Join(cfg.WorkspaceRoot, projectSlug(ctx, st, projectID))
		os.MkdirAll(root, 0o755)
		var limit int64 = 1 << 40
		if p, err := st.GetProjectByID(ctx, projectID); err == nil && p.DiskLimit > 0 {
			limit = p.DiskLimit
		}
		gate := storage.Gate{Limit: limit}
		return &syncpkg.SyncSession{
			ProjectID: projectID, Device: device, Mode: mode,
			Store: st, Root: root,
			MaxBytes: 1 << 30, AllowQuota: gate.Allow, Lock: lockFor(projectID),
		}
	}
	// 同步模式：超额降级为 cleanup（只许下载、删除、缩小）。
	// 每次接受连接都实时查库，不信任令牌里的模式——连接令牌2分钟有效且允许重连，
	// 只看令牌会让刚超额的项目在窗口内继续上传。查询失败按超额处理（fail closed）。
	quotaSvc := quota.Service{Reader: st}
	modeFor := func(projectID string) string {
		return syncModeFor(ctx, st, quotaSvc, projectID, cfg.PoolMargins, time.Now(), logln)
	}

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
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// guard：panic不分goroutine，这里不recover的话一次意外会连采集一起带走。
				guard("额度执行", logln, func() {
					// 只遍历有活跃连接的项目——没有连接就没有东西可关。
					// （采集必须遍历全部项目，那是另一个循环，见下方runUsageLoop。）
					for _, pid := range registry.ActiveProjects() {
						d, qerr := checkProject(ctx, st, quotaSvc, pid, cfg.PoolMargins, time.Now())
						if qerr != nil {
							// 查不到额度就不敢断言"没超"，但也不能凭空关掉正在用的终端：
							// 记日志让人看得见，实际拦截交给同步侧的fail-closed降级。
							logln("额度执行：项目%s额度查询失败，本轮不处理：%v", pid, qerr)
							continue
						}
						if d.Over {
							registry.CloseProject(pid)
						}
					}
				})
			}
		}
	}()

	// 用量采集（解P0-1）：每30秒扫一遍全部项目的会话JSONL并写usage_events。
	// 与上面的执行循环分成两个goroutine——采集遍历全部项目、执行只遍历有连接的项目，
	// 且采集失败不应影响"超额就关终端"这条链路。
	collectors := newUsageCollectors(cfg.UsageRoot, cfg.UsageWeights, st, st)
	go runUsageLoop(ctx, st, collectors, 30*time.Second, logln)

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
