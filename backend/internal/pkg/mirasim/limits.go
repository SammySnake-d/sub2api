package mirasim

// Quota-window state for a mirasim account, read from GET <relay base>/v1/limits.
//
// WHAT IT ANSWERS: how much of each of the account's four independent quota
// windows (5h, 7d, and the two per-family 7-day windows) is already spent, and
// when each one resets. That is the only PROACTIVE source of that fact: every
// other channel sub2api has is reactive (a 429 that already cost an upstream
// call) or partial (the response headers carry utilization for the two global
// windows only, and only on accounts that are receiving traffic).
//
// ---------------------------------------------------------------------------
// WHY THIS CALL MUST BE DEVICE-SIGNED — the opposite of FetchReferral.
// ---------------------------------------------------------------------------
//
// /auth/referral lives on the AUTH server and authenticates a plain bearer;
// signing it would replace that bearer with a relay device ticket the auth
// server never issued (see FetchReferral's long note). /v1/limits is the
// mirror image: it lives on the RELAY, and the relay answers an UNSIGNED
// request with a shared placeholder budget (~11667 on the 7d window) instead of
// this account's real numbers. ma-relay records exactly that
// (internal/relay/pool.go: "upstream returns the account's REAL 5h/7d budget on
// /v1/limits only when the request carries the device signature ... plus the
// usage-probe marker"). An unsigned probe therefore does not fail — it SUCCEEDS
// with a number that belongs to nobody, which is the single most dangerous
// outcome for a value that gates scheduling.
//
// The probe marker itself (x-mirasim-probe: usage) is set by ApplyContextHeaders
// for this path and then travels INSIDE the seal, exactly as in the real client:
// SealHeaders folds every x-mirasim-* header except x-mirasim-client into
// x-mirasim-enc. The one part of that branch that stays observable on the wire
// is accept-encoding: identity — which is also why it is the honest assertion
// for "the /v1/limits branch ran".
//
// ZERO TOKENS: this is a GET with no body. It costs an upstream request and,
// when the access token is near expiry, a refresh — it never consumes quota.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Quota-probe state lives in accounts.extra for the same reasons the plan probe
// keys do: none of it is a secret, the admin DTO does not redact extra, and
// every write to credentials on an apikey account drops extra keys written by
// other probes.
const (
	// ExtraQuotaProbe holds the /v1/limits snapshot written by the quota probe.
	// It is the ONLY key that probe writes.
	ExtraQuotaProbe = "mirasim_quota_probe"
	// ExtraQuotaProbeEnabled is the per-account opt-OUT (absent means enabled):
	// a quota reading is what keeps the scheduler from spending requests on an
	// account whose window is already spent, so the useful default is on.
	ExtraQuotaProbeEnabled = "mirasim_quota_probe_enabled"
)

// LimitsPath is the relay path the quota probe reads. It is also the path the
// signature covers and the value ApplyContextHeaders keys the usage-probe branch
// off, so the three MUST be the same constant.
const LimitsPath = "/v1/limits"

// limitsTimeout bounds one /v1/limits call.
//
// **这个值必须大于出口代理的换节点预算**,否则一次正常的节点轮换会被误判成超时:
//
//	sub2api(国内) →跨境~150ms→ resin(海外) →最多换 3 个节点,每次 8s→ 节点 → 上游
//
// 代理层光是建连最坏就要 24s(resin 的 connectDialMaxAttempts ×
// connectDialAttemptTimeout)。40s = 24s + 上游真实处理时间 + 余量。改小它之前先去
// 看 resin 那两个常量;两个数是绑在一起的。
//
// **但它不是「大量账号报 timeout」的修复,这一点要写清楚,因为我当时归因错了。**
//
// 2026-09-16 的错误与纠正,完整记下来以免重走:原值是照抄 ma-relay 的 10s,观测到
// 143 个账号里 43 个报 timeout,于是判断「预算不够,代理还没换完节点我们就放弃了」,
// 把它提到 40s。**结果 timeout 从 43 涨到 50 —— 方向是错的。** 真根因在出口池:
// resin 的 sticky 租约只按 TTL(168h)回收,不看节点是否已熔断或已超出平台的延迟
// 天花板,所以一个 640ms、已熔断的坏节点会把账号钉住整整一周。收紧订阅源白名单并
// 释放失格租约之后,同一个账号的探测从 97s 降到 0.73s,全量分布从
// 「84 ok / 58 failed」变成「140 ok / 0 timeout」。
//
// 教训:探测超时是**症状的度量尺**,不是症状的成因。看到一批 timeout 时先问
// 「这条链路的哪一段在慢」,不要先调放弃等待的阈值 —— 放宽超时只会让每一次失败
// 等得更久,把问题从「快速失败」变成「慢速失败」。
const limitsTimeout = 40 * time.Second

// LimitsWindow is one quota window as /v1/limits reports it.
//
// Used and Budget are ABSOLUTE counts in the upstream's own unit; ResetAt is a
// Unix second. Utilization is deliberately NOT a field here: it is a derived
// value that only exists when Budget > 0, and deriving it in the caller is what
// keeps "budget unknown" from silently becoming "0% used".
//
// EVIDENCE GRADE: PORTED, THEN CONFIRMED IN SHAPE. Every json tag is copied from
// ma-relay internal/relay/quota.go probeQuotaOne, which has been reading this
// endpoint in production for these same accounts.
//
// WHAT HAPPENS IF THE SHAPE IS WRONG — the same two modes as ReferralInfo, and
// the same division of responsibility:
//
//  1. the body is not JSON → FetchLimits returns a *LimitsDecodeError and the
//     caller records a failed probe;
//  2. the body is JSON under names this struct does not know → encoding/json
//     decodes an all-zero struct with NO error. Windows comes back empty. The
//     CALLER must reject that (service.MirasimQuotaProbeService records it as
//     "empty_windows"), because a zero-window snapshot recorded as OK would
//     read as "this account has no quota state" rather than "we could not read
//     it".
//
// Do not add a default to any field below. A defaulted Budget would turn "the
// shape is wrong" into a plausible-looking quota.
type LimitsWindow struct {
	Name    string  `json:"name"`
	Used    float64 `json:"used"`
	Budget  float64 `json:"budget"`
	ResetAt int64   `json:"reset_at"`
}

// LimitsInfo is one account's full quota reading.
type LimitsInfo struct {
	// Suspended is the upstream saying this account is halted outright, which is
	// not the same as any window being spent.
	Suspended bool           `json:"suspended"`
	Windows   []LimitsWindow `json:"windows"`
}

// LimitsRejectedError signals that the relay answered /v1/limits with a non-2xx
// status, as opposed to the request never arriving.
type LimitsRejectedError struct{ Status int }

func (e *LimitsRejectedError) Error() string {
	return fmt.Sprintf("mirasim limits returned HTTP %d", e.Status)
}

// LimitsDecodeError signals a 2xx whose body is not JSON at all (an HTML error
// page, a proxy interstitial, a truncated stream).
//
// It is kept apart from LimitsRejectedError on purpose: "the relay refused us"
// and "something in the path answered for the relay" call for different operator
// actions, and collapsing both into a bare error would erase the HTTP status,
// which is the only thing that distinguishes them.
type LimitsDecodeError struct{ Status int }

func (e *LimitsDecodeError) Error() string {
	return fmt.Sprintf("mirasim limits returned HTTP %d with a body that is not limits JSON", e.Status)
}

// LimitsStatus extracts the upstream HTTP status from a FetchLimits error, or 0
// when the failure was not an upstream answer at all (transport, timeout).
func LimitsStatus(err error) int {
	var rejected *LimitsRejectedError
	if errors.As(err, &rejected) {
		return rejected.Status
	}
	var decode *LimitsDecodeError
	if errors.As(err, &decode) {
		return decode.Status
	}
	return 0
}

// IsLimitsDecodeFailure reports whether err is a 2xx with an unreadable body.
func IsLimitsDecodeFailure(err error) bool {
	var decode *LimitsDecodeError
	return errors.As(err, &decode)
}

// relayBaseFor returns the relay base the account's credential is currently
// bound to, falling back to DefaultRelayBase.
//
// It exists so the fallback rule lives in exactly ONE place. A caller that
// re-derived it from its own Identity would be a second copy of that rule, free
// to drift — and a probe sent to the wrong base is not an error, it is a
// perfectly successful reading of somebody else's relay.
func (r *Registry) relayBaseFor(accountID int64) string {
	c := r.get(accountID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if base := strings.TrimRight(strings.TrimSpace(c.relayBase), "/"); base != "" {
		return base
	}
	return DefaultRelayBase
}

// FetchLimits reads one account's quota windows.
//
// RETURN CONTRACT — a non-nil *LimitsInfo means "the relay answered 2xx and the
// body was valid JSON". It does NOT mean the body was a limits payload: see
// LimitsWindow's evidence grade. Callers MUST treat an empty Windows slice as a
// failed reading rather than as an account with no quota state.
//
// It reuses the SAME per-account machinery as an outbound data-plane request —
// Prepare does the identity sync, the token refresh when the access token is
// within refreshBefore of expiry, and the device-ticket mint — so a quota probe
// never sends a stale token and never forks a second view of the account's token
// state. The caller's Doer supplies the account's own egress (its proxy, its TLS
// fingerprint), so the probe leaves from the IP the upstream already associates
// with this device.
func (r *Registry) FetchLimits(
	ctx context.Context,
	accountID int64,
	ident Identity,
	client Doer,
	persistCredentials Persister,
	persistExtra Persister,
) (*LimitsInfo, error) {
	if r == nil {
		return nil, errors.New("mirasim registry is nil")
	}
	if client == nil {
		return nil, errors.New("mirasim limits probe has no HTTP client")
	}

	prepared, err := r.Prepare(ctx, accountID, ident, client, persistCredentials, persistExtra)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prepared.Credential) == "" {
		return nil, errors.New("mirasim account has no credential to read its limits with")
	}

	reqCtx, cancel := context.WithTimeout(ctx, limitsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, r.relayBaseFor(accountID)+LimitsPath, nil)
	if err != nil {
		return nil, err
	}
	// The bearer and the credential folded into the canonical signing tuple MUST
	// be the same string; Prepared.Credential is the one value that is both.
	req.Header.Set("authorization", "Bearer "+prepared.Credential)
	ApplyContextHeaders(req.Header, ContextInput{
		Path:       LimitsPath,
		SessionID:  prepared.SessionID,
		AccountSub: prepared.AccountSub,
		Locale:     prepared.Locale,
	})
	// Signs the empty body and then seals every x-mirasim-* header. Unlike the
	// referral probe, a signature failure here is fatal: an unsigned /v1/limits
	// is answered with the shared placeholder budget, so sending it anyway would
	// produce a confident wrong number instead of a recorded failure.
	if err := SignAndSeal(req.Header, prepared.Signer, http.MethodGet, LimitsPath, nil, prepared.Credential); err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mirasim limits transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &LimitsRejectedError{Status: resp.StatusCode}
	}
	var info LimitsInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, &LimitsDecodeError{Status: resp.StatusCode}
	}
	return &info, nil
}
