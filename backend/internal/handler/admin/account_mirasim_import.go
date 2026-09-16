package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim/ccore"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Bulk import of mirasim accounts.
//
// A mirasim account is NOT a new platform: it is an ordinary anthropic apikey
// passthrough account whose credentials carry provider="mirasim" plus a
// persistent ed25519 device seed (see internal/pkg/mirasim/doc.go). The seed is
// the whole identity — the upstream binds the derived device id to the account
// at /v1/device/session — and a corrupted seed does not fail loudly: it produces
// a well-formed signature from the wrong key, which the relay answers with 403.
// A 403 reads like a banned account, so a silent one-byte seed corruption during
// a 165-account migration is close to unattributable after the fact.
//
// Hence the rule this importer enforces: EVERY account is signature-verified
// before it is reported as imported. Not a sample — verification is per-seed and
// independent, so a passing sample says nothing about the rest of the batch.

const (
	// mirasimImportDefaultRelayBase is the relay the account talks to when the
	// source record names none.
	mirasimImportDefaultRelayBase = "https://relay.mirasim.ai"
	// mirasimImportAPIKeyPlaceholder fills credentials.api_key. The mirasim
	// upstream decorator replaces the auth header with a device signature, so
	// the stored key is never sent; it exists because the anthropic apikey
	// account shape expects the field.
	mirasimImportAPIKeyPlaceholder = "mirasim-signed"
	// mirasimImportNamePrefix is the fallback account name stem.
	mirasimImportNamePrefix = "mirasim"
	// mirasimImportMaxEntries caps one request. The real migration is ~165
	// accounts; the cap only stops a runaway payload.
	mirasimImportMaxEntries = 2000
)

// MirasimAccountImportRequest is the admin payload: one or more raw JSON
// documents describing mirasim accounts, plus the per-batch account knobs.
type MirasimAccountImportRequest struct {
	Content                 string         `json:"content"`
	Contents                []string       `json:"contents"`
	Name                    string         `json:"name"`
	Notes                   *string        `json:"notes"`
	GroupIDs                []int64        `json:"group_ids"`
	ProxyID                 *int64         `json:"proxy_id"`
	Concurrency             *int           `json:"concurrency"`
	Priority                *int           `json:"priority"`
	RateMultiplier          *float64       `json:"rate_multiplier"`
	LoadFactor              *int           `json:"load_factor"`
	ExpiresAt               *int64         `json:"expires_at"`
	AutoPauseOnExpired      *bool          `json:"auto_pause_on_expired"`
	RelayBase               string         `json:"relay_base"`
	AuthBase                string         `json:"auth_base"`
	CredentialExtras        map[string]any `json:"credential_extras"`
	Extra                   map[string]any `json:"extra"`
	UpdateExisting          *bool          `json:"update_existing"`
	SkipDefaultGroupBind    *bool          `json:"skip_default_group_bind"`
	ConfirmMixedChannelRisk *bool          `json:"confirm_mixed_channel_risk"`
	// CreateMissingProxy lets a per-account proxy URL in the source data create
	// the proxy record it names. Off by default: creating proxy rows is a side
	// effect an operator should opt into.
	CreateMissingProxy *bool `json:"create_missing_proxy"`
	// Selection 是运营者的导入筛选规则。nil 表示不筛选:来源里有几条就导几条,
	// 与筛选层加入之前逐字节相同的行为。用指针而不是值,正是为了让「没传」和
	// 「传了一份全零策略」区分得开 —— 后者是一次明确的「我要筛,但这批不设
	// 上界」,前者是老调用方,两者不该塌成同一个判断。
	Selection *MirasimImportSelectionPolicy `json:"selection,omitempty"`
}

// MirasimAccountImportResult mirrors the codex importer's counters and adds
// Verified: how many imported accounts passed the per-account signature check.
// Verified MUST equal Created+Updated — anything else means an account was
// written without its identity being proven.
//
// Total 始终是「来源总数」,不是「尝试导入数」。加入筛选层之后账目守恒式变成:
//
//	Selected + Reserved == Total
//	Created + Updated + Skipped + Failed + Reserved == Total
//
// 两条都必须成立。差额不是四舍五入,而是有账号既没被导入也没被有意保留 ——
// 它在中间蒸发了,而蒸发的账号没人会去找,因为没有任何一栏显示它少了。
type MirasimAccountImportResult struct {
	Total int `json:"total"`
	// Selected / Reserved 在没传 selection 时也如实填(Selected==Total、
	// Reserved==0),不加 omitempty:让 "0" 在两种含义间摇摆,恰好会毁掉上面
	// 那条守恒式的可读性。
	Selected  int                           `json:"selected"`
	Reserved  int                           `json:"reserved"`
	Created   int                           `json:"created"`
	Updated   int                           `json:"updated"`
	Skipped   int                           `json:"skipped"`
	Failed    int                           `json:"failed"`
	Verified  int                           `json:"verified"`
	Items     []MirasimAccountImportItem    `json:"items,omitempty"`
	Decisions []mirasimImportDecision       `json:"decisions,omitempty"`
	Warnings  []MirasimAccountImportMessage `json:"warnings,omitempty"`
	Errors    []MirasimAccountImportMessage `json:"errors,omitempty"`
}

// MirasimAccountImportItem is the per-entry outcome. DeviceID is the public
// device identifier derived from the seed (a sha256 of the SPKI public key, not
// a secret) — the operator can diff it against the source system to confirm the
// seed survived the migration.
type MirasimAccountImportItem struct {
	Index     int    `json:"index"`
	Name      string `json:"name,omitempty"`
	Action    string `json:"action"`
	AccountID int64  `json:"account_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	Message   string `json:"message,omitempty"`
}

type MirasimAccountImportMessage struct {
	Index   int    `json:"index"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

// mirasimImportEntry is one source record with its 1-based position in the
// request, kept so every message can point at the offending line.
type mirasimImportEntry struct {
	Index int
	Value any
}

// mirasimImportAccount is a normalized source record. It owns its maps: nothing
// in here aliases the caller's parsed JSON, so the import can never mutate the
// source data it was handed.
type mirasimImportAccount struct {
	Name         string
	DeviceSeed   string
	DeviceID     string
	SessionID    string
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
	AuthBase     string
	RelayBase    string
	ProxyURL     string
	ProxyID      *int64
	// Plan* are the source's CLAIMED subscription state. They are labels, not
	// verified facts — see internal/pkg/mirasim/plan.go and the plan probe, which
	// reads the authoritative tier from /auth/referral and never writes over
	// these keys.
	//
	// PlanExpiresAt is the SUBSCRIPTION expiry and has nothing to do with
	// ExpiresAt above, which is when the access token dies.
	Plan          string
	PlanExpiresAt string
	Redeemed      *int64
	Threshold     *int64
	NextPlan      string
	Credentials   map[string]any
	Extra         map[string]any
	Warnings      []string
}

// ImportMirasimAccounts handles POST /admin/accounts/import/mirasim.
func (h *AccountHandler) ImportMirasimAccounts(c *gin.Context) {
	var req MirasimAccountImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Concurrency != nil && *req.Concurrency < 0 {
		response.BadRequest(c, "concurrency must be >= 0")
		return
	}
	if req.Priority != nil && *req.Priority < 0 {
		response.BadRequest(c, "priority must be >= 0")
		return
	}
	if req.RateMultiplier != nil && *req.RateMultiplier < 0 {
		response.BadRequest(c, "rate_multiplier must be >= 0")
		return
	}
	if req.LoadFactor != nil && *req.LoadFactor > 10000 {
		response.BadRequest(c, "load_factor must be <= 10000")
		return
	}
	if req.Selection != nil {
		if err := req.Selection.validate(); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
	}

	entries, err := parseMirasimImportEntries(req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if len(entries) == 0 {
		response.BadRequest(c, "请输入 mirasim 账号 JSON")
		return
	}

	executeAdminIdempotentJSON(c, "admin.accounts.import_mirasim", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.importMirasimAccounts(ctx, req, entries)
	})
}

// importMirasimAccounts is the batch body. It never returns "imported" for an
// account whose device seed failed verification.
func (h *AccountHandler) importMirasimAccounts(ctx context.Context, req MirasimAccountImportRequest, entries []mirasimImportEntry) (MirasimAccountImportResult, error) {
	result := MirasimAccountImportResult{
		Total:    len(entries),
		Selected: len(entries),
		Items:    make([]MirasimAccountImportItem, 0, len(entries)),
	}

	// 筛选先于一切写操作,也先于读库:被保留的账号不该在日志、缓存或任何一次
	// 查询里留下「差点被导入」的痕迹。
	//
	// req.Selection == nil 时整层不介入,entries 原样往下走 —— 这是老调用方的
	// 行为保证,不是优化。
	if req.Selection != nil {
		selected, reserved, decisions := selectMirasimImportCandidates(entries, *req.Selection)
		entries = selected
		result.Selected = len(selected)
		result.Reserved = len(reserved)
		result.Decisions = decisions
	}

	existingAccounts, err := h.listAccountsFiltered(ctx, service.PlatformAnthropic, service.AccountTypeAPIKey, "", "", 0, "", "created_at", "desc")
	if err != nil {
		return result, err
	}
	index := buildMirasimAccountIndex(existingAccounts)

	updateExisting := true
	if req.UpdateExisting != nil {
		updateExisting = *req.UpdateExisting
	}
	concurrency := 3
	if req.Concurrency != nil {
		concurrency = *req.Concurrency
	}
	priority := 50
	if req.Priority != nil {
		priority = *req.Priority
	}
	skipDefaultGroupBind := false
	if req.SkipDefaultGroupBind != nil {
		skipDefaultGroupBind = *req.SkipDefaultGroupBind
	}
	skipMixedChannelCheck := req.ConfirmMixedChannelRisk != nil && *req.ConfirmMixedChannelRisk
	proxies := &mirasimProxyResolver{
		svc:    h.adminService,
		create: req.CreateMissingProxy != nil && *req.CreateMissingProxy,
	}

	seen := map[string]int{}
	for _, entry := range entries {
		item, normErr := normalizeMirasimImportEntry(entry, req)
		if normErr != nil {
			result.recordFailure(entry.Index, "", normErr.Error())
			continue
		}
		name := item.Name

		// Per-account signature verification. Runs for EVERY entry, before any
		// write, and a failure here is a hard failure: an unverified seed that
		// reaches the database becomes an unexplained 403 later.
		deviceID, verifyErr := verifyMirasimDeviceIdentity(item.DeviceSeed)
		if verifyErr != nil {
			result.recordFailure(entry.Index, name, verifyErr.Error())
			continue
		}
		item.DeviceID = deviceID
		item.Credentials[mirasim.CredDeviceSeed] = item.DeviceSeed

		if firstIndex, dup := seen[deviceID]; dup {
			message := fmt.Sprintf("与第 %d 条导入项共用同一台设备，已跳过", firstIndex)
			result.Skipped++
			result.Items = append(result.Items, MirasimAccountImportItem{
				Index:    entry.Index,
				Name:     name,
				Action:   "skipped",
				DeviceID: deviceID,
				Message:  message,
			})
			result.Warnings = append(result.Warnings, MirasimAccountImportMessage{Index: entry.Index, Name: name, Message: message})
			continue
		}
		seen[deviceID] = entry.Index

		proxyID, proxyErr := proxies.resolve(ctx, item)
		if proxyErr != nil {
			result.recordFailure(entry.Index, name, proxyErr.Error())
			continue
		}
		if proxyID == nil {
			proxyID = req.ProxyID
		}

		existing := index.find(deviceID)
		if existing != nil && !updateExisting {
			message := "账号已存在，未开启覆盖更新，已跳过"
			result.Skipped++
			result.Items = append(result.Items, MirasimAccountImportItem{
				Index:     entry.Index,
				Name:      name,
				Action:    "skipped",
				AccountID: existing.ID,
				DeviceID:  deviceID,
				Message:   message,
			})
			result.Warnings = append(result.Warnings, MirasimAccountImportMessage{Index: entry.Index, Name: name, Message: message})
			continue
		}

		for _, warning := range item.Warnings {
			result.Warnings = append(result.Warnings, MirasimAccountImportMessage{Index: entry.Index, Name: name, Message: warning})
		}

		if existing != nil {
			updateInput := &service.UpdateAccountInput{
				Credentials:        mergeMirasimImportCredentials(existing.Credentials, item.Credentials),
				Extra:              mergeMirasimImportMap(existing.Extra, item.Extra),
				Concurrency:        req.Concurrency,
				Priority:           req.Priority,
				RateMultiplier:     req.RateMultiplier,
				LoadFactor:         req.LoadFactor,
				ExpiresAt:          req.ExpiresAt,
				AutoPauseOnExpired: req.AutoPauseOnExpired,
			}
			if proxyID != nil {
				updateInput.ProxyID = proxyID
			}
			if len(req.GroupIDs) > 0 {
				groupIDs := append([]int64(nil), req.GroupIDs...)
				updateInput.GroupIDs = &groupIDs
				updateInput.SkipMixedChannelCheck = skipMixedChannelCheck
			}
			updated, updateErr := h.adminService.UpdateAccount(ctx, existing.ID, updateInput)
			if updateErr != nil {
				result.recordFailure(entry.Index, name, updateErr.Error())
				continue
			}
			if h.tokenCacheInvalidator != nil && updated != nil {
				_ = h.tokenCacheInvalidator.InvalidateToken(ctx, updated)
			}
			accountID := existing.ID
			if updated != nil {
				accountID = updated.ID
				index.add(*updated)
			}
			result.Updated++
			result.Verified++
			result.Items = append(result.Items, MirasimAccountImportItem{
				Index:     entry.Index,
				Name:      name,
				Action:    "updated",
				AccountID: accountID,
				DeviceID:  deviceID,
			})
			continue
		}

		account, createErr := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
			Name:                  name,
			Notes:                 req.Notes,
			Platform:              service.PlatformAnthropic,
			Type:                  service.AccountTypeAPIKey,
			Credentials:           item.Credentials,
			Extra:                 item.Extra,
			ProxyID:               proxyID,
			Concurrency:           concurrency,
			Priority:              priority,
			RateMultiplier:        req.RateMultiplier,
			LoadFactor:            req.LoadFactor,
			GroupIDs:              req.GroupIDs,
			ExpiresAt:             req.ExpiresAt,
			AutoPauseOnExpired:    req.AutoPauseOnExpired,
			SkipDefaultGroupBind:  skipDefaultGroupBind,
			SkipMixedChannelCheck: skipMixedChannelCheck,
		})
		if createErr != nil {
			result.recordFailure(entry.Index, name, createErr.Error())
			continue
		}
		accountID := int64(0)
		if account != nil {
			accountID = account.ID
			index.add(*account)
		}
		result.Created++
		result.Verified++
		result.Items = append(result.Items, MirasimAccountImportItem{
			Index:     entry.Index,
			Name:      name,
			Action:    "created",
			AccountID: accountID,
			DeviceID:  deviceID,
		})
	}

	return result, nil
}

func (r *MirasimAccountImportResult) recordFailure(index int, name, message string) {
	r.Failed++
	r.Items = append(r.Items, MirasimAccountImportItem{
		Index:   index,
		Name:    name,
		Action:  "failed",
		Message: message,
	})
	r.Errors = append(r.Errors, MirasimAccountImportMessage{Index: index, Name: name, Message: message})
}

// ─────────────────────────── 导入筛选层 ───────────────────────────
//
// 143 个 mirasim 账号最初是用一段一次性 shell 脚本从来源清单里筛出来的。规则
// 只活在那段脚本里:它不在这个仓库,没有测试,也没人能在事后证明「再跑一次选
// 中的还是同一批」。下面这一层就是把那段脚本固化成可测的纯函数 —— 筛选是运营
// 决定,不是脚本的私人知识。
//
// 四条规则分两级,优先级不是风格问题而是权责问题:
//
//	ForceSkip > ForceKeep > SkipDisabled / SkipExpiringAfter
//
// 后两条是「默认值」:它们对来源数据做一般性判断。前两条是运营者对具体账号下
// 的决定。让默认值压过决定,等于把运营者的决定权交还给一条启发式规则 —— 那条
// 规则恰恰是因为会误判才需要被点名覆盖的。
//
// ForceSkip 又必须压过 ForceKeep:两份名单同时命中时,「别动这个号」是安全侧
// 的意思。把冲突解释成「导入」会造成不可回退的后果(号被投入生产、被用掉);
// 解释成「保留」最坏只是少导一个,运营者在 decisions 里一眼能看到并改名单。

const (
	// mirasimImportReasonDefault:没有任何规则命中,按默认导入。
	mirasimImportReasonDefault = "default"
	// mirasimImportReasonForceKeep:运营者点名要导。
	mirasimImportReasonForceKeep = "force_keep"
	// mirasimImportReasonForceSkip:运营者点名不导,压过其余一切。
	mirasimImportReasonForceSkip = "force_skip"
	// mirasimImportReasonDisabled:来源里标记为 disabled。
	mirasimImportReasonDisabled = "source_disabled"
	// mirasimImportReasonExpiryReserved:订阅到期晚于上界(运营者自用的一年期号)。
	mirasimImportReasonExpiryReserved = "reserved_expiry"
	// mirasimImportReasonExpiryUnreadable:来源确实带了订阅到期字段,但读不出时间。
	mirasimImportReasonExpiryUnreadable = "reserved_expiry_unreadable"
	// mirasimImportReasonPolicyUnreadable:策略自身的 cutoff 解析不了。
	mirasimImportReasonPolicyUnreadable = "policy_cutoff_unreadable"
)

// MirasimImportSelectionPolicy 是运营者的导入筛选规则。
type MirasimImportSelectionPolicy struct {
	// SkipDisabled:来源里标记为 disabled 的账号不导入。
	SkipDisabled bool `json:"skip_disabled"`
	// SkipExpiringAfter 是订阅到期时间的上界(RFC3339)。到期晚于它的一律保留,
	// 运营者自用的一年期号(2027 到期)靠这条挡住。
	//
	// 它读的是 plan_expires_at(订阅到期),不是 credentials.expires_at(access
	// token 到期)。两者差着几个数量级:token 几小时后就死,永远早于任何合理的
	// cutoff。拿 token 到期做判据,这条规则一个号都拦不住,而且是静默失效 ——
	// 计数看起来很正常,只是运营者的自用号全被导进了生产池。
	SkipExpiringAfter string `json:"skip_expiring_after,omitempty"`
	// ForceKeep / ForceSkip 按邮箱或账号标识匹配(大小写不敏感)。
	ForceKeep []string `json:"force_keep,omitempty"`
	ForceSkip []string `json:"force_skip,omitempty"`
}

// validate 在 HTTP 边界上拦下写坏的策略。放在这里而不是纯函数里,是因为纯函数
// 的签名不返回 error:一个解析不了的 cutoff 在纯函数里只能选择「静默失效」或
// 「全部保留」,两者都不如当场 400 告诉运营者他打错了。
func (p MirasimImportSelectionPolicy) validate() error {
	if strings.TrimSpace(p.SkipExpiringAfter) == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(p.SkipExpiringAfter)); err != nil {
		return fmt.Errorf("selection.skip_expiring_after 必须是 RFC3339 时间（例如 2026-12-31T23:59:59Z）")
	}
	return nil
}

// mirasimImportDecision 记录一条来源记录的去向以及「因为哪条规则」。
// 没有 Reason 的筛选结果等于没有筛选结果:运营者要复核的恰恰是「哪几个命中规则
// 又被点名覆盖了」,只给两个计数无法复核。
type mirasimImportDecision struct {
	Index    int    `json:"index"`
	Name     string `json:"name,omitempty"`
	Selected bool   `json:"selected"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
}

// mirasimImportEntryFacts 是筛选层需要、且只有筛选层需要的来源事实。
type mirasimImportEntryFacts struct {
	// Label 是给人看的标识,只从 name/label/email/account_name 取,不碰任何凭据。
	Label string
	// Identifiers 是这条记录可以被点名的全部写法。
	Identifiers []string
	Disabled    bool
	// PlanExpiresAt 仅在 PlanExpiryKnown 时有意义。
	PlanExpiresAt   time.Time
	PlanExpiryState int
}

const (
	// mirasimPlanExpiryAbsent:来源根本没提订阅到期。「没说」不是「说了读不懂」,
	// 两者必须分开:大量来源记录本来就不带这个字段,把它们全按不可读保留,等于
	// 这条规则从「挡住 2027 的号」变成「挡住绝大多数号」。
	mirasimPlanExpiryAbsent = iota
	mirasimPlanExpiryKnown
	// mirasimPlanExpiryUnreadable:字段在,但解析不出时间。
	mirasimPlanExpiryUnreadable
)

// mirasimImportSelector 是编译好的策略。把名单预先折叠成集合,是为了让每条
// entry 的判定与 entry 的顺序、与名单长度都无关。
type mirasimImportSelector struct {
	skipDisabled bool
	forceKeep    map[string]struct{}
	forceSkip    map[string]struct{}
	cutoff       time.Time
	cutoffSet    bool
	cutoffBroken bool
}

func newMirasimImportSelector(policy MirasimImportSelectionPolicy) mirasimImportSelector {
	selector := mirasimImportSelector{
		skipDisabled: policy.SkipDisabled,
		forceKeep:    mirasimImportNameSet(policy.ForceKeep),
		forceSkip:    mirasimImportNameSet(policy.ForceSkip),
	}
	raw := strings.TrimSpace(policy.SkipExpiringAfter)
	if raw == "" {
		return selector
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// 防御性 fail-closed。validate() 让这条在 HTTP 路径上不可达,但纯函数还
		// 有别的调用方;真走到这里就整批保留(点名的除外),因为「一个号都没导」
		// 是运营者立刻会发现的现象,而「规则悄悄失效、自用号进了生产池」不是。
		selector.cutoffBroken = true
		return selector
	}
	selector.cutoff = parsed
	selector.cutoffSet = true
	return selector
}

// selectMirasimImportCandidates 是筛选层的全部入口:纯函数,不读时钟、不碰
// 网络、不改 entries。
//
// 每条 entry 恰好落进 selected 或 reserved 之一 —— 这一点是结构保证的,不是靠
// 自觉:classify 返回唯一一个 decision,循环里只有一处 append 到 selected /
// reserved 的分支。想让一条 entry 蒸发,必须先把这个循环改成多出口。
func selectMirasimImportCandidates(entries []mirasimImportEntry, policy MirasimImportSelectionPolicy) (selected, reserved []mirasimImportEntry, decisions []mirasimImportDecision) {
	selector := newMirasimImportSelector(policy)
	decisions = make([]mirasimImportDecision, 0, len(entries))
	for _, entry := range entries {
		facts := mirasimImportFactsOf(entry)
		decision := selector.classify(facts)
		decision.Index = entry.Index
		decision.Name = facts.Label
		decisions = append(decisions, decision)
		if decision.Selected {
			selected = append(selected, entry)
		} else {
			reserved = append(reserved, entry)
		}
	}
	return selected, reserved, decisions
}

// classify 实现优先级 ForceSkip > ForceKeep > SkipDisabled/SkipExpiringAfter。
// 顺序即语义:调换任意两个 if 都会改变运营者的决定权归属。
func (s mirasimImportSelector) classify(facts mirasimImportEntryFacts) mirasimImportDecision {
	if hit, matched := mirasimImportNamed(s.forceSkip, facts.Identifiers); hit {
		return mirasimImportDecision{Selected: false, Reason: mirasimImportReasonForceSkip, Detail: matched}
	}
	if hit, matched := mirasimImportNamed(s.forceKeep, facts.Identifiers); hit {
		return mirasimImportDecision{Selected: true, Reason: mirasimImportReasonForceKeep, Detail: matched}
	}
	if s.cutoffBroken {
		return mirasimImportDecision{Selected: false, Reason: mirasimImportReasonPolicyUnreadable}
	}
	if s.skipDisabled && facts.Disabled {
		return mirasimImportDecision{Selected: false, Reason: mirasimImportReasonDisabled}
	}
	if s.cutoffSet {
		switch facts.PlanExpiryState {
		case mirasimPlanExpiryKnown:
			if facts.PlanExpiresAt.After(s.cutoff) {
				return mirasimImportDecision{
					Selected: false,
					Reason:   mirasimImportReasonExpiryReserved,
					Detail:   facts.PlanExpiresAt.UTC().Format(time.RFC3339),
				}
			}
		case mirasimPlanExpiryUnreadable:
			// 来源确实声明了一个订阅到期,只是这一份读不出来。读不出的声明不能
			// 当成「没有声明」:它有可能正是那个 2027 的自用号。
			return mirasimImportDecision{Selected: false, Reason: mirasimImportReasonExpiryUnreadable}
		}
	}
	return mirasimImportDecision{Selected: true, Reason: mirasimImportReasonDefault}
}

// mirasimImportFactsOf 从原始来源记录里读出筛选所需的事实。它复用
// mirasimImportEntryScopes,与 normalizeMirasimImportEntry 看的是同一批位置 ——
// 否则会出现「筛选按顶层字段判、导入按 credentials 里的字段判」这种两套真相。
func mirasimImportFactsOf(entry mirasimImportEntry) mirasimImportEntryFacts {
	scopes := mirasimImportEntryScopes(entry.Value)
	facts := mirasimImportEntryFacts{
		Label:       mirasimLookupString(scopes, "name", "label", "email", "account_name"),
		Identifiers: mirasimImportIdentifiers(scopes),
		Disabled:    mirasimImportDisabled(scopes),
	}
	planKeys := []string{mirasim.ExtraPlanExpiresAt, "plan_expires_at", "planExpiresAt", "subscription_expires_at"}
	if expires, found := mirasimLookupTime(scopes, planKeys...); found {
		facts.PlanExpiresAt = expires
		facts.PlanExpiryState = mirasimPlanExpiryKnown
	} else if raw := mirasimLookupString(scopes, planKeys...); raw != "" {
		facts.PlanExpiryState = mirasimPlanExpiryUnreadable
	}
	return facts
}

// mirasimImportIdentifiers 收集这条记录可以被点名的全部写法。
//
// 注意它与 mirasimLookupString 的区别:后者取第一个命中就停,那是「这条记录叫
// 什么」;这里要的是「运营者可能用哪个串点它」—— 名单里写的是邮箱,而记录的
// name 可能是别的东西,只取第一个会让点名静默失配。
func mirasimImportIdentifiers(scopes []map[string]any) []string {
	keys := []string{"email", "name", "label", "account_name", "account", "username", "id", "account_id", "uuid"}
	seen := make(map[string]struct{})
	var out []string
	for _, scope := range scopes {
		for _, key := range keys {
			value, ok := scope[key]
			if !ok || value == nil {
				continue
			}
			text := mirasimImportIdentifierText(value)
			if text == "" {
				continue
			}
			folded := strings.ToLower(text)
			if _, dup := seen[folded]; dup {
				continue
			}
			seen[folded] = struct{}{}
			out = append(out, text)
		}
	}
	return out
}

func mirasimImportIdentifierText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		// 账号 id 走 JSON 后是 float64;整数形态的 id 必须还原成 "42" 而不是
		// "42.000000",否则名单里写 42 永远配不上。
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

// mirasimImportDisabled 只认肯定式的禁用标记。
//
// 「读不出状态」绝不等于「被禁用」:把未知当禁用,运营者会在毫无提示的情况下
// 少导一批号,而少导的号不会报错 —— 它只是不在那里。
func mirasimImportDisabled(scopes []map[string]any) bool {
	for _, scope := range scopes {
		for _, key := range []string{"disabled", "is_disabled", "banned", "deleted"} {
			if value, ok := scope[key]; ok && mirasimImportBool(value) == 1 {
				return true
			}
		}
		for _, key := range []string{"enabled", "active", "is_active"} {
			if value, ok := scope[key]; ok && mirasimImportBool(value) == -1 {
				return true
			}
		}
		for _, key := range []string{"status", "state"} {
			text, ok := scope[key].(string)
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "disabled", "inactive", "banned", "suspended":
				return true
			}
		}
	}
	return false
}

// mirasimImportBool 返回 1(真)/ -1(假)/ 0(读不出来)。三态是必要的:把
// 「读不出来」折叠成 false,`enabled` 字段写成一个陌生形态时就会被当成
// enabled=false,从而静默禁用一个正常账号。
func mirasimImportBool(value any) int {
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1
		}
		return -1
	case float64:
		if typed != 0 {
			return 1
		}
		return -1
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			if parsed != 0 {
				return 1
			}
			return -1
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "y", "on":
			return 1
		case "false", "0", "no", "n", "off":
			return -1
		}
	}
	return 0
}

func mirasimImportNameSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.ToLower(strings.TrimSpace(name))
		if trimmed == "" {
			// 空串会匹配上「没有任何标识」的记录,把一次手滑的空行变成一条静默
			// 生效的全局规则。
			continue
		}
		set[trimmed] = struct{}{}
	}
	return set
}

// mirasimImportNamed 返回是否命中,以及命中的是哪个标识 —— 运营者复核名单时需要
// 知道是邮箱配上的还是 id 配上的。
func mirasimImportNamed(set map[string]struct{}, identifiers []string) (bool, string) {
	if len(set) == 0 {
		return false, ""
	}
	for _, identifier := range identifiers {
		if _, ok := set[strings.ToLower(strings.TrimSpace(identifier))]; ok {
			return true, identifier
		}
	}
	return false, ""
}

// verifyMirasimDeviceIdentity proves that THIS seed yields a usable, stable
// device identity, and returns the derived device id.
//
// Three independent legs, because the failure modes differ:
//  1. the seed decodes to a 32-byte ed25519 seed and derives a device id;
//  2. deriving twice from the same seed gives the same device id and public key
//     (a non-deterministic derivation would re-register the device on restart);
//  3. the crypto-core actually signs with it — headers generate, the signature
//     is a full-length ed25519 signature, and signing identical input twice is
//     byte-identical.
//
// Errors never quote the seed: base64/length failures are reported structurally.
//
// What this CANNOT catch, stated plainly: a seed that is still 32 well-formed
// bytes but the wrong 32 bytes. Any 32 bytes are a valid ed25519 seed, so a
// flipped byte simply yields a different — and locally perfectly valid — device.
// Only the upstream can tell that device apart from the account's real one, and
// it does so with a 403. The mitigation is the DeviceID reported per item: it is
// derived, not copied, so an operator can diff the returned ids against the
// source system and see exactly which accounts changed identity in transit.
func verifyMirasimDeviceIdentity(seed string) (string, error) {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return "", fmt.Errorf("缺少 device seed：mirasim 账号没有设备身份无法签名")
	}
	signer, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		return "", fmt.Errorf("device seed 无效（%d 字符），无法派生设备身份", len(seed))
	}
	if signer.DeviceID == "" || signer.PublicKeyB64 == "" {
		return "", fmt.Errorf("device seed 未能派生出 device id")
	}

	again, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		return "", fmt.Errorf("device seed 第二次派生失败，设备身份不稳定")
	}
	if again.DeviceID != signer.DeviceID || again.PublicKeyB64 != signer.PublicKeyB64 {
		return "", fmt.Errorf("device seed 派生结果不稳定：同一 seed 两次派生出不同 device id")
	}

	probeBody := []byte(`{"mirasim":"import-selfcheck"}`)
	headers, err := signer.Headers("POST", "/v1/messages", probeBody, "import-selfcheck", "", time.Unix(0, 0).UTC())
	if err != nil {
		return "", fmt.Errorf("device seed 无法生成签名头：%w", err)
	}
	if headers["x-mirasim-sig"] == "" {
		return "", fmt.Errorf("device seed 生成了空签名")
	}
	if headers["x-mirasim-device"] != signer.DeviceID {
		return "", fmt.Errorf("签名头中的 device id 与派生结果不一致")
	}
	if headers["x-mirasim-client"] != mirasim.ClientVersion {
		return "", fmt.Errorf("签名头中的客户端版本不是 %s", mirasim.ClientVersion)
	}

	// Determinism has to be checked below Headers: Headers draws a fresh nonce
	// per call by design, so two calls legitimately differ.
	canonical := strings.Join([]string{"POST", "/v1/messages", "0", "selfcheck", signer.DeviceID, mirasim.ClientVersion, "import-selfcheck"}, "\x00")
	first, err := ccore.Sign(signer.Seed(), canonical, "", probeBody)
	if err != nil {
		return "", fmt.Errorf("device seed 签名失败：%w", err)
	}
	if len(first) != ed25519.SignatureSize {
		return "", fmt.Errorf("签名长度为 %d 字节，期望 %d", len(first), ed25519.SignatureSize)
	}
	second, err := ccore.Sign(signer.Seed(), canonical, "", probeBody)
	if err != nil {
		return "", fmt.Errorf("device seed 二次签名失败：%w", err)
	}
	if !bytes.Equal(first, second) {
		return "", fmt.Errorf("同一输入两次签名不一致，设备密钥不可用")
	}
	return signer.DeviceID, nil
}

// mirasimAccountIndex maps a derived device id to the account that already owns
// it. The device id is the upstream identity, so it is the only sound dedup key
// — names and tokens both rotate.
type mirasimAccountIndex struct {
	byDeviceID map[string]service.Account
}

func buildMirasimAccountIndex(accounts []service.Account) *mirasimAccountIndex {
	index := &mirasimAccountIndex{byDeviceID: make(map[string]service.Account, len(accounts))}
	for _, account := range accounts {
		index.add(account)
	}
	return index
}

func (i *mirasimAccountIndex) add(account service.Account) {
	if !strings.EqualFold(strings.TrimSpace(mirasimCredentialString(account.Credentials, mirasim.CredProvider)), mirasim.ProviderMirasim) {
		return
	}
	seed := strings.TrimSpace(mirasimCredentialString(account.Credentials, mirasim.CredDeviceSeed))
	if seed == "" {
		return
	}
	signer, err := mirasim.NewDeviceSigner(seed)
	if err != nil {
		// An already-stored broken seed is not this import's problem to fix,
		// but it must not shadow an incoming good account either.
		return
	}
	i.byDeviceID[signer.DeviceID] = account
}

func (i *mirasimAccountIndex) find(deviceID string) *service.Account {
	account, ok := i.byDeviceID[deviceID]
	if !ok {
		return nil
	}
	return &account
}

// mirasimProxyResolver turns a per-account proxy URL into a proxy id, reusing an
// existing proxy row when one matches. Two accounts on the same egress must land
// on the same proxy row — mirasim binds a device to an IP, and duplicating rows
// would silently split one egress into several.
type mirasimProxyResolver struct {
	svc      service.AdminService
	create   bool
	loaded   bool
	existing []service.Proxy
	minted   map[string]int64
}

func (r *mirasimProxyResolver) resolve(ctx context.Context, item *mirasimImportAccount) (*int64, error) {
	if item.ProxyID != nil {
		return item.ProxyID, nil
	}
	raw := strings.TrimSpace(item.ProxyURL)
	if raw == "" {
		return nil, nil
	}
	spec, err := parseMirasimProxyURL(raw)
	if err != nil {
		return nil, err
	}
	key := spec.key()
	if id, ok := r.minted[key]; ok {
		return &id, nil
	}
	if r.svc == nil {
		return nil, fmt.Errorf("无法解析账号代理：代理服务不可用")
	}
	if !r.loaded {
		proxies, listErr := r.svc.GetAllProxies(ctx)
		if listErr != nil {
			return nil, listErr
		}
		r.existing = proxies
		r.loaded = true
	}
	for _, proxy := range r.existing {
		if strings.EqualFold(proxy.Protocol, spec.Protocol) &&
			strings.EqualFold(proxy.Host, spec.Host) &&
			proxy.Port == spec.Port &&
			proxy.Username == spec.Username {
			id := proxy.ID
			r.remember(key, id)
			return &id, nil
		}
	}
	if !r.create {
		return nil, fmt.Errorf("账号指定的代理 %s://%s:%d 不存在；请先建好代理或开启 create_missing_proxy", spec.Protocol, spec.Host, spec.Port)
	}
	created, createErr := r.svc.CreateProxy(ctx, &service.CreateProxyInput{
		Name:     fmt.Sprintf("%s-%s-%d", mirasimImportNamePrefix, spec.Host, spec.Port),
		Protocol: spec.Protocol,
		Host:     spec.Host,
		Port:     spec.Port,
		Username: spec.Username,
		Password: spec.Password,
	})
	if createErr != nil {
		return nil, createErr
	}
	if created == nil {
		return nil, fmt.Errorf("创建代理 %s:%d 未返回结果", spec.Host, spec.Port)
	}
	r.existing = append(r.existing, *created)
	id := created.ID
	r.remember(key, id)
	return &id, nil
}

func (r *mirasimProxyResolver) remember(key string, id int64) {
	if r.minted == nil {
		r.minted = map[string]int64{}
	}
	r.minted[key] = id
}

type mirasimProxySpec struct {
	Protocol string
	Host     string
	Port     int
	Username string
	Password string
}

// key identifies an egress. The password is deliberately excluded: it is a
// secret and two rows differing only by a rotated password are the same egress.
func (s mirasimProxySpec) key() string {
	return strings.ToLower(s.Protocol) + "://" + strings.ToLower(s.Username) + "@" + strings.ToLower(s.Host) + ":" + strconv.Itoa(s.Port)
}

func parseMirasimProxyURL(raw string) (mirasimProxySpec, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return mirasimProxySpec{}, fmt.Errorf("代理地址无法解析")
	}
	protocol := strings.ToLower(parsed.Scheme)
	switch protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return mirasimProxySpec{}, fmt.Errorf("不支持的代理协议 %q，仅支持 http/https/socks5/socks5h", protocol)
	}
	host, portText, splitErr := net.SplitHostPort(parsed.Host)
	if splitErr != nil {
		return mirasimProxySpec{}, fmt.Errorf("代理地址缺少端口")
	}
	port, portErr := strconv.Atoi(portText)
	if portErr != nil || port <= 0 || port > 65535 {
		return mirasimProxySpec{}, fmt.Errorf("代理端口无效")
	}
	spec := mirasimProxySpec{Protocol: protocol, Host: host, Port: port}
	if parsed.User != nil {
		spec.Username = parsed.User.Username()
		spec.Password, _ = parsed.User.Password()
	}
	return spec, nil
}

// parseMirasimImportEntries splits the request payload into per-account source
// records. It accepts a JSON array, a JSON object, an object wrapping an
// "accounts"/"items"/"data" array, and JSON Lines.
//
// It reads req only; the returned entries hold freshly decoded values, so
// nothing downstream can write through into the caller's request.
func parseMirasimImportEntries(req MirasimAccountImportRequest) ([]mirasimImportEntry, error) {
	contents := make([]string, 0, 1+len(req.Contents))
	if strings.TrimSpace(req.Content) != "" {
		contents = append(contents, req.Content)
	}
	for _, content := range req.Contents {
		if strings.TrimSpace(content) != "" {
			contents = append(contents, content)
		}
	}

	var entries []mirasimImportEntry
	for _, content := range contents {
		values, err := parseMirasimImportContent(content)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			entries = append(entries, mirasimImportEntry{Index: len(entries) + 1, Value: value})
			if len(entries) > mirasimImportMaxEntries {
				return nil, fmt.Errorf("单次导入最多 %d 个账号", mirasimImportMaxEntries)
			}
		}
	}
	return entries, nil
}

func parseMirasimImportContent(content string) ([]any, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, nil
	}
	var whole any
	if err := json.Unmarshal([]byte(trimmed), &whole); err == nil {
		return flattenMirasimImportValue(whole), nil
	}
	// JSON Lines: one account per line.
	var values []any
	for lineNo, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, ","))
		if line == "" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return nil, fmt.Errorf("第 %d 行不是合法 JSON", lineNo+1)
		}
		values = append(values, flattenMirasimImportValue(value)...)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("导入内容不是合法的 mirasim 账号 JSON")
	}
	return values, nil
}

func flattenMirasimImportValue(value any) []any {
	switch typed := value.(type) {
	case []any:
		var out []any
		for _, element := range typed {
			out = append(out, flattenMirasimImportValue(element)...)
		}
		return out
	case map[string]any:
		for _, key := range []string{"accounts", "items", "data", "list"} {
			if nested, ok := typed[key].([]any); ok {
				var out []any
				for _, element := range nested {
					out = append(out, flattenMirasimImportValue(element)...)
				}
				return out
			}
		}
		return []any{typed}
	default:
		return []any{typed}
	}
}

// mirasimImportEntryScopes lists the maps a lookup may read, outermost first.
//
// Sources differ on nesting: some export flat, some wrap secrets in
// "credentials" and the session id in "extra". Nothing here is mutated, and the
// SELECTION layer deliberately shares this helper with the normalizer — two
// different scope lists would mean the filter judged an account on one set of
// fields while the import wrote another.
func mirasimImportEntryScopes(value any) []map[string]any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	scopes := []map[string]any{object}
	for _, key := range []string{"credentials", "credential", "extra", "metadata"} {
		if nested, nestedOK := object[key].(map[string]any); nestedOK {
			scopes = append(scopes, nested)
		}
	}
	return scopes
}

// normalizeMirasimImportEntry turns one source record into the account shape
// sub2api stores. Every map it returns is newly allocated: the source record is
// read, never written.
func normalizeMirasimImportEntry(entry mirasimImportEntry, req MirasimAccountImportRequest) (*mirasimImportAccount, error) {
	if _, ok := entry.Value.(map[string]any); !ok {
		return nil, fmt.Errorf("第 %d 条不是 JSON 对象；mirasim 账号至少需要 device seed 与 token", entry.Index)
	}
	scopes := mirasimImportEntryScopes(entry.Value)

	item := &mirasimImportAccount{
		DeviceSeed:   mirasimLookupString(scopes, mirasim.CredDeviceSeed, "device_seed", "deviceSeed", "seed"),
		SessionID:    mirasimLookupString(scopes, mirasim.ExtraSessionID, "session_id", "sessionId", "session"),
		AccessToken:  mirasimLookupString(scopes, mirasim.CredAccessToken, "accessToken", "token"),
		RefreshToken: mirasimLookupString(scopes, mirasim.CredRefreshToken, "refreshToken"),
		AuthBase:     mirasimLookupString(scopes, mirasim.CredAuthBase, "auth_base", "authBase"),
		RelayBase:    mirasimLookupString(scopes, "base_url", "relay_base", "baseUrl", "relayBase", "relay"),
		ProxyURL:     mirasimLookupString(scopes, "proxy", "proxy_url", "proxyUrl"),
		Name:         mirasimLookupString(scopes, "name", "label", "email", "account_name"),
		// The mirasim.Extra* aliases come first so a sub2api export round-trips:
		// re-importing an account this system already wrote finds its own keys.
		Plan:     strings.ToLower(mirasimLookupString(scopes, mirasim.ExtraPlanClaimed, "plan", "tier", "subscription")),
		NextPlan: strings.ToLower(mirasimLookupString(scopes, mirasim.ExtraPlanNext, "next_plan", "nextPlan")),
	}
	if item.DeviceSeed == "" {
		return nil, fmt.Errorf("第 %d 条缺少 device seed（mirasim_device_seed）", entry.Index)
	}
	if item.AccessToken == "" && item.RefreshToken == "" {
		return nil, fmt.Errorf("第 %d 条缺少 access_token / refresh_token", entry.Index)
	}
	if proxyID, found := mirasimLookupInt64(scopes, "proxy_id", "proxyId"); found {
		item.ProxyID = &proxyID
	}
	if expires, found := mirasimLookupTime(scopes, mirasim.CredExpiresAt, "expiresAt", "expires", "expiry"); found {
		item.ExpiresAt = &expires
	} else if raw := mirasimLookupString(scopes, mirasim.CredExpiresAt, "expiresAt", "expires", "expiry"); raw != "" {
		item.Warnings = append(item.Warnings, "expires_at 无法解析，已忽略；令牌将在首次使用时按需刷新")
	}

	// Subscription expiry. Looked up under its OWN key set — "plan_expires_at"
	// never overlaps "expires_at", because conflating a months-away subscription
	// end with an hour-away token end would either pin a dead token as fresh or
	// report the whole pool as expiring today.
	if planExpires, found := mirasimLookupTime(scopes, mirasim.ExtraPlanExpiresAt, "plan_expires_at", "planExpiresAt", "subscription_expires_at"); found {
		// RFC3339Nano round-trips the source form ("2027-09-10T19:20:40.460323Z")
		// unchanged while still normalising unix timestamps into one shape.
		item.PlanExpiresAt = planExpires.UTC().Format(time.RFC3339Nano)
	} else if raw := mirasimLookupString(scopes, mirasim.ExtraPlanExpiresAt, "plan_expires_at", "planExpiresAt", "subscription_expires_at"); raw != "" {
		// Keep the unparsable original rather than dropping it: an operator can
		// still read it, and the plan probe will replace it with an authoritative
		// value on its next cycle.
		item.PlanExpiresAt = raw
		item.Warnings = append(item.Warnings, "plan_expires_at 无法解析为时间，已按原样保留")
	}
	if redeemed, found := mirasimLookupInt64(scopes, mirasim.ExtraPlanRedeemed, "redeemed", "invites_redeemed"); found {
		item.Redeemed = &redeemed
	}
	if threshold, found := mirasimLookupInt64(scopes, mirasim.ExtraPlanThreshold, "threshold", "invite_threshold"); found {
		item.Threshold = &threshold
	}

	if item.RelayBase == "" {
		item.RelayBase = strings.TrimSpace(req.RelayBase)
	}
	if item.RelayBase == "" {
		item.RelayBase = mirasimImportDefaultRelayBase
	}
	if item.AuthBase == "" {
		item.AuthBase = strings.TrimSpace(req.AuthBase)
	}
	if item.Name == "" {
		item.Name = buildMirasimImportAccountName(req.Name, entry.Index)
	}
	if item.RefreshToken == "" {
		item.Warnings = append(item.Warnings, "该账号没有 refresh_token，access_token 过期后无法自动续期")
	}

	credentials := map[string]any{
		mirasim.CredProvider: mirasim.ProviderMirasim,
		"api_key":            mirasimImportAPIKeyPlaceholder,
		"base_url":           item.RelayBase,
	}
	// The seed is attached by the caller only after verification, so an
	// unverified seed can never reach a write.
	if item.AccessToken != "" {
		credentials[mirasim.CredAccessToken] = item.AccessToken
	}
	if item.RefreshToken != "" {
		credentials[mirasim.CredRefreshToken] = item.RefreshToken
	}
	if item.ExpiresAt != nil {
		credentials[mirasim.CredExpiresAt] = item.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if item.AuthBase != "" {
		credentials[mirasim.CredAuthBase] = item.AuthBase
	}
	item.Credentials = mergeMirasimImportMap(credentials, req.CredentialExtras)

	// anthropic_passthrough is required, not cosmetic: mirasim signs the exact
	// request bytes, so any body rewrite between signing and sending invalidates
	// the signature.
	extra := map[string]any{"anthropic_passthrough": true}
	if item.SessionID != "" {
		extra[mirasim.ExtraSessionID] = item.SessionID
	}
	// Claimed subscription state. Written under the mirasim.ExtraPlan* keys so
	// the operator console can filter on plan without a probe having run yet, and
	// so a re-import of the same source file backfills accounts that predate this
	// code (update_existing merges these in over the stored extra).
	//
	// Absent fields are NOT written as zero values: a source record that simply
	// does not carry `redeemed` must not be recorded as "0 invitations redeemed",
	// which reads as a fact and is not one.
	if item.Plan != "" {
		extra[mirasim.ExtraPlanClaimed] = item.Plan
	}
	if item.PlanExpiresAt != "" {
		extra[mirasim.ExtraPlanExpiresAt] = item.PlanExpiresAt
	}
	if item.Redeemed != nil {
		extra[mirasim.ExtraPlanRedeemed] = *item.Redeemed
	}
	if item.Threshold != nil {
		extra[mirasim.ExtraPlanThreshold] = *item.Threshold
	}
	if item.NextPlan != "" {
		extra[mirasim.ExtraPlanNext] = item.NextPlan
	}
	item.Extra = mergeMirasimImportMap(extra, req.Extra)
	return item, nil
}

func buildMirasimImportAccountName(base string, index int) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = mirasimImportNamePrefix
	}
	return fmt.Sprintf("%s-%d", base, index)
}

// mergeMirasimImportMap returns a NEW map: base values, then incoming overrides.
// Neither argument is modified, which is what keeps the caller's request and the
// stored account state from aliasing each other.
func mergeMirasimImportMap(base, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(incoming))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range incoming {
		out[key] = value
	}
	return out
}

// mergeMirasimImportCredentials layers an incoming record over the stored one.
// Re-importing a record that lost a field must not delete the stored value: an
// export without refresh_token is an incomplete export, not a revocation.
func mergeMirasimImportCredentials(existing, incoming map[string]any) map[string]any {
	out := mergeMirasimImportMap(existing, nil)
	for key, value := range incoming {
		if text, isText := value.(string); isText && strings.TrimSpace(text) == "" {
			continue
		}
		out[key] = value
	}
	for _, key := range []string{mirasim.CredRefreshToken, mirasim.CredAccessToken} {
		if strings.TrimSpace(mirasimCredentialString(out, key)) == "" {
			if stored := mirasimCredentialString(existing, key); stored != "" {
				out[key] = stored
			}
		}
	}
	return out
}

func mirasimCredentialString(credentials map[string]any, key string) string {
	if credentials == nil {
		return ""
	}
	value, ok := credentials[key]
	if !ok || value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func mirasimLookupString(scopes []map[string]any, keys ...string) string {
	for _, scope := range scopes {
		for _, key := range keys {
			value, ok := scope[key]
			if !ok || value == nil {
				continue
			}
			if text, isText := value.(string); isText && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func mirasimLookupInt64(scopes []map[string]any, keys ...string) (int64, bool) {
	for _, scope := range scopes {
		for _, key := range keys {
			value, ok := scope[key]
			if !ok || value == nil {
				continue
			}
			switch typed := value.(type) {
			case float64:
				return int64(typed), true
			case json.Number:
				if parsed, err := typed.Int64(); err == nil {
					return parsed, true
				}
			case string:
				if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
					return parsed, true
				}
			}
		}
	}
	return 0, false
}

// mirasimLookupTime accepts RFC3339, unix seconds and unix milliseconds, because
// each of those appears in real exports.
func mirasimLookupTime(scopes []map[string]any, keys ...string) (time.Time, bool) {
	for _, scope := range scopes {
		for _, key := range keys {
			value, ok := scope[key]
			if !ok || value == nil {
				continue
			}
			switch typed := value.(type) {
			case string:
				text := strings.TrimSpace(typed)
				if text == "" {
					continue
				}
				if parsed, err := time.Parse(time.RFC3339, text); err == nil {
					return parsed, true
				}
				if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
					return mirasimUnixTime(parsed), true
				}
			case float64:
				return mirasimUnixTime(int64(typed)), true
			case json.Number:
				if parsed, err := typed.Int64(); err == nil {
					return mirasimUnixTime(parsed), true
				}
			}
		}
	}
	return time.Time{}, false
}

// mirasimUnixTime disambiguates seconds from milliseconds: anything past ~1e12
// is not a plausible second-precision timestamp for this decade.
func mirasimUnixTime(value int64) time.Time {
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}
