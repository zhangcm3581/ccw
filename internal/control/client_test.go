package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExchangeNeverLogsCDK(t *testing.T) {
	// 约定：Client所有error字符串不得包含CDK明文
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid_cdk"}`))
	}))
	defer srv.Close()
	c := Client{Base: srv.URL}
	_, err := c.Exchange(context.Background(), "ccw_secret.value123")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "ccw_secret") || strings.Contains(err.Error(), "value123") {
		t.Fatalf("error leaks cdk: %v", err)
	}
}

func TestExchangeRetriesWithBackoff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(502) // 网关暂时不可用→重试
			return
		}
		w.Write([]byte(`{"session_token":"tok","project_id":"pa","project_slug":"project-a"}`))
	}))
	defer srv.Close()
	c := Client{Base: srv.URL, RetryBase: 1} // 极短退避加速测试
	res, err := c.Exchange(context.Background(), "ccw_x.y")
	if err != nil || res.SessionToken != "tok" {
		t.Fatalf("retry failed: %+v %v", res, err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

func TestExchange4xxNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := Client{Base: srv.URL, RetryBase: 1}
	if _, err := c.Exchange(context.Background(), "ccw_bad.x"); err == nil {
		t.Fatal("want error on 401")
	}
	if calls != 1 {
		t.Fatalf("4xx must not be retried, got %d calls", calls)
	}
}

// 401 的提示必须能照着做下一步。
//
// 原先一律是 "rejected with status 401"，看到的人无从下手（2026-07-30 真机上
// 就是这个）。同时守两条：不得暗示 CDK 是否存在（服务端刻意不区分），
// 也不得指向不存在的命令——`--reset` 就不存在，实际是 `cclaude logout`，
// 而认证失败时客户端已自动清缓存，压根不需要额外命令。
func TestStatusErrorIsActionable(t *testing.T) {
	msg := statusError(401).Error()
	for _, want := range []string{"CDK", "管理后台", "签发"} {
		if !strings.Contains(msg, want) {
			t.Errorf("401 提示缺少 %q：%s", want, msg)
		}
	}
	for _, bad := range []string{"--reset", "rejected with status"} {
		if strings.Contains(msg, bad) {
			t.Errorf("401 提示含 %q（不存在的命令/无信息量的原文）：%s", bad, msg)
		}
	}
	// 不得泄露存在性：不能出现"不存在"/"未找到"这类区分
	for _, leak := range []string{"该卡不存在", "未找到该 CDK"} {
		if strings.Contains(msg, leak) {
			t.Errorf("401 提示泄露存在性：%s", msg)
		}
	}
	if !strings.Contains(statusError(429).Error(), "频繁") {
		t.Error("429 应说明是限流")
	}
	if !strings.Contains(statusError(404).Error(), "--api") {
		t.Error("404 应提示检查 --api")
	}
}
