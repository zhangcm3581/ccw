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
				lastErr = statusError(resp.StatusCode)
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

// statusError把HTTP状态码翻成**能照着做下一步**的中文。
//
// 原先一律是"rejected with status 401"，看到的人无从下手——尤其401，它同时
// 覆盖"卡打错了"与"这张卡已经不存在了"两种情形（服务端刻意不区分：
// CDK认证失败一律invalid_cdk，不泄露存在性）。客户端同样不去猜是哪一种，
// 但可以把**该做什么**说清楚。
func statusError(code int) error {
	switch code {
	case 401, 403:
		// 不提示"再跑某个命令清缓存"：认证失败时cclaude已经自动清掉本地缓存的CDK，
		// 直接再运行一次就会重新让你输入（见cmd/cclaude/main.go）。
		return fmt.Errorf("CDK 未通过认证（HTTP %d）。可能是粘漏了字符，也可能这张卡已经失效——"+
			"在管理后台撤销或轮换过，或者那台节点被重置过（重置会让旧卡全部作废）。\n"+
			"到管理后台的「CDK」页签发一张新的，再运行一次 cclaude 即可重新输入", code)
	case 429:
		return fmt.Errorf("请求过于频繁（HTTP 429），稍等一会儿再试")
	case 404:
		return fmt.Errorf("接口不存在（HTTP 404）：确认 --api 填的是节点的接入域名，" +
			"且该节点已完成部署")
	}
	return fmt.Errorf("控制面拒绝了请求（HTTP %d）", code)
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
		return out, statusError(resp.StatusCode)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
