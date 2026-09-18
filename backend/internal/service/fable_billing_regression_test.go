//go:build unit

package service

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

// Replays the reported token amounts through the real usage/debit entrypoint.
// The result must not depend on choosing response-model billing or enabling
// account-stat pricing just to escape a hidden model-specific surcharge.
func TestFableRecordUsage_NoHiddenTripleAcrossBillingSources(t *testing.T) {
	for _, source := range []string{BillingModelSourceRequested, BillingModelSourceUpstream, BillingModelSourceResponse} {
		for _, stats := range []bool{false, true} {
			for _, multiplier := range []*float64{nil, testPtrFloat64(1), testPtrFloat64(1.5)} {
				t.Run(fmt.Sprintf("%s/stats=%v/mult=%v", source, stats, multiplier), func(t *testing.T) {
					usage := &openAIRecordUsageLogRepoStub{inserted: true}
					user := &openAIRecordUsageUserRepoStub{}
					svc := newGatewayRecordUsageServiceForTest(usage, user, &openAIRecordUsageSubRepoStub{})
					gid := int64(902)
					channel := &Channel{ID: 1, Status: StatusActive, BillingModelSource: source, ApplyPricingToAccountStats: stats,
						ModelPricing: []ChannelModelPricing{{Platform: PlatformAnthropic, Models: []string{"claude-fable-5-1"}, BillingMode: BillingModeToken,
							InputPrice: testPtrFloat64(10e-6), OutputPrice: testPtrFloat64(50e-6), CacheWritePrice: testPtrFloat64(12.5e-6), CacheReadPrice: testPtrFloat64(.25e-6), MaxReasoningEffortMultiplier: multiplier}}}
					channel.GroupIDs = []int64{gid}
					svc.channelService = newTestChannelService(makeStandardRepo(*channel, map[int64]string{gid: PlatformAnthropic}))
					svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
					effort := "max"
					err := svc.RecordUsage(context.Background(), &RecordUsageInput{
						Result: &ForwardResult{RequestID: "fable-root-cause", Model: "claude-fable-5-1", UpstreamModel: "claude-fable-5-1", UpstreamResponseModel: "claude-fable-5-1", ReasoningEffort: &effort,
							Usage: ClaudeUsage{InputTokens: 56, OutputTokens: 935, CacheCreationInputTokens: 509908, CacheReadInputTokens: 89729}},
						APIKey: &APIKey{ID: 501, GroupID: &gid, Group: &Group{ID: gid, Platform: PlatformAnthropic, RateMultiplier: .35}},
						User:   &User{ID: 601}, Account: &Account{ID: 701, Platform: PlatformAnthropic},
						ChannelUsageFields: ChannelUsageFields{OriginalModel: "claude-fable-5-1", ChannelMappedModel: "claude-fable-5-1", BillingModelSource: source},
					})
					require.NoError(t, err)
					want := 6.44359225
					if multiplier != nil {
						want *= *multiplier
					}
					require.InDelta(t, want, usage.lastLog.TotalCost, 1e-9)
					require.InDelta(t, want*.35, usage.lastLog.ActualCost, 1e-8)
					require.InDelta(t, want*.35, user.lastAmount, 1e-8)
					require.NotNil(t, usage.lastLog.AccountStatsCost)
					accountCost := 6.44359225
					if stats {
						accountCost = want
					}
					require.InDelta(t, accountCost, *usage.lastLog.AccountStatsCost, 1e-8)
				})
			}
		}
	}
}
