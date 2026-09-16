package admin

// Contract test for the mirasim quota DTO.
//
// WHY THIS FILE EXISTS: the frontend quota cell is written against the JSON
// names and shapes pinned below, and every way of breaking that contract is
// SILENT. Rename `mirasim_quota` and the cell renders nothing. Turn `windows`
// from [] into null and the renderer's map() throws instead. Swap `utilization`
// from a 0..1 fraction to a percent and every bar reads 100x wrong. Drop the
// field from the list item and the account TABLE — the one view that needs it —
// stays blank while the detail API looks correct. None of these produce a
// compile error, a 500, or a log line: they produce a blank cell that looks like
// "this account has no quota".
//
// So the assertions compare whole JSON subtrees, not struct fields. A subtree
// comparison catches renames, removals AND silent additions; reading
// `snapshot.Windows` in Go would catch none of the three.
//
// The fixtures are accounts.extra values, not Go views, so each test exercises
// the whole read path — service.BuildMirasimQuotaSnapshot composing the stored
// snapshot, then this layer projecting it onto the wire shape. A change on
// either side that would reach the browser fails here.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Fixed instants, written out in both representations the two producers use, so
// the golden JSON below can stay literal.
const (
	mirasimQuotaTestResetUnix  = int64(1790121600) // 2026-09-23T00:00:00Z
	mirasimQuotaTestResetJSON  = "2026-09-23T00:00:00Z"
	mirasimQuotaTestObservedAt = "2026-09-16T03:04:05Z"
	mirasimQuotaTestSampledAt  = "2026-09-16T03:00:00Z"
)

func mirasimQuotaTestAccount(extra map[string]any) *service.Account {
	return &service.Account{
		ID:       42,
		Name:     "mira-42",
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		// The provider credential is the ONLY thing that makes an
		// anthropic/apikey account a mirasim account (service.IsMirasimAccount).
		Credentials: map[string]any{mirasim.CredProvider: mirasim.ProviderMirasim},
		Extra:       extra,
	}
}

// mirasimQuotaSubtree marshals the full account DTO and hands back the raw
// `mirasim_quota` value. It asserts the KEY IS PRESENT rather than tolerating an
// omitted field: "not a mirasim account" must be an explicit null, so a client
// can tell it apart from a backend that does not know the field at all.
func mirasimQuotaSubtree(t *testing.T, account *service.Account) string {
	t.Helper()
	raw, err := json.Marshal(dto.AccountFromServiceShallow(account))
	require.NoError(t, err)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &envelope))
	value, ok := envelope["mirasim_quota"]
	require.True(t, ok, "字段 mirasim_quota 必须始终出现在账号 DTO 里（非 mirasim 账号是显式 null，不是省略）")
	return string(value)
}

// TestAccountDTOMirasimQuotaIsNullForNonMirasimAccounts pins the negative half
// of the contract: the field exists for every account and is null unless the
// account is actually a mirasim one. An empty object here would make the console
// grow a quota cell on anthropic, openai and gemini accounts alike.
func TestAccountDTOMirasimQuotaIsNullForNonMirasimAccounts(t *testing.T) {
	plainAnthropic := &service.Account{
		ID:          7,
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
	}
	require.JSONEq(t, `null`, mirasimQuotaSubtree(t, plainAnthropic))

	openai := &service.Account{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	require.JSONEq(t, `null`, mirasimQuotaSubtree(t, openai))
}

// TestAccountDTOMirasimQuotaIsEmptyNotFakedWhenNothingHasBeenProbed pins the
// "no data" shape. The two halves have independent producers, so a mirasim
// account with neither reading must say so: source "", windows [], plan "".
// Inventing a plan label or a zero-budget window here is what would show an
// unprobed account as fully available.
func TestAccountDTOMirasimQuotaIsEmptyNotFakedWhenNothingHasBeenProbed(t *testing.T) {
	require.JSONEq(t, `{
		"source": "",
		"status": "",
		"suspended": false,
		"observed_at": null,
		"windows": [],
		"plan": "",
		"next_plan": "",
		"plan_expires_at": "",
		"plan_source": "unknown"
	}`, mirasimQuotaSubtree(t, mirasimQuotaTestAccount(nil)))
}

// TestAccountDTOMirasimQuotaFromLimitsProbe pins the full-fidelity shape: the
// /v1/limits probe is the only source with absolute counts.
//
// It carries the two most dangerous edges in one payload:
//   - the 7d_fable window had no budget reported, so utilization stays null —
//     NOT 0, which would draw a full green bar over "we have no idea";
//   - the 5h utilization is 1.02, a real observed value from this upstream
//     meaning 102% of budget consumed. It passes through unclamped and
//     unrescaled; clamping belongs to the bar's width, not to the number.
func TestAccountDTOMirasimQuotaFromLimitsProbe(t *testing.T) {
	account := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraQuotaProbe: map[string]any{
			"status":      "ok",
			"suspended":   false,
			"observed_at": mirasimQuotaTestObservedAt,
			"windows": []any{
				map[string]any{
					"name":        "5h",
					"used":        float64(51),
					"budget":      float64(50),
					"utilization": 1.02,
					"reset_at":    mirasimQuotaTestResetJSON,
				},
				map[string]any{
					"name":        "7d",
					"used":        float64(2500),
					"budget":      float64(10000),
					"utilization": 0.25,
				},
				map[string]any{
					// Budget not reported: "剩余未知", never "剩余 100%".
					"name": "7d_fable",
					"used": float64(3),
				},
			},
		},
	})

	require.JSONEq(t, `{
		"source": "limits",
		"status": "ok",
		"suspended": false,
		"observed_at": "`+mirasimQuotaTestObservedAt+`",
		"windows": [
			{"name": "5h",       "used": 51,   "budget": 50,    "utilization": 1.02, "reset_at": "`+mirasimQuotaTestResetJSON+`"},
			{"name": "7d",       "used": 2500, "budget": 10000, "utilization": 0.25, "reset_at": null},
			{"name": "7d_fable", "used": 3,    "budget": null,  "utilization": null, "reset_at": null}
		],
		"plan": "",
		"next_plan": "",
		"plan_expires_at": "",
		"plan_source": "unknown"
	}`, mirasimQuotaSubtree(t, account))
}

// TestAccountDTOMirasimQuotaKeepsUnrecognisedWindowNamesVerbatim: only "5h" and
// "7d" are observed tokens; "7d_claude"/"7d_fable" are inferred and the nearest
// real evidence spells the family window "7d_oi". So an unexpected name is
// EVIDENCE, and it has to reach the console as itself. Mapping it onto a known
// token would erase the only signal that the inferred vocabulary is wrong;
// dropping it would hide a window the upstream is actually enforcing.
func TestAccountDTOMirasimQuotaKeepsUnrecognisedWindowNamesVerbatim(t *testing.T) {
	account := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraQuotaProbe: map[string]any{
			"status":      "ok",
			"observed_at": mirasimQuotaTestObservedAt,
			"windows": []any{
				map[string]any{"name": "7d_oi", "used": float64(1), "budget": float64(4), "utilization": 0.25},
			},
		},
	})

	require.JSONEq(t, `{
		"source": "limits",
		"status": "ok",
		"suspended": false,
		"observed_at": "`+mirasimQuotaTestObservedAt+`",
		"windows": [
			{"name": "7d_oi", "used": 1, "budget": 4, "utilization": 0.25, "reset_at": null}
		],
		"plan": "",
		"next_plan": "",
		"plan_expires_at": "",
		"plan_source": "unknown"
	}`, mirasimQuotaSubtree(t, account))
}

// TestAccountDTOMirasimQuotaFromPassiveHeaders pins the degraded source. Header
// sampling carries a ratio and nothing else, so used/budget MUST stay null: a
// back-derived count would be a number the upstream never said at any single
// instant, and on the wire it would be indistinguishable from a probed one.
//
// The 5h reset stays null too, even though this account has a session_window_end
// — that column is a 5h boundary the service layer may have PREDICTED rather
// than observed, so the quota contract does not pass it off as an upstream
// reading. A renderer that wants it can read session_window_end itself, where
// its provenance is visible.
func TestAccountDTOMirasimQuotaFromPassiveHeaders(t *testing.T) {
	sessionEnd, err := time.Parse(time.RFC3339, mirasimQuotaTestResetJSON)
	require.NoError(t, err)
	account := mirasimQuotaTestAccount(map[string]any{
		// 0 is a real reading (a freshly reset window), not an absence.
		"session_window_utilization":   float64(0),
		"passive_usage_7d_utilization": 0.42,
		"passive_usage_7d_reset":       mirasimQuotaTestResetUnix,
		"passive_usage_sampled_at":     mirasimQuotaTestSampledAt,
	})
	account.SessionWindowEnd = &sessionEnd

	require.JSONEq(t, `{
		"source": "headers",
		"status": "",
		"suspended": false,
		"observed_at": "`+mirasimQuotaTestSampledAt+`",
		"windows": [
			{"name": "5h", "used": null, "budget": null, "utilization": 0,    "reset_at": null},
			{"name": "7d", "used": null, "budget": null, "utilization": 0.42, "reset_at": "`+mirasimQuotaTestResetJSON+`"}
		],
		"plan": "",
		"next_plan": "",
		"plan_expires_at": "",
		"plan_source": "unknown"
	}`, mirasimQuotaSubtree(t, account))
}

// TestAccountDTOMirasimQuotaPrefersLimitsOverHeadersWithoutMerging pins the
// precedence AND the no-merge rule. ma-relay's header path merges header
// utilization into whatever a previous probe left behind and appends an empty
// shell for windows the probe never saw; replicating that would put budget=0
// windows on screen, which read as "探到了，额度是 0" instead of "没探到".
func TestAccountDTOMirasimQuotaPrefersLimitsOverHeadersWithoutMerging(t *testing.T) {
	account := mirasimQuotaTestAccount(map[string]any{
		"passive_usage_7d_utilization": 0.42,
		"passive_usage_sampled_at":     mirasimQuotaTestSampledAt,
		mirasim.ExtraQuotaProbe: map[string]any{
			"status":      "ok",
			"observed_at": mirasimQuotaTestObservedAt,
			"windows": []any{
				map[string]any{"name": "7d", "used": float64(1), "budget": float64(4), "utilization": 0.25},
			},
		},
	})

	require.JSONEq(t, `{
		"source": "limits",
		"status": "ok",
		"suspended": false,
		"observed_at": "`+mirasimQuotaTestObservedAt+`",
		"windows": [
			{"name": "7d", "used": 1, "budget": 4, "utilization": 0.25, "reset_at": null}
		],
		"plan": "",
		"next_plan": "",
		"plan_expires_at": "",
		"plan_source": "unknown"
	}`, mirasimQuotaSubtree(t, account))
}

// TestAccountDTOMirasimQuotaReportsFailingProbeOverStoredWindows pins the
// staleness signal. A probe that is currently failing still has to surface
// status "failed" next to the last good reading it carried over — otherwise the
// console shows day-old numbers with nothing to say that nothing has refreshed
// them, which is the one state an operator cannot detect by looking.
func TestAccountDTOMirasimQuotaReportsFailingProbeOverStoredWindows(t *testing.T) {
	account := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraQuotaProbe: map[string]any{
			"status":      "failed",
			"suspended":   true,
			"observed_at": mirasimQuotaTestObservedAt,
			"windows": []any{
				map[string]any{"name": "7d", "used": float64(1), "budget": float64(2), "utilization": 0.5},
			},
		},
	})

	require.JSONEq(t, `{
		"source": "limits",
		"status": "failed",
		"suspended": true,
		"observed_at": "`+mirasimQuotaTestObservedAt+`",
		"windows": [
			{"name": "7d", "used": 1, "budget": 2, "utilization": 0.5, "reset_at": null}
		],
		"plan": "",
		"next_plan": "",
		"plan_expires_at": "",
		"plan_source": "unknown"
	}`, mirasimQuotaSubtree(t, account))
}

// TestAccountDTOMirasimQuotaCarriesPlanWithItsEvidenceGrade pins the plan half,
// including plan_source. The import label is known to over-state the tier
// ("标 max 实为 plus"), so a console that renders a claimed plan as fact repeats
// that error in the UI — the grade has to travel with the value.
func TestAccountDTOMirasimQuotaCarriesPlanWithItsEvidenceGrade(t *testing.T) {
	claimed := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraPlanClaimed:   "max",
		mirasim.ExtraPlanExpiresAt: "2027-09-10T19:20:40.460323Z",
	})
	require.JSONEq(t, `{
		"source": "",
		"status": "",
		"suspended": false,
		"observed_at": null,
		"windows": [],
		"plan": "max",
		"next_plan": "",
		"plan_expires_at": "2027-09-10T19:20:40.460323Z",
		"plan_source": "claimed"
	}`, mirasimQuotaSubtree(t, claimed))

	probed := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraPlanClaimed: "max",
		mirasim.ExtraPlanProbe: map[string]any{
			"status":          "ok",
			"plan":            "plus",
			"next_plan":       "max",
			"plan_expires_at": "2027-09-10T19:20:40.460323Z",
			"pending_upgrade": true,
			"last_attempt_at": mirasimQuotaTestObservedAt,
			"next_probe_at":   mirasimQuotaTestObservedAt,
		},
	})
	require.JSONEq(t, `{
		"source": "",
		"status": "",
		"suspended": false,
		"observed_at": null,
		"windows": [],
		"plan": "plus",
		"next_plan": "max",
		"plan_expires_at": "2027-09-10T19:20:40.460323Z",
		"plan_source": "authoritative"
	}`, mirasimQuotaSubtree(t, probed))
}

// TestAccountListItemCarriesMirasimQuota pins the field on the LIST projection.
// The account table is the surface that renders the quota cell, and it is served
// by AccountListItem, not Account — a contract that only covered the detail DTO
// would pass while the table stayed blank.
func TestAccountListItemCarriesMirasimQuota(t *testing.T) {
	account := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraQuotaProbe: map[string]any{
			"status":      "ok",
			"observed_at": mirasimQuotaTestObservedAt,
			"windows": []any{
				map[string]any{"name": "7d", "used": float64(1), "budget": float64(4), "utilization": 0.25},
			},
		},
	})

	detail := dto.AccountFromServiceShallow(account)
	item := dto.AccountListItemFromAccount(detail)

	detailRaw, err := json.Marshal(detail)
	require.NoError(t, err)
	itemRaw, err := json.Marshal(item)
	require.NoError(t, err)

	var detailEnvelope, itemEnvelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(detailRaw, &detailEnvelope))
	require.NoError(t, json.Unmarshal(itemRaw, &itemEnvelope))

	itemQuota, ok := itemEnvelope["mirasim_quota"]
	require.True(t, ok, "列表项也必须带 mirasim_quota —— 账号表格正是渲染额度格的那个视图")
	require.JSONEq(t, string(detailEnvelope["mirasim_quota"]), string(itemQuota),
		"列表项与详情的 mirasim_quota 必须逐字相同，否则同一个账号在两个视图里显示不同额度")
}

// TestProbeMirasimQuotaEndpointReturnsTheSameSnapshotShape pins the 「查询」
// button's contract: the envelope names, and the fact that `quota` is the very
// same subtree the cell already knows how to render. `probed` is pinned too —
// an instance with NO prober injected must report false, so the console cannot
// present a stored reading as a fresh measurement.
//
// 注意这条断言说的是「这个 handler 实例没有注入探测器」，**不是**「本仓还没接线」。
// 两者曾经是同一件事，而那正是缺陷：mirasimQuotaProber() 硬编码 return nil，
// 于是生产上点「查询」永远回显后台上次存的 status=failed，一次上游请求都不发。
// 接线补齐后这条仍然成立且仍然有价值 —— 见下面那条配对的阳性测试。
func TestProbeMirasimQuotaEndpointReturnsTheSameSnapshotShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := mirasimQuotaTestAccount(map[string]any{
		mirasim.ExtraQuotaProbe: map[string]any{
			"status":      "ok",
			"observed_at": mirasimQuotaTestObservedAt,
			"windows": []any{
				map[string]any{"name": "7d", "used": float64(1), "budget": float64(4), "utilization": 0.25},
			},
		},
	})
	stub := newStubAdminService()
	stub.getAccountResult = account
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts/:id/mirasim-quota", handler.ProbeMirasimQuota)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/42/mirasim-quota", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var body struct {
		Data struct {
			AccountID int64           `json:"account_id"`
			Probed    bool            `json:"probed"`
			Quota     json.RawMessage `json:"quota"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, int64(42), body.Data.AccountID)
	require.False(t, body.Data.Probed, "没有注入探测器的实例必须报 probed=false，不能把库里的旧读数说成刚探到的")
	require.JSONEq(t, mirasimQuotaSubtree(t, account), string(body.Data.Quota),
		"端点返回的 quota 必须与账号 DTO 里的 mirasim_quota 逐字相同")
}

// TestMirasimQuotaProberIsNilOnlyWhenNoServiceWasInjected 是接线缺陷的**阳性标定**。
//
// 上一条测试（probed=false）单独存在时鉴别力为零：把 mirasimQuotaProber() 改回
// 硬编码 `return nil`，它照样全绿 —— 生产上那个「点探测永远失败」的缺陷就是在
// 这种全绿下活了很久的。本条与它配对，断言**注入了就必须能用**。
func TestMirasimQuotaProberIsNilOnlyWhenNoServiceWasInjected(t *testing.T) {
	h := &AccountHandler{}
	require.Nil(t, h.mirasimQuotaProber(), "没注入时必须是 nil，否则端点会在 nil 接收者上探测")

	h.SetMirasimQuotaProbeService(&service.MirasimQuotaProbeService{})
	require.NotNil(t, h.mirasimQuotaProber(),
		"注入了探测器却拿不到 —— 这正是 mirasimQuotaProber() 硬编码 return nil 时的形态："+
			"控制台的「查询」永远回显后台旧读数，点一下就显示失败，而上游一次请求都没收到")
}

// TestMirasimQuotaProberDoesNotLeakATypedNil 是 Go 接口那个经典坑的差分保护。
//
// 去掉 mirasimQuotaProber() 里的显式 nil 判断、直接 `return h.mirasimQuotaProbe`，
// 上面两条**仍然全绿**（`&AccountHandler{}` 的字段是 nil，返回的接口值却带类型，
// 而 require.Nil 用反射判空，恰好会放过它）。真正会坏的是端点：`prober != nil`
// 成立 → 在 nil 接收者上调 ProbeAccount → service 的 nil 守卫回
// ErrMirasimQuotaProbeUnavailable → 端点从「诚实回显旧读数」退化成「报一个假的
// 探测失败」。所以这里直接按**接口值**判，不用 require.Nil。
func TestMirasimQuotaProberDoesNotLeakATypedNil(t *testing.T) {
	h := &AccountHandler{}
	prober := h.mirasimQuotaProber()
	require.True(t, prober == nil,
		"mirasimQuotaProber() 泄漏了一个带类型的 nil 指针：`prober != nil` 会成立，"+
			"端点会去 nil 接收者上探测，把「没接线」错报成「探测失败」")
}

// TestProbeMirasimQuotaEndpointRejectsNonMirasimAccounts: "this account has no
// quota dimension" is a different answer from "this account's quota is unknown",
// and the console acts differently on them, so the endpoint refuses instead of
// answering with an empty snapshot. The reason code is the probe service's own,
// so this condition has one name wherever it is raised.
func TestProbeMirasimQuotaEndpointRejectsNonMirasimAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	stub.getAccountResult = &service.Account{
		ID:          9,
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
	}
	handler := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts/:id/mirasim-quota", handler.ProbeMirasimQuota)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/9/mirasim-quota", nil))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "MIRASIM_QUOTA_PROBE_ACCOUNT_INVALID", body.Reason)
}

// TestMirasimQuotaProbeExtraKeyIsPinned freezes the accounts.extra key, VALUE
// and all. Producer and reader share the constant, so a rename of the identifier
// is a compile error — but changing the STRING is not: it would orphan every
// snapshot already written to the database, and every affected account would
// simply go back to looking unprobed.
func TestMirasimQuotaProbeExtraKeyIsPinned(t *testing.T) {
	require.Equal(t, "mirasim_quota_probe", mirasim.ExtraQuotaProbe)
}
