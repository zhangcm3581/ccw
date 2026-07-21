package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ExchangeResult struct {
	SessionToken string `json:"session_token"`
	ProjectID    string `json:"project_id"`
	ProjectSlug  string `json:"project_slug"`
}

type Client struct {
	Base      string
	RetryBase time.Duration // 默认1秒；测试可设极短
}

// doJSON带指数退避：5xx与网络错误重试（上限5次），4xx立即失败。
// 错误信息只含状态码，绝不包含请求体或CDK（防泄漏）。
func (c Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	backoff := c.RetryBase
	if backoff <= 0 {
		backoff = time.Second
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
				if backoff *= 2; backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("control: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("control: request failed (network)")
			continue // 网络错误重试
		}
		retry := false
		func() {
			defer resp.Body.Close()
			switch {
			case resp.StatusCode >= 500:
				lastErr = fmt.Errorf("control: server error %d", resp.StatusCode)
				retry = true
			case resp.StatusCode >= 400:
				lastErr = fmt.Errorf("control: rejected with status %d", resp.StatusCode)
			default:
				lastErr = json.NewDecoder(resp.Body).Decode(out)
			}
		}()
		if !retry {
			return lastErr
		}
	}
	return lastErr
}

func (c Client) Exchange(ctx context.Context, cdk string) (ExchangeResult, error) {
	var out ExchangeResult
	err := c.doJSON(ctx, "POST", "/v1/auth/exchange", map[string]string{"cdk": cdk}, &out)
	return out, err
}

func (c Client) Connection(ctx context.Context, sessionToken string) (ConnectionResponse, error) {
	var out ConnectionResponse
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/v1/connection", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("control: request failed (network)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("control: rejected with status %d", resp.StatusCode)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
