package usage

import (
	"context"
	"os"
	"testing"
)

func TestParseLines(t *testing.T) {
	f, err := os.Open("testdata/session-sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	events, bad := ParseLines(f)
	if len(events) != 4 { // req_S1 + req_S2×2 + req_S3；去重是Sink的职责
		t.Fatalf("want 4 events, got %d", len(events))
	}
	if bad != 1 {
		t.Fatalf("want 1 bad line, got %d", bad)
	}
	e := events[0]
	if e.SourceEventID != "req_S1" || e.Input != 5347 || e.Output != 553 ||
		e.CacheRead != 18891 || e.CacheWrite != 3633 || e.Model != "claude-fable-5" {
		t.Fatalf("first event wrong: %+v", e)
	}
}

func TestWeighted(t *testing.T) {
	e := Event{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 20}
	w := Weights{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12}
	if got := Weighted(e, w); got != 10*10+5*50+100*1+20*12 {
		t.Fatalf("weighted wrong: %d", got)
	}
}

type memSink struct{ ids map[string]bool }

func (m *memSink) Insert(_ context.Context, _ string, e Event, _ int64) error {
	if m.ids == nil {
		m.ids = map[string]bool{}
	}
	m.ids[e.SourceEventID] = true
	return nil
}

func TestCollectorIncrementalScan(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/projects/x", 0o755)
	line := `{"type":"assistant","requestId":"req_C","timestamp":"2026-07-19T09:00:00.000Z","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	path := dir + "/projects/x/s.jsonl"
	os.WriteFile(path, []byte(line), 0o644)
	sink := &memSink{}
	c := &Collector{Dir: dir, ProjectID: "pa", Sink: sink}
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sink.ids["req_C"] {
		t.Fatal("req_C not collected")
	}
	// 追加一行后再扫，只处理增量（通过偏移量），且新事件被采集
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"assistant","requestId":"req_D","timestamp":"2026-07-19T09:01:00.000Z","message":{"model":"m","usage":{"input_tokens":2,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n")
	f.Close()
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sink.ids["req_D"] {
		t.Fatal("incremental event not collected")
	}
}

func TestPartialLineNotLostAcrossScans(t *testing.T) {
	// 审查§2.7.3：末尾半行不解析、不丢失，补全后被准确采集
	dir := t.TempDir()
	os.MkdirAll(dir+"/projects/x", 0o755)
	path := dir + "/projects/x/s.jsonl"
	full := `{"type":"assistant","requestId":"req_E","timestamp":"2026-07-19T09:02:00.000Z","message":{"model":"m","usage":{"input_tokens":3,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	os.WriteFile(path, []byte(full[:60]), 0o644) // 只写前60字节，无换行
	sink := &memSink{}
	c := &Collector{Dir: dir, ProjectID: "pa", Sink: sink}
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.ids["req_E"] {
		t.Fatal("half line must not be parsed yet")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(full[60:])
	f.Close()
	if err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sink.ids["req_E"] {
		t.Fatal("completed line must be collected exactly once")
	}
}
