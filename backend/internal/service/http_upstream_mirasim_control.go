package service

import "context"

// Opt-out marker for the mirasim signing decorator.
//
// repository.mirasimUpstream signs every outbound request that belongs to a
// mirasim account and, crucially, REPLACES the Authorization header with the
// relay-issued device ticket. That is correct for the relay data plane and for
// /v1/limits, and wrong for the auth server's control plane (/auth/refresh,
// /auth/referral), which authenticates the plain access-token bearer.
//
// The decorator's own control-plane calls sidestep this by talking to the
// wrapped upstream directly (repository.mirasimDoer). Callers in the service
// layer only hold the decorated upstream, so they need an explicit way to say
// "this one is control plane": they mark the request context, and the decorator
// delegates it byte-for-byte instead of signing it.
//
// The marker is deliberately narrow — it suppresses SIGNING only. The caller
// still chooses the proxy, the account id and the TLS profile it passes to
// DoWithTLS, so the request keeps the account's egress IP and Mirasim.app TLS
// fingerprint.
type mirasimSigningDisabledContextKey struct{}

// WithMirasimSigningDisabled marks ctx as a mirasim control-plane request that
// must go out with the credentials its caller set, unsigned.
func WithMirasimSigningDisabled(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, mirasimSigningDisabledContextKey{}, true)
}

// MirasimSigningDisabled reports whether ctx carries the opt-out marker.
func MirasimSigningDisabled(ctx context.Context) bool {
	return ctx != nil && ctx.Value(mirasimSigningDisabledContextKey{}) == true
}
