package admin

// Admin surface for mirasim subscription plans.
//
// The operator's question is "which of these 143 accounts is max, which is plus,
// and which ones expire soon" — so the list endpoint returns the RESOLVED plan
// per account together with where that value came from, and filters on the
// resolved value. An account whose import label disagrees with the authoritative
// reading is returned with both values and a note, never with one silently
// replacing the other.

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type mirasimPlanProbeEnabledRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

type mirasimPlanProbeBatchRequest struct {
	AccountIDs []int64 `json:"account_ids" binding:"required"`
}

// MirasimPlanListResponse is the console payload. Counts are computed over the
// FILTERED set so a filtered view's totals match its rows.
type MirasimPlanListResponse struct {
	Items    []service.MirasimPlanItem `json:"items"`
	Total    int                       `json:"total"`
	ByPlan   map[string]int            `json:"by_plan"`
	Mismatch int                       `json:"mismatch"`
	Probed   int                       `json:"probed"`
}

// ListMirasimPlans handles GET /admin/accounts/mirasim-plan.
//
// Query: plan=plus|max (resolved tier), mismatch=true (only accounts whose claim
// and authoritative tier disagree).
func (h *AccountHandler) ListMirasimPlans(c *gin.Context) {
	ctx := c.Request.Context()
	planFilter := strings.TrimSpace(c.Query("plan"))
	mismatchOnly := strings.EqualFold(strings.TrimSpace(c.Query("mismatch")), "true")

	// mirasim accounts are platform=anthropic type=apikey; BuildMirasimPlanItems
	// drops every anthropic account that is not one.
	accounts, err := h.listAccountsFiltered(ctx, service.PlatformAnthropic, service.AccountTypeAPIKey, "", "", 0, "", "created_at", "desc")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	items := service.BuildMirasimPlanItems(accounts, planFilter, mismatchOnly)

	payload := MirasimPlanListResponse{
		Items:  items,
		Total:  len(items),
		ByPlan: map[string]int{},
	}
	for _, item := range items {
		plan := item.Plan
		if plan == "" {
			plan = "unknown"
		}
		payload.ByPlan[plan]++
		if item.PlanMismatch {
			payload.Mismatch++
		}
		if item.PlanSource == service.MirasimPlanSourceAuthoritative {
			payload.Probed++
		}
	}
	response.Success(c, payload)
}

func (h *AccountHandler) GetMirasimPlanProbeSettings(c *gin.Context) {
	if h.mirasimPlanProbe == nil {
		response.ErrorFrom(c, service.ErrMirasimPlanProbeUnavailable)
		return
	}
	settings, err := h.mirasimPlanProbe.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) UpdateMirasimPlanProbeSettings(c *gin.Context) {
	if h.mirasimPlanProbe == nil {
		response.ErrorFrom(c, service.ErrMirasimPlanProbeUnavailable)
		return
	}
	var req service.MirasimPlanProbeSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.mirasimPlanProbe.UpdateSettings(c.Request.Context(), &req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	settings, err := h.mirasimPlanProbe.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) SetMirasimPlanProbeEnabled(c *gin.Context) {
	if h.mirasimPlanProbe == nil {
		response.ErrorFrom(c, service.ErrMirasimPlanProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req mirasimPlanProbeEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.mirasimPlanProbe.SetAccountEnabled(c.Request.Context(), accountID, *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_id": accountID, "enabled": *req.Enabled})
}

// ProbeMirasimPlan handles POST /admin/accounts/:id/mirasim-plan-probe.
func (h *AccountHandler) ProbeMirasimPlan(c *gin.Context) {
	if h.mirasimPlanProbe == nil {
		response.ErrorFrom(c, service.ErrMirasimPlanProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	snapshot, err := h.mirasimPlanProbe.ProbeAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, service.MirasimPlanProbeResult{AccountID: accountID, Snapshot: snapshot})
}

// ProbeMirasimPlanBatch handles POST /admin/accounts/mirasim-plan/batch. It is
// the practical backfill path for accounts imported before plan fields existed:
// the authoritative half needs no source file, only the account's own egress.
func (h *AccountHandler) ProbeMirasimPlanBatch(c *gin.Context) {
	if h.mirasimPlanProbe == nil {
		response.ErrorFrom(c, service.ErrMirasimPlanProbeUnavailable)
		return
	}
	var req mirasimPlanProbeBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.AccountIDs) == 0 || len(req.AccountIDs) > service.MirasimPlanProbeMaxBatchSize {
		response.BadRequest(c, "account_ids must contain between 1 and 20 items")
		return
	}
	seen := make(map[int64]struct{}, len(req.AccountIDs))
	accountIDs := make([]int64, 0, len(req.AccountIDs))
	for _, accountID := range req.AccountIDs {
		if accountID <= 0 {
			response.BadRequest(c, "account_ids must contain positive IDs")
			return
		}
		if _, exists := seen[accountID]; exists {
			continue
		}
		seen[accountID] = struct{}{}
		accountIDs = append(accountIDs, accountID)
	}
	response.Success(c, gin.H{"results": h.mirasimPlanProbe.ProbeAccounts(c.Request.Context(), accountIDs)})
}
