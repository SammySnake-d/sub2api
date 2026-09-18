package config

// Zero waits for recovery until the client cancels. A positive window is an
// optional operator deadline for admitting further attempts.
const DefaultMirasimFailoverWindowSeconds = 0

func DefaultMirasimRecoveryConfig() GatewayConfig {
	return GatewayConfig{MirasimFailoverEnabled: true, MirasimFailoverWindowSeconds: DefaultMirasimFailoverWindowSeconds,
		MirasimBackoffInitialMS: 1000, MirasimBackoffMaxMS: 8000, MirasimBackoffJitter: 0.5,
		MirasimCooldownBaseSeconds: 30, MirasimCooldownDecaySeconds: 1800}
}
