//go:build unit

package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/mirasim"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// stubAccountTestPricer 按型号名直接给单价，不触碰真实价卡加载逻辑。
// 名字不在表里 = 价格表认不出这个型号（HasIdentifiedTokenPricing 返回 false）。
type stubAccountTestPricer struct {
	inputPrice  map[string]float64
	outputPrice map[string]float64
	asked       []string
}

func (p *stubAccountTestPricer) HasIdentifiedTokenPricing(model string) bool {
	p.asked = append(p.asked, model)
	_, ok := p.inputPrice[model]
	return ok
}

func (p *stubAccountTestPricer) GetModelPricing(model string) (*ModelPricing, error) {
	in, ok := p.inputPrice[model]
	if !ok {
		return nil, nil
	}
	return &ModelPricing{InputPricePerToken: in, OutputPricePerToken: p.outputPrice[model]}, nil
}

// newMirasimShapedAccount 造一个与线上 mirasim 同形的账号：platform=anthropic +
// credentials.provider=mirasim（IsMirasimAccount 的全部判据），type=apikey。
func newMirasimShapedAccount(id int64, modelMapping map[string]any) *Account {
	credentials := map[string]any{
		"api_key":            "sk-mirasim-test",
		"base_url":           "https://upstream.example.com",
		mirasim.CredProvider: mirasim.ProviderMirasim,
	}
	if modelMapping != nil {
		credentials["model_mapping"] = modelMapping
	}
	return &Account{
		ID:          id,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: credentials,
	}
}

// newClaudeTestServiceForModelSelection 组一个只够跑通 anthropic 直连测试路径的服务。
func newClaudeTestServiceForModelSelection(account *Account, pricer accountTestModelPricer) (*AccountTestService, *queuedHTTPUpstream) {
	// 上游回一条最短的合法 Claude SSE，使测试路径正常收尾。
	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"message_stop\"}\n\n"))
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	repo := &mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}
	return &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		modelPricer:  pricer,
	}, upstream
}

func sentUpstreamModel(t *testing.T, upstream *queuedHTTPUpstream) string {
	t.Helper()
	require.Len(t, upstream.requests, 1)
	body, err := io.ReadAll(upstream.requests[0].Body)
	require.NoError(t, err)
	return gjson.GetBytes(body, "model").String()
}

// 正例：账号白名单排除了全局默认型号时，健康检查改用白名单里最便宜的那个。
// 这正是 mirasim 的形状——它不提供 sonnet-4-5，旧实现每次必拿 422。
func TestAccountTestService_DefaultTestModelPicksCheapestWhitelistedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(701, map[string]any{
		"claude-opus-4-6":   "claude-opus-4-6",
		"claude-haiku-4-6":  "claude-haiku-4-6",
		"claude-sonnet-4-6": "claude-sonnet-4-6",
	})
	pricer := &stubAccountTestPricer{
		inputPrice: map[string]float64{
			"claude-opus-4-6":   0.000015,
			"claude-haiku-4-6":  0.0000008,
			"claude-sonnet-4-6": 0.000003,
		},
		outputPrice: map[string]float64{
			"claude-opus-4-6":   0.000075,
			"claude-haiku-4-6":  0.000004,
			"claude-sonnet-4-6": 0.000015,
		},
	}
	svc, upstream := newClaudeTestServiceForModelSelection(account, pricer)

	require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault))

	// [[cov:DM:whitelist-cheapest]] 发给上游的是白名单里最便宜的型号，不是全局默认。
	require.Equal(t, "claude-haiku-4-6", sentUpstreamModel(t, upstream))
	require.NotEqual(t, claude.DefaultTestModel, sentUpstreamModel(t, upstream))
	// "最便宜"来自价卡服务而不是内置表：三个候选都被问过价。
	require.Len(t, pricer.asked, 3)
	require.Contains(t, pricer.asked, "claude-opus-4-6")
}

// 差分阴性：账号没有白名单时仍回落原默认值——证明这不是"无条件换成便宜型号"。
// 与上一条只差 model_mapping 这一个开关。
func TestAccountTestService_DefaultTestModelFallsBackWithoutWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(702, nil)
	pricer := &stubAccountTestPricer{
		inputPrice:  map[string]float64{"claude-haiku-4-6": 0.0000008},
		outputPrice: map[string]float64{"claude-haiku-4-6": 0.000004},
	}
	svc, upstream := newClaudeTestServiceForModelSelection(account, pricer)

	require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault))

	// [[cov:DM:no-whitelist-keeps-default]]
	require.Equal(t, claude.DefaultTestModel, sentUpstreamModel(t, upstream))
	// 没有白名单就没有可比的候选，压根不该去问价。
	require.Empty(t, pricer.asked)
}

// 差分阴性：白名单**包含**全局默认时沿用全局默认，即使白名单里有更便宜的型号。
// 与正例只差"白名单里有没有 sonnet-4-5"这一个开关。
func TestAccountTestService_DefaultTestModelKeptWhenWhitelistCoversIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(703, map[string]any{
		claude.DefaultTestModel: claude.DefaultTestModel,
		"claude-haiku-4-6":      "claude-haiku-4-6",
	})
	pricer := &stubAccountTestPricer{
		inputPrice: map[string]float64{
			claude.DefaultTestModel: 0.000003,
			"claude-haiku-4-6":      0.0000008,
		},
		outputPrice: map[string]float64{
			claude.DefaultTestModel: 0.000015,
			"claude-haiku-4-6":      0.000004,
		},
	}
	svc, upstream := newClaudeTestServiceForModelSelection(account, pricer)

	require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault))

	// [[cov:DM:whitelist-covers-default]]
	require.Equal(t, claude.DefaultTestModel, sentUpstreamModel(t, upstream))
	require.Empty(t, pricer.asked)
}

// 调用方显式传了型号时，白名单选型不得插手。
func TestAccountTestService_ExplicitModelIDBeatsWhitelistSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	account := newMirasimShapedAccount(704, map[string]any{
		"claude-opus-4-6":  "claude-opus-4-6",
		"claude-haiku-4-6": "claude-haiku-4-6",
	})
	pricer := &stubAccountTestPricer{
		inputPrice: map[string]float64{
			"claude-opus-4-6":  0.000015,
			"claude-haiku-4-6": 0.0000008,
		},
		outputPrice: map[string]float64{
			"claude-opus-4-6":  0.000075,
			"claude-haiku-4-6": 0.000004,
		},
	}
	svc, upstream := newClaudeTestServiceForModelSelection(account, pricer)

	require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "claude-opus-4-6", "", AccountTestModeDefault))

	// [[cov:DM:explicit-model-wins]] 显式型号原样上路，尽管 haiku 更便宜。
	require.Equal(t, "claude-opus-4-6", sentUpstreamModel(t, upstream))
	require.Empty(t, pricer.asked)
}

// 选型规则本身的边界：通配符不是型号、认不出价的不被丢掉、并列有确定次序。
func TestCheapestWhitelistedAccountTestModel_SelectionOrder(t *testing.T) {
	pricer := &stubAccountTestPricer{
		inputPrice: map[string]float64{
			"claude-opus-4-6":  0.000015,
			"claude-haiku-4-6": 0.0000008,
		},
		outputPrice: map[string]float64{
			"claude-opus-4-6":  0.000075,
			"claude-haiku-4-6": 0.000004,
		},
	}

	// 通配符键是映射规则不是具体型号，必须跳过；认不出价的具体型号不参与比价。
	withWildcard := newMirasimShapedAccount(705, map[string]any{
		"claude-*":              "claude-haiku-4-6",
		"claude-opus-4-6":       "claude-opus-4-6",
		"claude-haiku-4-6":      "claude-haiku-4-6",
		"claude-unpriced-alias": "claude-unpriced-alias",
	})
	// [[cov:DM:skip-wildcard-prefer-priced]]
	require.Equal(t, "claude-haiku-4-6", cheapestWhitelistedAccountTestModel(withWildcard, pricer))

	// 一个都比不出价时不放弃：取字典序最小的具体型号（账号至少真的服务它）。
	allUnpriced := newMirasimShapedAccount(706, map[string]any{
		"zeta-model":  "zeta-model",
		"alpha-model": "alpha-model",
	})
	// [[cov:DM:unpriced-falls-to-lexical]]
	require.Equal(t, "alpha-model", cheapestWhitelistedAccountTestModel(allUnpriced, pricer))

	// 同价并列必须每次选同一个（map 迭代无序）。
	tied := newMirasimShapedAccount(707, map[string]any{
		"claude-tie-b": "claude-tie-b",
		"claude-tie-a": "claude-tie-a",
	})
	tiedPricer := &stubAccountTestPricer{
		inputPrice:  map[string]float64{"claude-tie-a": 0.000001, "claude-tie-b": 0.000001},
		outputPrice: map[string]float64{"claude-tie-a": 0.000002, "claude-tie-b": 0.000002},
	}
	for range 20 {
		// [[cov:DM:tie-break-deterministic]]
		require.Equal(t, "claude-tie-a", cheapestWhitelistedAccountTestModel(tied, tiedPricer))
	}

	// 只有通配符的白名单挑不出具体型号 → 交回上层回落全局默认。
	onlyWildcard := newMirasimShapedAccount(708, map[string]any{"claude-*": "claude-haiku-4-6"})
	// [[cov:DM:wildcard-only-yields-nothing]]
	require.Equal(t, "", cheapestWhitelistedAccountTestModel(onlyWildcard, pricer))
}
