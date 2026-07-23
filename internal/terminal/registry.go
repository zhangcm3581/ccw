package terminal

import "sync"

// ConnRegistry跟踪每个项目的活跃终端连接，供额度主动执行时按项目关闭（审查§9.3）。
// 每个连接注册一个cancel函数（关闭其WebSocket/PTY）。
type ConnRegistry struct {
	mu sync.Mutex
	m  map[string]map[int]func()
	id int
}

func NewConnRegistry() *ConnRegistry {
	return &ConnRegistry{m: map[string]map[int]func(){}}
}

// Add注册一个连接的关闭函数，返回注销句柄。
func (r *ConnRegistry) Add(projectID string, cancel func()) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[projectID] == nil {
		r.m[projectID] = map[int]func(){}
	}
	r.id++
	id := r.id
	r.m[projectID][id] = cancel
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.m[projectID], id)
	}
}

// CloseProject关闭某项目的所有活跃终端连接（超额时调用）。
func (r *ConnRegistry) CloseProject(projectID string) int {
	r.mu.Lock()
	cancels := make([]func(), 0, len(r.m[projectID]))
	for _, c := range r.m[projectID] {
		cancels = append(cancels, c)
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// ActiveProjects返回当前有活跃连接的项目ID（供周期额度检查遍历）。
func (r *ConnRegistry) ActiveProjects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.m))
	for pid, conns := range r.m {
		if len(conns) > 0 {
			out = append(out, pid)
		}
	}
	return out
}
