package dto

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMirasimPoolControlsRoundTripDTO(t *testing.T) {
	a := &service.Account{ID: 1, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"provider": "mirasim"}, Extra: map[string]any{"base_rpm": 12, "max_sessions": 3, "session_idle_timeout_minutes": 7, "window_cost_limit": 50.0, "enable_tls_fingerprint": true}}
	d := AccountFromServiceShallow(a)
	require.Equal(t, 12, *d.BaseRPM)
	require.Equal(t, 3, *d.MaxSessions)
	require.Equal(t, 7, *d.SessionIdleTimeoutMin)
	require.Equal(t, 50.0, *d.WindowCostLimit)
	require.Equal(t, "strict", *d.RPMStrategy)
	require.Nil(t, d.EnableTLSFingerprint, "must not inherit OAuth identity toggles")
	a.Credentials = map[string]any{}
	d = AccountFromServiceShallow(a)
	require.Nil(t, d.BaseRPM)
	require.Nil(t, d.MaxSessions)
}
