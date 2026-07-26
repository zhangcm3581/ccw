package console

import (
	"os"
	"path/filepath"
	stdsync "sync"
	"time"

	"ccw/internal/redact"
)

// 流水线实时日志的落盘与广播（console-fleet-design §5.4）。
//
// 每行同时做两件事：追加到 <logDir>/<run_id>.log、广播给订阅中的浏览器（SSE）。
// **两条路径都先经redact**——这是凭据最容易泄漏出去的地方（写盘会长期留存、
// 推流会出现在任何打开页面的人眼前）。sshexec已在源头脱敏，这里是第二道。

type logLine struct {
	At   time.Time
	Text string
}

// LogHub管理各run的日志缓冲与订阅者。
type LogHub struct {
	Dir string // 日志目录；为空则只在内存里保留

	mu      stdsync.Mutex
	buffers map[string][]logLine     // run_id → 已产生的行（供新订阅者补齐历史）
	subs    map[string][]chan string // run_id → 订阅者
	done    map[string]bool          // run_id → 是否已结束
	order   []string                 // run_id的出现顺序，用于淘汰最旧的缓冲
}

func NewLogHub(dir string) *LogHub {
	return &LogHub{
		Dir:     dir,
		buffers: map[string][]logLine{},
		subs:    map[string][]chan string{},
		done:    map[string]bool{},
	}
}

// maxRetainedRuns限制内存里保留缓冲的run数量：Console是长驻进程，
// 每次部署都留2000行的话，跑够多次就会稳定占住内存。
// 被淘汰的run仍能从磁盘日志看到全文（LogDir），只是页面不再回放历史。
const maxRetainedRuns = 50

// evictLocked在runID首次出现时登记，并淘汰超出上限的最旧run。调用方须持锁。
func (h *LogHub) evictLocked(runID string) {
	if _, seen := h.buffers[runID]; seen {
		return
	}
	h.order = append(h.order, runID)
	for len(h.order) > maxRetainedRuns {
		oldest := h.order[0]
		h.order = h.order[1:]
		// 仍有订阅者的run不淘汰（正在被人看着）
		if len(h.subs[oldest]) > 0 {
			h.order = append(h.order, oldest)
			break
		}
		delete(h.buffers, oldest)
		delete(h.done, oldest)
	}
}

// maxBufferedLines限制单次运行在内存里保留的行数：一次失控的构建输出
// 不该把Console内存吃光。超出后丢弃最旧的（磁盘上仍是完整的）。
const maxBufferedLines = 2000

// Append记录一行并广播。text在这里再过一次脱敏。
func (h *LogHub) Append(runID, text string) {
	text = redact.String(text)
	h.mu.Lock()
	h.evictLocked(runID)
	buf := append(h.buffers[runID], logLine{At: time.Now(), Text: text})
	if len(buf) > maxBufferedLines {
		buf = buf[len(buf)-maxBufferedLines:]
	}
	h.buffers[runID] = buf
	subs := append([]chan string(nil), h.subs[runID]...)
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- text:
		default:
			// 订阅者跟不上（浏览器卡住/网络慢）：丢这一行而不是阻塞整条流水线。
			// 磁盘上的日志是完整的，刷新页面即可看到全部。
		}
	}
	h.writeFile(runID, text)
}

func (h *LogHub) writeFile(runID, text string) {
	if h.Dir == "" {
		return
	}
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(h.Dir, runID+".log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + text + "\n")
}

// History返回该run已产生的行（只读，不建立订阅）。
//
// 运行详情页用它而不是"Subscribe后立刻cancel"——后者除了浪费一个通道，
// 还会在每次页面渲染时打开一次「发送方持有已注销通道」的窗口。
func (h *LogHub) History(runID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.buffers[runID]))
	for _, l := range h.buffers[runID] {
		out = append(out, l.Text)
	}
	return out
}

// Subscribe返回一个通道与历史行。调用方必须在结束时调用返回的取消函数。
//
// **cancel只注销、不close(ch)。**曾经close过，结果是：Append在锁外向已复制的
// 通道列表发送，与cancel的close竞争，触发`panic: send on closed channel`——
// 触发路径很日常（部署过程中关掉日志页）。panic会被编排层的recover接住，
// 但代价是正在进行的部署被中止、节点被标记degraded。
// 现在通道不关闭，注销后没有新的发送方，它随订阅者一起被GC回收；
// 订阅者靠自己的ctx结束循环，靠doneMarker知道运行已完结。
func (h *LogHub) Subscribe(runID string) (history []string, ch chan string, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.buffers[runID] {
		history = append(history, l.Text)
	}
	ch = make(chan string, 256)
	h.subs[runID] = append(h.subs[runID], ch)
	var once stdsync.Once
	cancel = func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			for i, c := range h.subs[runID] {
				if c == ch {
					h.subs[runID] = append(h.subs[runID][:i], h.subs[runID][i+1:]...)
					break
				}
			}
			if len(h.subs[runID]) == 0 {
				delete(h.subs, runID)
			}
		})
	}
	return history, ch, cancel
}

// Finish标记运行结束：SSE据此关闭连接，浏览器不再重连。
func (h *LogHub) Finish(runID string) {
	h.mu.Lock()
	h.done[runID] = true
	subs := append([]chan string(nil), h.subs[runID]...)
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- doneMarker:
		default:
		}
	}
}

// IsDone报告运行是否已结束（订阅时用于决定是否立即收尾）。
func (h *LogHub) IsDone(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.done[runID]
}

// doneMarker是流内的结束信号；SSE handler见到它就关闭连接。
const doneMarker = "\x00done"
