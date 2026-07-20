package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Event struct {
	SourceEventID                        string
	OccurredAt                           time.Time
	Model                                string
	Input, Output, CacheRead, CacheWrite int64
}

type rawLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// parseLine解析单行。返回(event, isEvent, isBad)：非用量行两者皆false。
func parseLine(line string) (Event, bool, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Event{}, false, false
	}
	var rl rawLine
	if err := json.Unmarshal([]byte(line), &rl); err != nil {
		return Event{}, false, true
	}
	if rl.Type != "assistant" || rl.Message.Usage == nil || rl.RequestID == "" {
		return Event{}, false, false // 非用量行，不算坏行
	}
	ts, err := time.Parse(time.RFC3339Nano, rl.Timestamp)
	if err != nil {
		return Event{}, false, true
	}
	u := rl.Message.Usage
	return Event{
		SourceEventID: rl.RequestID, OccurredAt: ts.UTC(), Model: rl.Message.Model,
		Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
	}, true, false
}

func ParseLines(r io.Reader) ([]Event, int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20) // 长行容忍到8MB
	var out []Event
	bad := 0
	for sc.Scan() {
		if e, ok, isBad := parseLine(sc.Text()); ok {
			out = append(out, e)
		} else if isBad {
			bad++
		}
	}
	return out, bad
}

type Weights struct{ Input, Output, CacheRead, CacheWrite int64 }

func Weighted(e Event, w Weights) int64 {
	return e.Input*w.Input + e.Output*w.Output + e.CacheRead*w.CacheRead + e.CacheWrite*w.CacheWrite
}

type Sink interface {
	// Insert幂等键为(projectID, e.SourceEventID)；同一requestId再次出现时
	// 按GREATEST各字段取最大值更新（Phase 1证据：真实数据多条值相同，等价于取最终记录）。
	Insert(ctx context.Context, projectID string, e Event, weighted int64) error
}

// OffsetStore持久化每文件读取游标（审查§2.7）；生产实现写usage_offsets表。
type OffsetStore interface {
	Load(ctx context.Context, projectID, fileIdentity string) (offset int64, partial string, err error)
	Save(ctx context.Context, projectID, fileIdentity, path string, offset int64, partial string) error
}

type fileCursor struct {
	offset  int64  // committed_offset：最后一个完整行末尾的位置
	partial string // 已读到但未见换行的尾部半行
}

type Collector struct {
	Dir       string
	ProjectID string
	Sink      Sink
	Weights   Weights
	Offsets   OffsetStore // 生产必须注入；为nil时退化为进程内存游标（仅单元测试）
	mem       map[string]fileCursor
	BadLines  int64 // 指标：坏行/超长行/读取错误累计，由worker暴露，不静默丢弃
}

// fileIdentity在Linux上应取dev:inode（处理轮转/重建）；退化实现取路径。
// 生产版可在collector_linux.go以syscall.Stat_t实现，此处默认用路径。
func fileIdentity(path string, fi os.FileInfo) string { return path }

func (c *Collector) load(ctx context.Context, id string) (fileCursor, error) {
	if c.Offsets == nil {
		if c.mem == nil {
			c.mem = map[string]fileCursor{}
		}
		return c.mem[id], nil
	}
	off, partial, err := c.Offsets.Load(ctx, c.ProjectID, id)
	return fileCursor{offset: off, partial: partial}, err
}

func (c *Collector) save(ctx context.Context, id, path string, cur fileCursor) error {
	if c.Offsets == nil {
		c.mem[id] = cur
		return nil
	}
	return c.Offsets.Save(ctx, c.ProjectID, id, path, cur.offset, cur.partial)
}

func (c *Collector) Scan(ctx context.Context) error {
	return filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil // 单文件失败不中断整体采集
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil
		}
		id := fileIdentity(p, fi)
		cur, err := c.load(ctx, id)
		if err != nil {
			return nil // 游标读不到：跳过本轮，不前进
		}
		resume := cur.offset + int64(len(cur.partial))
		if fi.Size() < resume {
			cur = fileCursor{} // 截断/轮转：从头重扫，幂等写入兜底
			resume = 0
		}
		if _, err := f.Seek(resume, io.SeekStart); err != nil {
			return nil
		}
		br := bufio.NewReaderSize(f, 64<<10)
		for {
			chunk, rerr := br.ReadString('\n')
			if rerr == nil {
				line := cur.partial + chunk
				cur.partial = ""
				if e, ok, isBad := parseLine(line); ok {
					if err := c.Sink.Insert(ctx, c.ProjectID, e, Weighted(e, c.Weights)); err != nil {
						return err // Sink失败：游标不保存，下轮重试
					}
				} else if isBad {
					c.BadLines++
				}
				cur.offset += int64(len(line)) // 只在完整行后推进committed_offset
				continue
			}
			if rerr == io.EOF {
				cur.partial += chunk // 半行暂存，下一轮补全
				break
			}
			c.BadLines++ // 读取错误：记指标，游标停在最后完整行
			break
		}
		return c.save(ctx, id, p, cur)
	})
}
