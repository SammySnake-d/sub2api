package service

import "testing"

// 回归来源：2026-09-16 13:56 运维把 model_mapping 的键从裸名改成带日期的
// claude-haiku-4-5-20251001，14:13 客户端发裸名 → 143/143 个 mirasim
// （platform=anthropic + type=apikey）账号被「模型支持」判据剔除 → 404
// model_not_found，14:18 改回裸名才恢复。两种写法必须命中同一条白名单项，
// 且白名单里根本没有的模型无论哪种写法都必须继续被拒。

func anthropicAccountWithMapping(accountType string, mapping map[string]any) *Account {
	return &Account{
		Platform: PlatformAnthropic,
		Type:     accountType,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"model_mapping": mapping,
		},
	}
}

func TestAnthropicModelAliasLookup_BareWhitelistAcceptsDatedRequest(t *testing.T) {
	svc := &GatewayService{}
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		account := anthropicAccountWithMapping(accountType, map[string]any{
			"claude-haiku-4-5": "claude-haiku-4-5",
		})
		if !svc.isModelSupportedByAccount(account, "claude-haiku-4-5-20251001") {
			t.Fatalf("type=%s: dated request must hit a bare whitelist key", accountType)
		}
		if !account.IsModelSupported("claude-haiku-4-5-20251001") {
			t.Fatalf("type=%s: IsModelSupported must accept the dated form", accountType)
		}
	}
}

// 生产上真炸的那一侧：白名单写带日期，客户端发裸名。
func TestAnthropicModelAliasLookup_DatedWhitelistAcceptsBareRequest(t *testing.T) {
	svc := &GatewayService{}
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		account := anthropicAccountWithMapping(accountType, map[string]any{
			"claude-haiku-4-5-20251001": "claude-haiku-4-5-20251001",
		})
		if !svc.isModelSupportedByAccount(account, "claude-haiku-4-5") {
			t.Fatalf("type=%s: bare request must hit a dated whitelist key (2026-09-16 outage)", accountType)
		}
		if !account.IsModelSupported("claude-haiku-4-5") {
			t.Fatalf("type=%s: IsModelSupported must accept the bare form", accountType)
		}
	}
}

// 选号判据与转发路径必须落在同一条 mapping 项上：GetMappedModel 是 apikey 账号
// 转发时真正使用的名字来源。只修选号判据会让这里继续返回请求原名。
func TestAnthropicModelAliasLookup_ForwardPathResolvesSameMappingEntry(t *testing.T) {
	datedWhitelist := anthropicAccountWithMapping(AccountTypeAPIKey, map[string]any{
		"claude-haiku-4-5-20251001": "claude-haiku-4-5-20251001",
	})
	if got := datedWhitelist.GetMappedModel("claude-haiku-4-5"); got != "claude-haiku-4-5-20251001" {
		t.Fatalf("GetMappedModel(bare) = %q, want the dated upstream name", got)
	}

	bareWhitelist := anthropicAccountWithMapping(AccountTypeAPIKey, map[string]any{
		"claude-haiku-4-5": "claude-haiku-4-5",
	})
	if got := bareWhitelist.GetMappedModel("claude-haiku-4-5-20251001"); got != "claude-haiku-4-5" {
		t.Fatalf("GetMappedModel(dated) = %q, want the bare upstream name", got)
	}
}

func TestAnthropicModelAliasLookup_WildcardWhitelistStillWorks(t *testing.T) {
	svc := &GatewayService{}
	account := anthropicAccountWithMapping(AccountTypeAPIKey, map[string]any{
		"claude-haiku-*": "claude-haiku-4-5-20251001",
	})
	for _, requested := range []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001"} {
		if !svc.isModelSupportedByAccount(account, requested) {
			t.Fatalf("wildcard whitelist must still match %q", requested)
		}
	}
	if svc.isModelSupportedByAccount(account, "claude-opus-4-5") {
		t.Fatalf("wildcard claude-haiku-* must not match claude-opus-4-5")
	}
}

// 差分阴性（最关键）：白名单里根本没有的模型必须继续被拒绝，两种写法都拒。
// 「白名单没配就该 404」是本系统的正确语义；归一化一旦把未配置的模型名归一化到
// 某个已配置的键上，整张模型白名单会静默失效——那是权限缺陷，不只是功能缺陷。
// 埋雷标定：把 IsModelSupported 的 mapping 分支改成无条件 return true、或把
// alternateAnthropicModelIDForLookup 换成「正则剥掉结尾 8 位日期 + 前缀匹配」，
// 本用例必须变红。
func TestAnthropicModelAliasLookup_UnlistedModelStillRejected(t *testing.T) {
	svc := &GatewayService{}
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		account := anthropicAccountWithMapping(accountType, map[string]any{
			"claude-haiku-4-5": "claude-haiku-4-5",
		})
		rejected := []string{
			// 运维刻意不支持的模型：裸名与带日期两种写法都必须被拒。
			"claude-sonnet-4-5",
			"claude-sonnet-4-5-20250929",
			"claude-opus-4-5",
			"claude-opus-4-5-20251101",
			// 伪造/过期日期后缀：不能被「剥掉日期」之类的宽松规则放行。
			"claude-haiku-4-5-20240101",
			"claude-haiku-4-5-99999999",
			// 别的厂商。
			"gemini-3-pro",
		}
		for _, requested := range rejected {
			if svc.isModelSupportedByAccount(account, requested) {
				t.Fatalf("type=%s: %q is not in the whitelist and must stay rejected", accountType, requested)
			}
			if account.IsModelSupported(requested) {
				t.Fatalf("type=%s: IsModelSupported(%q) must stay false", accountType, requested)
			}
		}
	}
}

// 未配置的模型也不能在转发路径上被悄悄改名成某条已配置的 mapping。
func TestAnthropicModelAliasLookup_UnlistedModelNotRewrittenOnForward(t *testing.T) {
	account := anthropicAccountWithMapping(AccountTypeAPIKey, map[string]any{
		"claude-haiku-4-5": "claude-haiku-4-5",
	})
	for _, requested := range []string{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929"} {
		if got := account.GetMappedModel(requested); got != requested {
			t.Fatalf("GetMappedModel(%q) = %q, want the request unchanged", requested, got)
		}
	}
}

// 差分阴性：非 anthropic 平台的归一化逻辑不受影响。
func TestAnthropicModelAliasLookup_NonAnthropicPlatformsUnchanged(t *testing.T) {
	for _, platform := range []string{PlatformGemini, PlatformAntigravity} {
		if got := normalizeRequestedModelForLookup(platform, "gemini-3.1-pro-preview-customtools"); got != "gemini-3.1-pro-preview" {
			t.Fatalf("platform=%s: customtools alias = %q, want gemini-3.1-pro-preview", platform, got)
		}
		// Anthropic 的短/长互换绝不能泄漏到 gemini/antigravity。
		if got := normalizeRequestedModelForLookup(platform, "claude-haiku-4-5"); got != "claude-haiku-4-5" {
			t.Fatalf("platform=%s: claude alias leaked, got %q", platform, got)
		}
	}
	for _, platform := range []string{PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok} {
		if got := normalizeRequestedModelForLookup(platform, "claude-haiku-4-5-20251001"); got != "claude-haiku-4-5-20251001" {
			t.Fatalf("platform=%s: dated claude alias leaked, got %q", platform, got)
		}
	}

	geminiAccount := &Account{
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gemini-3.1-pro-preview": "gemini-3.1-pro-preview"},
		},
	}
	if !geminiAccount.IsModelSupported("gemini-3.1-pro-preview-customtools") {
		t.Fatalf("gemini customtools alias must remain supported")
	}
	if geminiAccount.IsModelSupported("gemini-3-flash") {
		t.Fatalf("gemini unlisted model must stay rejected")
	}
}

func TestAnthropicModelAliasLookup_EmptyMappingUnaffected(t *testing.T) {
	account := &Account{
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
	}
	if !account.IsModelSupported("claude-haiku-4-5") {
		t.Fatalf("empty mapping must keep allowing every model")
	}
}
