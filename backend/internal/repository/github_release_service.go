package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type githubReleaseClient struct {
	httpClient         *http.Client
	downloadHTTPClient *http.Client
	updateGitHubToken  string
}

type githubReleaseClientError struct {
	err error
}

// NewGitHubReleaseClient 创建 GitHub Release 客户端
// proxyURL 为空时直连 GitHub，支持 http/https/socks5/socks5h 协议
// 代理配置失败时行为由 allowDirectOnProxyError 控制：
//   - false（默认）：返回错误占位客户端，禁止回退到直连
//   - true：回退到直连，并打 warn 留痕（httpclient 侧统一处理）
//
// 客户端构造统一走 httpclient.NewControlPlaneClient，与定价同步共用同一个落点；
// 控制面代理为什么需要存在见 config.ControlPlaneConfig（国内 VPS 直连 api.github.com
// 会 i/o timeout，导致 Codex 版本跟随静默停更）。
func NewGitHubReleaseClient(proxyURL string, allowDirectOnProxyError bool) service.GitHubReleaseClient {
	apiClient, err := httpclient.NewControlPlaneClient(httpclient.ControlPlaneOptions{
		Service:                 "github_release",
		ProxyURL:                proxyURL,
		Timeout:                 30 * time.Second,
		AllowDirectOnProxyError: allowDirectOnProxyError,
	})
	if err != nil {
		return &githubReleaseClientError{err: err}
	}
	apiClient = cloneHTTPClient(apiClient)
	apiClient.CheckRedirect = githubAPICheckRedirect(apiClient.CheckRedirect)

	// 下载客户端需要更长的超时时间
	downloadClient, err := httpclient.NewControlPlaneClient(httpclient.ControlPlaneOptions{
		Service:                 "github_release_download",
		ProxyURL:                proxyURL,
		Timeout:                 10 * time.Minute,
		AllowDirectOnProxyError: allowDirectOnProxyError,
	})
	if err != nil {
		return &githubReleaseClientError{err: err}
	}
	downloadClient = cloneHTTPClient(downloadClient)

	return &githubReleaseClient{
		httpClient:         apiClient,
		downloadHTTPClient: downloadClient,
		updateGitHubToken:  os.Getenv("UPDATE_GITHUB_TOKEN"),
	}
}

func cloneHTTPClient(client *http.Client) *http.Client {
	cloned := *client
	return &cloned
}

func isGitHubAPIURL(url *url.URL) bool {
	return url != nil && strings.EqualFold(url.Scheme, "https") && url.User == nil &&
		strings.EqualFold(url.Host, "api.github.com")
}

func githubAPICheckRedirect(previous func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !isGitHubAPIURL(req.URL) {
			req.Header.Del("Authorization")
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
}

func (c *githubReleaseClient) newAPIRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Sub2API-Updater")
	if c.updateGitHubToken != "" && isGitHubAPIURL(req.URL) {
		req.Header.Set("Authorization", "Bearer "+c.updateGitHubToken)
	}
	return req, nil
}

func (c *githubReleaseClientError) FetchLatestRelease(ctx context.Context, repo string) (*service.GitHubRelease, error) {
	return nil, c.err
}

func (c *githubReleaseClientError) FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*service.GitHubRelease, error) {
	return nil, c.err
}

func (c *githubReleaseClientError) DownloadFile(ctx context.Context, url, dest string, maxSize int64) error {
	return c.err
}

func (c *githubReleaseClientError) FetchChecksumFile(ctx context.Context, url string) ([]byte, error) {
	return nil, c.err
}

func (c *githubReleaseClient) FetchLatestRelease(ctx context.Context, repo string) (*service.GitHubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	req, err := c.newAPIRequest(ctx, url)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release service.GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	return &release, nil
}

func (c *githubReleaseClient) FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*service.GitHubRelease, error) {
	if perPage <= 0 {
		perPage = 10
	}
	if perPage > 100 {
		perPage = 100 // GitHub API hard limit
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", repo, perPage)

	req, err := c.newAPIRequest(ctx, url)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []*service.GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}

	return releases, nil
}

func (c *githubReleaseClient) DownloadFile(ctx context.Context, url, dest string, maxSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	// 使用预配置的下载客户端（已包含代理配置）
	resp, err := c.downloadHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	// SECURITY: Check Content-Length if available
	if resp.ContentLength > maxSize {
		return fmt.Errorf("file too large: %d bytes (max %d)", resp.ContentLength, maxSize)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}

	// SECURITY: Use LimitReader to enforce max download size even if Content-Length is missing/wrong
	limited := io.LimitReader(resp.Body, maxSize+1)
	written, err := io.Copy(out, limited)

	// Close file before attempting to remove (required on Windows)
	_ = out.Close()

	if err != nil {
		_ = os.Remove(dest) // Clean up partial file (best-effort)
		return err
	}

	// Check if we hit the limit (downloaded more than maxSize)
	if written > maxSize {
		_ = os.Remove(dest) // Clean up partial file (best-effort)
		return fmt.Errorf("download exceeded maximum size of %d bytes", maxSize)
	}

	return nil
}

func (c *githubReleaseClient) FetchChecksumFile(ctx context.Context, url string) ([]byte, error) {
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
