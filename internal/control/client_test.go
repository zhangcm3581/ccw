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
