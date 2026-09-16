package admin

// Admin surface for a single mirasim account's quota windows.
//
// The operator's question behind the console's 「查询」 button is "how much of
// this account's 5h / 7d budget is left, right now". The answer is a snapshot,
// and the endpoint returns the SAME snapshot object the account list embeds as
// `mirasim_quota` — built by the same mapper — so a refresh can never disagree
// with the cell it refreshes.

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// MirasimQuotaResponse is what POST /admin/accounts/:id/mirasim-quota returns.
type MirasimQuotaResponse struct {
	AccountID int64 `json:"account_id"`
	// Probed reports whether this call actually went upstream, as opposed to
	// returning the stored reading. It exists so the console can never present a
	// re-read as a fresh measurement: with Probed=false the numbers are as old
	// as Quota.observed_at says they are, and saying "探测成功" over them would
	// be a lie the operator cannot detect.
	Probed bool `json:"probed"`
	// Quota is the same shape as Account.mirasim_quota. Never null here — the
	// endpoint rejects non-mirasim accounts before it gets this far — but the
	// halves inside it are empty until their producers have written something.
	Quota *dto.MirasimQuotaSnapshot `json:"quota"`
}

// mirasimQuotaProber is the seam the live /v1/limits probe plugs into. Its
// signature is *service.MirasimQuotaProbeService.ProbeAccount verbatim, so
// wiring the real service needs no adapter.
type mirasimQuotaProber interface {
	ProbeAccount(ctx context.Context, accountID int64) (*service.MirasimQuotaProbeSnapshot, error)
}

// mirasimQuotaProber returns the live prober, or nil when none is wired.
//
// **这里的显式 nil 判断不是多余的防御，是这个函数唯一的技术含量。**
// `h.mirasimQuotaProbe` 是一个 *service.MirasimQuotaProbeService。把一个 nil 的
// 具体指针直接 return 成 mirasimQuotaProber 接口，得到的接口值**不等于 nil**
// （接口的类型字段非空），于是下面 `prober != nil` 会成立，接着在 nil 接收者上
// 调 ProbeAccount —— 这是 Go 里最经典的那个坑。service 侧确实写了 `if s == nil`
// 的守卫，所以它不会 panic，而是回 ErrMirasimQuotaProbeUnavailable：
// **端点会从「诚实地回显旧读数」退化成「报一个假的探测失败」**，恰好是本文件
// 最想避免的那种谎。
//
// 历史：这个函数曾经硬编码 `return nil`（接线 TODO 未完成），症状是控制台上
// 「查询」按钮永远返回 Probed=false，操作者看到的是后台探测器上次存下的
// status=failed，于是「点一下探测」永远显示失败，而实际上一次上游请求都没发出去。
// 2026-09-16 补齐接线。
func (h *AccountHandler) mirasimQuotaProber() mirasimQuotaProber {
	if h == nil || h.mirasimQuotaProbe == nil {
		return nil
	}
	return h.mirasimQuotaProbe
}

// ProbeMirasimQuota handles POST /admin/accounts/:id/mirasim-quota.
//
// Auth and rate limiting are inherited from the /admin group exactly like the
// neighbouring per-account probes (mirasim-plan-probe, ollama-cloud-usage/refresh):
// admin auth, the panel-wide per-user limiter, and the audit-log middleware.
// Nothing extra is layered on here, so the three endpoints cannot drift apart.
func (h *AccountHandler) ProbeMirasimQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	ctx := c.Request.Context()
	account, err := h.adminService.GetAccount(ctx, accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if !service.IsMirasimAccount(account) {
		// Rejected rather than answered with an empty snapshot: "this account
		// has no quota dimension" and "this account's quota is unknown" are
		// different answers and the console acts differently on them. The error
		// is the probe service's own, so there is one reason code for this
		// condition whether it is raised here or inside the probe.
		response.ErrorFrom(c, service.ErrMirasimQuotaProbeAccountInvalid)
		return
	}

	probed := false
	if prober := h.mirasimQuotaProber(); prober != nil {
		if _, err := prober.ProbeAccount(ctx, accountID); err != nil {
			response.ErrorFrom(c, err)
			return
		}
		probed = true
		// Re-read: the probe persists into accounts.extra, so the account value
		// fetched before it ran is stale by construction. The response is built
		// from the STORED state either way, never from the probe's return value
		// — one shape for the cell, whether or not a probe just ran.
		if refreshed, err := h.adminService.GetAccount(ctx, accountID); err == nil {
			account = refreshed
		}
	}

	response.Success(c, MirasimQuotaResponse{
		AccountID: accountID,
		Probed:    probed,
		// Built through the account mapper on purpose. The alternative — a
		// second construction path just for this endpoint — is how the refresh
		// button starts returning a shape the cell cannot read.
		Quota: dto.AccountFromServiceShallow(account).MirasimQuota,
	})
}
