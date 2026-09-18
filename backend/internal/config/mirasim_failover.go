package config

// DefaultMirasimFailoverWindowSeconds permits full eligible-pool sweeps before
// returning a transient upstream failure. Operators can override it via YAML
// or GATEWAY_MIRASIM_FAILOVER_WINDOW_SECONDS; zero disables pool recovery.
const DefaultMirasimFailoverWindowSeconds = 90
