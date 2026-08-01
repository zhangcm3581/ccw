package main

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	syncpkg "ccw/internal/sync"
)

type fakeCloud struct {
	list   []syncpkg.WorkspaceInfo
	purged []string
	failOn string
}

func (f *fakeCloud) Workspaces() ([]syncpkg.WorkspaceInfo, error) { return f.list, nil }
func (f *fakeCloud) Purge(ws string) (int64, error) {
	if ws == f.failOn {
		return 0, errors.New("boom")
	}
	f.purged = append(f.purged, ws)
	for _, w := range f.list {
		if w.WS == ws {
			return w.Bytes, nil
		}
	}
	return 0, nil
}
func (f *fakeCloud) Close() error { return nil }

func cloudIO(keys string) (pickerIO, *bytes.Buffer) {
	var out bytes.Buffer
	return pickerIO{
		in:      strings.NewReader(keys),
		out:     &out,
		isTTY:   true,
		makeRaw: func() (func(), error) { return func() {}, nil },
	}, &out
}

func sample() *fakeCloud {
	return &fakeCloud{list: []syncpkg.WorkspaceInfo{
		{WS: "test-1a2b3c4d", Bytes: 539, Files: 3},
		{WS: "code-9f8e7d6c", Bytes: 252, Files: 2},
		{WS: "old-5544aabb", Bytes: 0},
	}}
}

// 空格标记、Enter 删除所选。**没标记时 Enter 什么都不该删**——
// 这一屏是破坏性的，回车不该有隐含的默认动作。
func TestCloudDeletesOnlyMarked(t *testing.T) {
	f := sample()
	io_, out := cloudIO(" " + kDown + kDown + " " + kEnt)
	if err := runCloudManager(io_, f, "code-9f8e7d6c", 1013, 16106127360); err != nil {
		t.Fatal(err)
	}
	if len(f.purged) != 2 {
		t.Fatalf("应删2个，got %v", f.purged)
	}
	if f.purged[0] != "test-1a2b3c4d" || f.purged[1] != "old-5544aabb" {
		t.Errorf("删错了对象：%v", f.purged)
	}
	if !strings.Contains(out.String(), "本地文件没有变化") {
		t.Error("必须写明本地文件不受影响，否则「删除」看着像要删代码")
	}
}

func TestCloudEnterWithoutMarksDeletesNothing(t *testing.T) {
	f := sample()
	io_, _ := cloudIO(kEnt + kEnt + "q")
	if err := runCloudManager(io_, f, "", 0, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.purged) != 0 {
		t.Errorf("没标记就回车不该删任何东西，got %v", f.purged)
	}
}

func TestCloudQuitDeletesNothing(t *testing.T) {
	for _, k := range []string{"q", "\x1b"} {
		f := sample()
		io_, _ := cloudIO(" " + k) // 标记了但直接退出
		if err := runCloudManager(io_, f, "", 0, 1); err != nil {
			t.Fatal(err)
		}
		if len(f.purged) != 0 {
			t.Errorf("按 %q 退出不该删任何东西，got %v", k, f.purged)
		}
	}
}

// 单个失败不能吞掉其余：要如实报告哪些没删掉。
func TestCloudReportsPartialFailure(t *testing.T) {
	f := sample()
	f.failOn = "test-1a2b3c4d"
	io_, out := cloudIO(" " + kDown + kDown + " " + kEnt)
	if err := runCloudManager(io_, f, "", 0, 1); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "删除失败") || !strings.Contains(s, "test-1a2b3c4d") {
		t.Errorf("应指出失败的那个：%s", s)
	}
	if !strings.Contains(s, "已删除 1 个") {
		t.Errorf("成功数应只算真删掉的：%s", s)
	}
}

// 当前正在用的副本要标出来——手一抖删掉正在用的而不自知是最糟的。
func TestCloudMarksCurrentWorkspace(t *testing.T) {
	f := sample()
	io_, out := cloudIO("q")
	runCloudManager(io_, f, "code-9f8e7d6c", 0, 1)
	if !strings.Contains(out.String(), "（当前）") {
		t.Error("当前副本应有标记")
	}
}

// 配额行按截图的口径显示。
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0B", 539: "539B", 1013: "1013B",
		1024: "1.0KB", 252 * 1024: "252KB",
		16106127360: "15.0GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

var _ = bufio.NewReader
