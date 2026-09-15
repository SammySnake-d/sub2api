package repository

// The signing decorator must be able to stand down.
//
// mirasim's auth server (/auth/refresh, /auth/referral) authenticates the plain
// access-token bearer, while the relay data plane authenticates a device
// signature whose bearer is a relay-issued ticket. The decorator cannot tell the
// two apart from the URL alone — a future auth base is just another host — so
// the caller marks the request and the decorator delegates it untouched.
//
// Both arms are asserted in one place on purpose: "unsigned when marked" is only
// meaningful next to a positive control proving the same fixture DOES sign when
// the marker is absent.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newMirasimControlPlaneRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://auth.example.invalid/auth/referral", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer test-plain-bearer")
	req.Header.Set("content-type", "application/json")
	return req
}

func mirasimSignatureHeaders(h http.Header) []string {
	var names []string
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-mirasim-") {
			names = append(names, strings.ToLower(name))
		}
	}
	return names
}

func TestMirasimDecoratorSkipsSigningWhenMarkedControlPlane(t *testing.T) {
	next, _, up := newMirasimFixture(t)

	// Positive control: the very same request, unmarked, IS signed and has its
	// bearer replaced. Without this leg, a decorator that silently stopped
	// signing everything would still pass the assertion below.
	if _, err := up.Do(newMirasimControlPlaneRequest(t, context.Background()), "", 1, 1); err != nil {
		t.Fatalf("unmarked request error = %v", err)
	}
	signed, _ := next.last()
	if len(mirasimSignatureHeaders(signed.Header)) == 0 {
		t.Fatalf("positive control failed: an unmarked mirasim request carried no x-mirasim-* headers, so this fixture cannot detect signing at all")
	}
	if got := signed.Header.Get("Authorization"); got == "Bearer test-plain-bearer" {
		t.Fatalf("positive control failed: an unmarked mirasim request kept the caller's bearer %q, so the assertion below is vacuous", got)
	}

	// The marked request must arrive exactly as the caller built it.
	marked := newMirasimControlPlaneRequest(t, service.WithMirasimSigningDisabled(context.Background()))
	if _, err := up.Do(marked, "", 1, 1); err != nil {
		t.Fatalf("marked request error = %v", err)
	}
	forwarded, _ := next.last()
	if names := mirasimSignatureHeaders(forwarded.Header); len(names) != 0 {
		t.Fatalf("marked control-plane request was signed: %v", names)
	}
	if got := forwarded.Header.Get("Authorization"); got != "Bearer test-plain-bearer" {
		t.Fatalf("marked control-plane request bearer = %q, want the caller's own %q", got, "Bearer test-plain-bearer")
	}
	// The decorator deletes x-api-key as part of signing; a delegated request
	// must not have been touched at all.
	if forwarded.Header.Get("x-mirasim-enc") != "" {
		t.Fatalf("marked control-plane request carried a sealed signature")
	}
}

// TestMirasimSigningOptOutDoesNotLeakAcrossRequests: the marker lives on one
// request's context, so the next data-plane request on the same account must be
// signed again.
func TestMirasimSigningOptOutDoesNotLeakAcrossRequests(t *testing.T) {
	next, _, up := newMirasimFixture(t)

	marked := newMirasimControlPlaneRequest(t, service.WithMirasimSigningDisabled(context.Background()))
	if _, err := up.Do(marked, "", 1, 1); err != nil {
		t.Fatalf("marked request error = %v", err)
	}

	body := `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	if _, err := up.Do(newSignedRequest(t, body), "", 1, 1); err != nil {
		t.Fatalf("data-plane request error = %v", err)
	}
	forwarded, _ := next.last()
	// A data-plane request carries its signature SEALED in x-mirasim-enc (only
	// x-mirasim-client travels in the clear); the plaintext x-mirasim-sig shape
	// belongs to the device-session mint, so asserting on it here would pass for
	// the wrong reason.
	if forwarded.Header.Get("x-mirasim-enc") == "" {
		t.Fatalf("the signing opt-out leaked: the following data-plane request went out unsealed (headers %v)",
			mirasimSignatureHeaders(forwarded.Header))
	}
	if got := forwarded.Header.Get("x-mirasim-client"); got != mirasim.ClientVersion {
		t.Fatalf("data-plane request client header = %q, want %q", got, mirasim.ClientVersion)
	}
}
