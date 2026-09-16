package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type pricingRemoteClient struct {
	httpClient *http.Client
}

// pricingRemoteClientError 代理初始化失败时的错误占位客户端
// 所有请求直接返回初始化错误，禁止回退到直连
type pricingRemoteClientError struct {
	err error
}

func (c *pricingRemoteClientError) FetchPricingJSON(_ context.Context, _ string) ([]byte, error) {
	return nil, c.err
}

func (c *pricingRemoteClientError) FetchHashText(_ context.Context, _ string) (string, error) {
	return "", c.err
}

// NewPricingRemoteClient 创建定价数据远程客户端
// proxyURL 为空时直连，支持 http/https/socks5/socks5h 协议
// 代理配置失败时行为由 allowDirectOnProxyError 控制：
//   - false（默认）：返回错误占位客户端，禁止回退到直连
//   - true：回退到直连，并打 warn 留痕（httpclient 侧统一处理）
//
// 客户端构造统一走 httpclient.NewControlPlaneClient：这里和 github_release_service.go
// 以前各自维护一份逐行相同的 GetClient + 兜底逻辑，其中的回退分支一条日志都不打，
// 代理坏了只表现为「又直连超时了」。控制面代理的来源见 config.ControlPlaneConfig。
func NewPricingRemoteClient(proxyURL string, allowDirectOnProxyError bool) service.PricingRemoteClient {
	client, err := httpclient.NewControlPlaneClient(httpclient.ControlPlaneOptions{
		Service:                 "pricing",
		ProxyURL:                proxyURL,
		Timeout:                 30 * time.Second,
		AllowDirectOnProxyError: allowDirectOnProxyError,
	})
	if err != nil {
		return &pricingRemoteClientError{err: err}
	}
	return &pricingRemoteClient{
		httpClient: client,
	}
}

func (c *pricingRemoteClient) FetchPricingJSON(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (c *pricingRemoteClient) FetchHashText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// 哈希文件格式：hash  filename 或者纯 hash
	hash := strings.TrimSpace(string(body))
	parts := strings.Fields(hash)
	if len(parts) > 0 {
		return parts[0], nil
	}
	return hash, nil
}
