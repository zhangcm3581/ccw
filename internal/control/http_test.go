package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccw/internal/auth"
	"ccw/internal/project"
	"ccw/internal/quota"
	"ccw/internal/storage"
	"ccw/internal/token"
)

type fixedReader struct{ perProject map[string]int64 }

func (f fixedReader) WindowUsed(_ context.Context, pid string, _ time.Time) (int64, error) {
	return f.perProject[pid], nil
}

func (f fixedReader) PoolUsed(_ context.Context, _ string, _ time.Time) (int64, error) {
	return 0, nil
}

var testKey = make([]byte, 32)

// newTestServer返回的Server与真实部署同构：无进程内会话状态，project一律经getProject查库。
func newTestServer(t *testing.T, fiveHourUsed int64) (*Server, string) {
	t.Helper()
	cdkA, pubA, _ := auth.NewCDK()
	_, secA, _ := auth.SplitCDK(cdkA)
	hashA, _ := auth.HashSecret(secA)
	pa := project.Project{ID: "pa", AccountID: "acc", Slug: "project-a",
		DiskLimit: 1000, FiveHourLimit: 100, SevenDayLimit: 1000}
	resolver := project.NewMemoryResolver(map[string]project.Entry{
		pubA: {SecretHash: hashA, Project: pa},
	})
	getProject := func(_ context.Context, id string) (project.Project, error) {
		if id == pa.ID {
			return pa, nil
		}
		return project.Project{}, project.ErrInvalidCDK
	}
	q := quota.Service{Reader: fixedReader{perProject: map[string]int64{"pa": fiveHourUsed}}}
	s := New(resolver, getProject, testKey, q, storage.NewMemoryIndex(),
		func(_ context.Context, p project.Project) (quota.Limits, error) {
			return quota.Limits{FiveHour: p.FiveHourLimit, SevenDay: p.SevenDayLimit,
				PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}, nil
		}, "wss://ccw.example.com/ws")
	return s, cdkA
}

func exchange(t *testing.T, base, cdk string) string {
	t.Helper()
	resp, err := http.Post(base+"/v1/auth/exchange", "application/json",
		strings.NewReader(`{"cdk":"`+cdk+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("exchange status %d", resp.StatusCode)
	}
	var ex struct {
		SessionToken string `json:"session_token"`
		ProjectID    string `json:"project_id"`
	}
	json.NewDecoder(resp.Body).Decode(&ex)
	if ex.ProjectID != "pa" || ex.SessionToken == "" {
		t.Fatalf("bad exchange payload: %+v", ex)
	}
	return ex.SessionToken
}

func getConnection(t *testing.T, base, sessionToken string) (ConnectionResponse, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/connection", nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var conn ConnectionResponse
	json.NewDecoder(resp.Body).Decode(&conn)
	return conn, resp.StatusCode
}

func TestExchangeAndConnection(t *testing.T) {
	s, cdk := newTestServer(t, 10)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	conn, code := getConnection(t, srv.URL, exchange(t, srv.URL, cdk))
	if code != 200 {
		t.Fatalf("connection status %d", code)
	}
	if conn.TerminalToken == "" || conn.SyncToken == "" || conn.Over {
		t.Fatalf("expected tokens for non-over project: %+v", conn)
	}
	if conn.SyncMode != "rw" {
		t.Fatalf("healthy project must be rw, got %q", conn.SyncMode)
	}
	// 终端令牌audience必须是terminal，不能拿去开同步
	if _, err := token.Verify(testKey, conn.TerminalToken, token.AudSync, time.Now()); err == nil {
		t.Fatal("terminal token must not verify as sync")
	}
	// 连接令牌是2分钟短期令牌
	c, err := token.Verify(testKey, conn.TerminalToken, token.AudTerminal, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(c.ExpiresAt); d > 2*time.Minute+time.Second {
		t.Fatalf("connection token ttl must be 2 minutes, got %v", d)
	}
}

func TestInvalidCDKUniformError(t *testing.T) {
	s, _ := newTestServer(t, 10)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/auth/exchange", "application/json",
		strings.NewReader(`{"cdk":"ccw_wrong.zzz"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "ccw_wrong") {
		t.Fatal("error body must not echo the cdk")
	}
}

func TestOverProjectGetsCleanupSyncOnly(t *testing.T) {
	s, cdk := newTestServer(t, 100) // 5h用量达到上限100
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	conn, _ := getConnection(t, srv.URL, exchange(t, srv.URL, cdk))
	if !conn.Over || conn.OverReason != "five_hour_limit" {
		t.Fatalf("must be over with reason: %+v", conn)
	}
	if conn.TerminalToken != "" {
		t.Fatalf("over project must get no terminal token: %+v", conn)
	}
	// 超额仍必须能清理：sync token照发，但模式为cleanup（审查§2.8/§7）
	if conn.SyncToken == "" || conn.SyncMode != "cleanup" {
		t.Fatalf("over project must still get cleanup-mode sync token: %+v", conn)
	}
}

// 审查§5.4/验收§15.1：control-api无进程内会话状态，
// 重启（全新Server实例）后未过期的session token仍能解析出项目。
func TestSessionSurvivesServerRestart(t *testing.T) {
	s1, cdk := newTestServer(t, 10)
	srv1 := httptest.NewServer(s1.Handler())
	sessionToken := exchange(t, srv1.URL, cdk)
	srv1.Close() // 模拟control-api重启

	s2, _ := newTestServer(t, 10) // 全新实例，无任何继承的内存状态
	srv2 := httptest.NewServer(s2.Handler())
	defer srv2.Close()

	conn, code := getConnection(t, srv2.URL, sessionToken)
	if code != 200 || conn.ProjectID != "pa" {
		t.Fatalf("session must survive restart: code=%d conn=%+v", code, conn)
	}
}

func TestExchangeRateLimited(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.MaxAuthAttempts = 3 // 收紧阈值便于测试
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var last int
	for i := 0; i < 5; i++ {
		resp, err := http.Post(srv.URL+"/v1/auth/exchange", "application/json",
			strings.NewReader(`{"cdk":"ccw_bad.zzz"}`))
		if err != nil {
			t.Fatal(err)
		}
		last = resp.StatusCode
		resp.Body.Close()
	}
	if last != 429 {
		t.Fatalf("repeated failures must be rate limited, got %d", last)
	}
}

// **返回给客户端的限额，必须就是闸门判定用的那个限额。**
//
// 2026-08-03 真机：project-a 挂了 7x 档位（7d 实际限额 5086014），
// 闸门按档位判定为超额，客户端却打印
// `7d:8132044/10000000 项目受限（seven_day_limit）`——
// 10000000 是 projects 表里没折算的绝对值。数字比限额小、却说超额，
// 那句提示等于自己否定自己。
func TestConnectionReportsTheLimitTheGateUsed(t *testing.T) {
	s, cdk := newTestServer(t, 0)
	// LimitsFor 给出与项目原始列**不同**的限额（模拟档位折算）：
	// 原始列是 5h 100 / 7d 1000，折算后是 50 / 500。
	s.LimitsFor = func(_ context.Context, _ project.Project) (quota.Limits, error) {
		return quota.Limits{FiveHour: 50, SevenDay: 500,
			PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	conn, code := getConnection(t, srv.URL, exchange(t, srv.URL, cdk))
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if conn.FiveHourLimit != 50 || conn.SevenDayLimit != 500 {
		t.Errorf("应报告折算后的限额 50/500，got %d/%d（读了原始列就会是 100/1000）",
			conn.FiveHourLimit, conn.SevenDayLimit)
	}
}

// 同一件事的另一面：报告的限额必须与 Over 判定自洽——
// 不能出现 used < limit 却说超额。
func TestConnectionLimitAgreesWithOverFlag(t *testing.T) {
	s, cdk := newTestServer(t, 80) // 5h 已用 80
	s.LimitsFor = func(_ context.Context, _ project.Project) (quota.Limits, error) {
		return quota.Limits{FiveHour: 50, SevenDay: 500, // 80 >= 50，超额
			PoolFiveHour: 1 << 40, PoolSevenDay: 1 << 40}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	conn, _ := getConnection(t, srv.URL, exchange(t, srv.URL, cdk))
	if !conn.Over {
		t.Fatalf("80 >= 50 应判超额: %+v", conn)
	}
	if conn.FiveHourUsed < conn.FiveHourLimit {
		t.Errorf("说了超额，报告的数字却是 %d/%d——客户端会显示一句自相矛盾的提示",
			conn.FiveHourUsed, conn.FiveHourLimit)
	}
}
