package repository

// Live end-to-end proof: one real request, sub2api's own transport stack ->
// mirasim -> HTTP 200 with real content.
//
// Gated behind MIRASIM_LIVE=1 because it spends real money and needs a real
// account. It exercises the integrated path:
//
//	NewMirasimUpstream(NewHTTPUpstream(cfg)) .DoWithTLS(...)
//	  -> registry: token refresh + device-session mint (through the SAME
//	     upstream client, so the same egress IP)
//	  -> context headers + canonical fingerprint
//	  -> SignAndSeal
//	  -> httpUpstreamService transport
//
// The request is assembled exactly as service.buildUpstreamRequestAnthropicAPIKeyPassthrough
// assembles it, including the "?beta=true" query that the signature must NOT cover.
//
// Secrets come from the environment and are never printed.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestMirasimLiveEndToEnd(t *testing.T) {
	if os.Getenv("MIRASIM_LIVE") != "1" {
		t.Skip("set MIRASIM_LIVE=1 (and the MIRASIM_* credential env vars) to run the live end-to-end proof; it costs real money")
	}
	seed := os.Getenv("MIRASIM_DEVICE_SEED")
	access := os.Getenv("MIRASIM_ACCESS_TOKEN")
	if seed == "" || access == "" {
		t.Fatal("MIRASIM_DEVICE_SEED and MIRASIM_ACCESS_TOKEN are required")
	}
	relayBase := envOr("MIRASIM_RELAY_BASE", mirasim.DefaultRelayBase)
	model := envOr("MIRASIM_MODEL", "claude-haiku-4-5")
	proxyURL := os.Getenv("MIRASIM_PROXY_URL")

	creds := map[string]any{
		mirasim.CredProvider:     mirasim.ProviderMirasim,
		mirasim.CredDeviceSeed:   seed,
		mirasim.CredAccessToken:  access,
		mirasim.CredRefreshToken: os.Getenv("MIRASIM_REFRESH_TOKEN"),
		mirasim.CredAuthBase:     envOr("MIRASIM_AUTH_BASE", mirasim.DefaultAuthBase),
		"base_url":               relayBase,
		"api_key":                "mirasim-signed",
	}
	if v := os.Getenv("MIRASIM_EXPIRES_AT"); v != "" {
		creds[mirasim.CredExpiresAt] = v
	}

	store := &fakeAccountStore{accounts: map[int64]*service.Account{
		1: {ID: 1, Platform: domain.PlatformAnthropic, Type: domain.AccountTypeAPIKey, Credentials: creds, Extra: extraForLive()},
	}}
	up := NewMirasimUpstream(NewHTTPUpstream(nil), store)

	// The upstream gates /v1/messages on the request looking like real Claude
	// Code: without the canonical first system block it answers 400
	// "the request was rejected as invalid", and with a near-miss block it
	// answers 403 "the request was rejected" (both verified against ma-relay's
	// own signing code, so neither is a signature problem). sub2api's own
	// claudeCodeSystemPrompt is the same string.
	const claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
	reqBody := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"stream":     false,
		"system":     []any{map[string]any{"type": "text", "text": claudeCodeSystemPrompt}},
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with the single word: pong"}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(relayBase, "/")+"/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "mirasim-signed") // must be removed by the decorator

	start := time.Now()
	resp, err := up.DoWithTLS(req, proxyURL, 1, 4, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("transport error after %s: %v", elapsed.Round(time.Millisecond), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("HTTP %d in %s (request-id=%s)", resp.StatusCode, elapsed.Round(time.Millisecond), resp.Header.Get("request-id"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream returned %d: %s", resp.StatusCode, truncate(string(raw), 600))
	}

	var parsed struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode response: %v\n%s", err, truncate(string(raw), 600))
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	t.Logf("model=%s stop=%s content=%q usage: input=%d output=%d cache_read=%d cache_creation=%d",
		parsed.Model, parsed.StopReason, text,
		parsed.Usage.InputTokens, parsed.Usage.OutputTokens,
		parsed.Usage.CacheReadInputTokens, parsed.Usage.CacheCreationInputTokens)

	if strings.TrimSpace(text) == "" {
		t.Fatal("HTTP 200 but no text content came back")
	}
	if parsed.Usage.InputTokens == 0 && parsed.Usage.OutputTokens == 0 {
		t.Fatal("HTTP 200 but usage is all zero")
	}
}

func extraForLive() map[string]any {
	e := map[string]any{}
	if v := strings.TrimSpace(os.Getenv("MIRASIM_SESSION_ID")); v != "" {
		e[mirasim.ExtraSessionID] = v
	}
	return e
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
