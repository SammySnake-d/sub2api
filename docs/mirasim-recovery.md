# Mirasim recovery controls

Capacity errors remain separate from account quota/auth failures. Normal selection skips persisted per-account/model cooldowns. When every otherwise eligible account is cooling, an optional Redis lease admits one real waiting request as an early recovery probe. Probe start rate is shared by requested model across groups/instances; every group/model/quota/profit/session guard still applies. Failed probes retain their history. Success clears only the exact account/model cooldown generation observed by that attempt, including `/v1/messages`.

`gateway` settings in the operator's `config.yaml` (environment overrides use uppercase `GATEWAY_` names):

| Setting | Default | Behavior |
|---|---:|---|
| `mirasim_recovery_probe_interval_seconds` | 5 | Minimum interval between half-open probe starts; 0 disables. Distributed lease prevents concurrent probes while valid. Fresh failures wait at least the configured cooldown base before probing. Redis unavailable means ordinary cooldown selection only. |
| `mirasim_first_output_timeout_seconds` | 60 | Per-attempt signing/header/first meaningful SSE output deadline. Non-stream responses must complete within this deadline. 0 disables. Pings and empty metadata do not count. Text, thinking and tool starts commit the attempt; an accepted stream is never cut off by this deadline or replayed. |
| `mirasim_single_token_compatibility` | false | Explicitly allow `max_tokens: 1` to become 2 for Mirasim only. The upstream does not accept a one-token budget. This may produce one extra token; actual usage is billed. It never returns synthetic success. Other values/providers are unchanged. |

Existing recovery window, backoff/jitter, cooldown base/decay, and `mirasim_capacity_park_minutes` remain effective. Window 0 continues waiting until recovery or client cancellation. A long-wait policy cannot promise success while the provider has no capacity; caller/proxy deadlines still apply.

Edit the persistent operator configuration and restart the inactive blue/green instance; these are startup settings, not a hot-reload API. Validate that instance, switch the proxy after acceptance, then drain the old instance. Existing cooldown/quota rows require no reset or migration. Rolling back the binary is compatible with the new Redis lease keys, which expire.

Observability:

- `gateway.waiting_capacity`: next selection time, wait interval, account switch count.
- `gateway.mirasim_capacity_probe`: admitted account/model and age since failure.
- `gateway.request_timing`: total request duration, final attempt, preceding selection/retries, first output. Usage duration/TTFT includes time before the final attempt. Token amounts/prices/idempotency are unchanged.
- `gateway.mirasim_output_limit_compatibility`: requested and upstream output limits.
- Canceled Anthropic requests retain cancellation classification instead of a generic upstream 502.

Verification includes Redis concurrent claims/owner renewal/expiry, quota/model exclusions, hour-long cooldown recovery, first-output pings/header stalls, cancellation, and no truncation of accepted streams. Three protocol entrypoints are tested for account failover, exactly one usage record, cooldown recovery, and end-to-end duration. These simulated results do not prove live upstream capacity; release acceptance must include a real completion event.

## Output-budget origin diagnostics

`gateway.mirasim_request_limit_trace_enabled` defaults to false. Enabling it at startup records `gateway.request_limit_trace` with stages `ingress`, `forward`, and `egress` (final signed body before transport send). Each record carries the same gateway request correlation IDs, numeric API key/account IDs, the immutable ingress budget snapshot and the current one. Only numeric/type information for `max_tokens`, `max_output_tokens`, `max_completion_tokens`, and `stream` is retained; prompts, raw bodies, secrets and arbitrary string field values are never retained. These diagnostic events stay in normal rotating logs, not the Ops database log index.

Trace collection does not change output budgets. Existing compatibility rewriting has its own event. Disabled instrumentation produces no trace events. Enable only while diagnosing and disable again after collecting the relevant request. A final outgoing body record proves the sender's payload, not successful upstream receipt. An unrelated synthetic verification request must not be used as evidence for an external evaluator's original request.

## Mira pool controls

Mirasim API-key accounts support configurable request admission, active sessions and spending windows without being reclassified as OAuth accounts. In **Accounts → Edit**, Mira accounts have their own pool-control section. Existing concurrency remains in the standard account field. Operator values are stored in `account.extra`, loaded by the scheduler and exposed by the admin API/runtime counters. Missing/zero RPM, sessions or spend limit means disabled; no pool-wide thresholds are inserted on upgrade.

- `base_rpm`: hard **attempts per Redis server-minute** ceiling for Mira; checked atomically before signing/dispatch. Failed upstream attempts count. Rejected local attempts do not. Sticky sessions do not bypass it; blue/green instances share the counter. No success-side double count.
- `max_sessions`, `session_idle_timeout_minutes`: active session admission. Failed selections/forwarding release their registration; successful sessions use the existing idle lifetime. Existing shared-session idle semantics are retained, not an upstream concurrent-request guarantee.
- `window_cost_limit`, `window_cost_sticky_reserve`: standard-cost scheduling threshold, using the provider's current window or a rolling five hours when unknown. This retains the existing cached/post-usage **soft** budget; it is not a prepaid hard reservation and in-flight work can overshoot it. An explicit Mira reserve of zero is honored.
- A configured Mira RPM/session guard fails closed if its backing capability is unavailable. Ordinary API-key providers and OAuth credential/fingerprint behavior are unchanged.

Mira's RPM controls apply on the common transport-attempt path for Messages, Responses and Chat Completions. Generic account concurrency remains separate. Account-level controls do not substitute for provider/model capacity recovery or weekly allowance checks. This change does not alter system blocks, sampling, model routing, token counts or prices. The current three system blocks are retained until a successful full-prompt baseline permits a meaningful ablation experiment.
