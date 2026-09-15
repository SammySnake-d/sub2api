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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
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

	// [[cov:SG:query-not-signed]] The URL carries "?beta=true" — which sub2api
	// always appends — while mirasim.SignAndSeal is given req.URL.Path only. If
	// the query were inside the signed canonical string or the seal AAD, the
	// upstream would reject this; HTTP 200 is the proof that it is not.
	t.Logf("HTTP %d in %s (request-id=%s)", resp.StatusCode, elapsed.Round(time.Millisecond), resp.Header.Get("request-id"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream returned %d for a request whose URL carried ?beta=true: %s", resp.StatusCode, truncate(string(raw), 600))
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
	// The accepted request declared this client version, and the signature the
	// upstream just verified covers that same value.
	if got := req.Header.Get("x-mirasim-client"); got != mirasim.ClientVersion {
		t.Fatalf("accepted request declared x-mirasim-client %q, want mirasim.ClientVersion %q", got, mirasim.ClientVersion)
	}
	if got := req.URL.RawQuery; got != "beta=true" {
		t.Fatalf("query = %q: the ?beta=true this obligation is about never reached the wire", got)
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

// liveDoer routes mirasim's control-plane calls through the real upstream client.
type liveDoer struct {
	next     service.HTTPUpstream
	proxyURL string
}

func (d *liveDoer) Do(req *http.Request) (*http.Response, error) {
	return d.next.Do(req, d.proxyURL, 1, 4)
}

// TestMirasimLiveRejectsPathMismatch is the negative control for the whole
// signing port: it proves the upstream actually VERIFIES the signature, rather
// than ignoring it and letting anything through. Without this, every positive
// result in this package is consistent with "the server does not check".
//
// It signs for one path and sends to another. Rejection is free — a 4xx burns no
// tokens — so this is the cheapest assertion in the suite.
func TestMirasimLiveRejectsPathMismatch(t *testing.T) {
	if os.Getenv("MIRASIM_LIVE") != "1" {
		t.Skip("set MIRASIM_LIVE=1 (and the MIRASIM_* credential env vars) to run the live negative control")
	}
	seed := os.Getenv("MIRASIM_DEVICE_SEED")
	access := os.Getenv("MIRASIM_ACCESS_TOKEN")
	if seed == "" || access == "" {
		t.Fatal("MIRASIM_DEVICE_SEED and MIRASIM_ACCESS_TOKEN are required")
	}
	relayBase := envOr("MIRASIM_RELAY_BASE", mirasim.DefaultRelayBase)
	proxyURL := os.Getenv("MIRASIM_PROXY_URL")

	base := NewHTTPUpstream(nil)
	ident := mirasim.Identity{
		DeviceSeed:  seed,
		AccessToken: access,
		AuthBase:    envOr("MIRASIM_AUTH_BASE", mirasim.DefaultAuthBase),
		RelayBase:   relayBase,
		SessionID:   os.Getenv("MIRASIM_SESSION_ID"),
	}
	if v := os.Getenv("MIRASIM_EXPIRES_AT"); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			ident.ExpiresAt = ts
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	prepared, err := mirasim.NewRegistry().Prepare(ctx, 1, ident, &liveDoer{next: base, proxyURL: proxyURL}, nil, nil)
	if err != nil {
		t.Fatalf("prepare credential: %v", err)
	}

	// Control: the credential itself must be genuine, otherwise the rejection
	// below would be attributable to a missing/blank bearer rather than to the
	// path. The positive half of the pair is TestMirasimLiveEndToEnd, which sends
	// a correctly-signed path with the same machinery and gets HTTP 200.
	if prepared.Credential == "" || prepared.Signer == nil {
		t.Fatal("mirasim.Registry.Prepare returned no credential/signer; a rejection would prove nothing")
	}

	body := []byte(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(relayBase, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+prepared.Credential)

	const wrongPath = "/v1/deliberately-wrong-path"
	mirasim.ApplyContextHeaders(req.Header, mirasim.ContextInput{
		Path:       wrongPath,
		SessionID:  prepared.SessionID,
		AccountSub: prepared.AccountSub,
		Locale:     prepared.Locale,
	})
	// [[cov:SG:path-signed]] Everything is a valid, correctly-assembled mirasim
	// request EXCEPT that mirasim.SignAndSeal is given wrongPath while the request
	// is sent to /v1/messages. The path is inside both the signed canonical string
	// and the seal AAD, so the upstream must refuse it. A 200 here would mean the
	// signature is decorative and every other result in this package is vacuous.
	if err := mirasim.SignAndSeal(req.Header, prepared.Signer, req.Method, wrongPath, body, prepared.Credential); err != nil {
		t.Fatal(err)
	}

	resp, err := base.DoWithTLS(req, proxyURL, 1, 4, tlsfingerprint.MirasimProfile())
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	t.Logf("path-mismatch -> HTTP %d  %s", resp.StatusCode, truncate(strings.ReplaceAll(string(raw), "\n", " "), 300))
	if resp.StatusCode == http.StatusOK {
		t.Fatal("upstream ACCEPTED a request signed for a different path: the signature is not being verified, so every positive signing result here proves nothing")
	}
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
	default:
		t.Fatalf("expected an authentication-class rejection, got HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
}
