package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadMirasimFailoverWindow(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      int
		invalid   bool
	}{
		{"default", "", 0, false}, {"until cancelled", "0", 0, false}, {"override", "180", 180, false},
		{"negative", "-1", 0, true}, {"too large", "601", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_MIRASIM_FAILOVER_WINDOW_SECONDS", tc.env)
			cfg, err := Load()
			if tc.invalid {
				require.ErrorContains(t, err, "mirasim_failover_window_seconds")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Gateway.MirasimFailoverWindowSeconds)
		})
	}
}

func TestLoadMirasimFailoverWindowPersistsAcrossReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(file, []byte("gateway:\n  mirasim_failover_window_seconds: 45\n"), 0600))
	for _, seconds := range []int{45, 45, 120} {
		resetViperWithJWTSecret(t)
		t.Setenv("CONFIG_FILE", file)
		t.Setenv("GATEWAY_MIRASIM_FAILOVER_WINDOW_SECONDS", "")
		if seconds == 120 {
			t.Setenv("GATEWAY_MIRASIM_FAILOVER_WINDOW_SECONDS", strconv.Itoa(seconds))
		}
		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, seconds, cfg.Gateway.MirasimFailoverWindowSeconds)
	}
}

func TestLoadMirasimRecoveryKnobs(t *testing.T) {
	resetViperWithJWTSecret(t)
	for key, value := range map[string]string{
		"GATEWAY_MIRASIM_FAILOVER_ENABLED":       "false",
		"GATEWAY_MIRASIM_BACKOFF_INITIAL_MS":     "1500",
		"GATEWAY_MIRASIM_BACKOFF_MAX_MS":         "9000",
		"GATEWAY_MIRASIM_BACKOFF_JITTER":         "0.25",
		"GATEWAY_MIRASIM_COOLDOWN_BASE_SECONDS":  "45",
		"GATEWAY_MIRASIM_COOLDOWN_DECAY_SECONDS": "900",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.MirasimFailoverEnabled)
	require.Equal(t, 1500, cfg.Gateway.MirasimBackoffInitialMS)
	require.Equal(t, 9000, cfg.Gateway.MirasimBackoffMaxMS)
	require.Equal(t, .25, cfg.Gateway.MirasimBackoffJitter)
	require.Equal(t, 45, cfg.Gateway.MirasimCooldownBaseSeconds)
	require.Equal(t, 900, cfg.Gateway.MirasimCooldownDecaySeconds)
}
