//go:build unit

package service

// 账号售价倍率（accounts.sell_rate_multiplier）回归：
//   - 生效规则：账号售价 ?? (用户-分组覆盖 ?? 分组默认)，再乘高峰因子；
//   - usage_logs.rate_multiplier 记实际生效值，actual_cost = total_cost × 实际倍率；
//   - Gateway（Anthropic 等）与 OpenAI 两条 RecordUsage 路径同口径；
//   - 利润门对配置了售价的账号逐账号求阈值；
//   - composite 分组按最终服务账号的售价计价，而不是父分组价。

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

func sellRatePtr(v float64) *float64 { return &v }

func TestAccountResolveSellRateMultiplier(t *testing.T) {
	cases := []struct {
		name     string
		account  *Account
		fallback float64
		want     float64
	}{
		{"nil account falls back", nil, 2.0, 2.0},
		{"unset falls back to group", &Account{}, 2.0, 2.0},
		{"sell rate overrides group", &Account{SellRateMultiplier: sellRatePtr(1.1)}, 2.0, 1.1},
		{"sell rate may exceed group", &Account{SellRateMultiplier: sellRatePtr(4.2)}, 2.0, 4.2},
		{"zero is a legal free sell rate", &Account{SellRateMultiplier: sellRatePtr(0)}, 2.0, 0},
		{"negative is treated as unset", &Account{SellRateMultiplier: sellRatePtr(-1)}, 2.0, 2.0},
		{"NaN is treated as unset", &Account{SellRateMultiplier: sellRatePtr(math.NaN())}, 2.0, 2.0},
		{"Inf is treated as unset", &Account{SellRateMultiplier: sellRatePtr(math.Inf(1))}, 2.0, 2.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.account.ResolveSellRateMultiplier(tc.fallback))
		})
	}
}

func TestNormalizeSellRateMultiplierInput(t *testing.T) {
	v, clear, err := normalizeSellRateMultiplierInput(nil)
	require.NoError(t, err)
	require.True(t, clear)
	require.Nil(t, v)

	v, clear, err = normalizeSellRateMultiplierInput(sellRatePtr(-1))
	require.NoError(t, err)
	require.True(t, clear, "负数约定为清空")
	require.Nil(t, v)

	v, clear, err = normalizeSellRateMultiplierInput(sellRatePtr(0))
	require.NoError(t, err)
	require.False(t, clear)
	require.Equal(t, 0.0, *v)

	v, clear, err = normalizeSellRateMultiplierInput(sellRatePtr(1.25))
	require.NoError(t, err)
	require.False(t, clear)
	require.Equal(t, 1.25, *v)

	_, _, err = normalizeSellRateMultiplierInput(sellRatePtr(math.NaN()))
	require.Error(t, err)
	_, _, err = normalizeSellRateMultiplierInput(sellRatePtr(math.Inf(1)))
	require.Error(t, err)
	_, _, err = normalizeSellRateMultiplierInput(sellRatePtr(1e7))
	require.Error(t, err)
}

func newGatewaySellRateServiceForTest(usageRepo UsageLogRepository, userRepo UserRepository, rateRepo UserGroupRateRepository) *GatewayService {
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{})
	svc.userGroupRateResolver = newUserGroupRateResolver(rateRepo, nil, resolveUserGroupRateCacheTTL(svc.cfg), nil, "service.gateway.sell_rate.test")
	return svc
}

func sellRateGatewayInput(requestID string, group *Group, account *Account, pricingAt time.Time) *RecordUsageInput {
	return &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: requestID,
			Usage:     ClaudeUsage{InputTokens: 1000, OutputTokens: 500},
			Model:     "claude-sonnet-4",
			Duration:  time.Second,
		},
		APIKey:    &APIKey{ID: 901, GroupID: i64p(group.ID), Group: group},
		User:      &User{ID: 902},
		Account:   account,
		PricingAt: pricingAt,
	}
}

func TestGatewayRecordUsage_AccountSellRate(t *testing.T) {
	offPeak := time.Date(2026, time.January, 1, 12, 0, 0, 0, timezone.Location())

	cases := []struct {
		name      string
		group     *Group
		userRate  *float64
		account   *Account
		pricingAt time.Time
		want      float64
	}{
		{
			name:    "account sell rate overrides group rate",
			group:   &Group{ID: 41, Platform: PlatformAnthropic, RateMultiplier: 2.0},
			account: &Account{ID: 1, Platform: PlatformAnthropic, SellRateMultiplier: sellRatePtr(1.1)},
			want:    1.1,
		},
		{
			name:    "empty sell rate falls back to group rate",
			group:   &Group{ID: 42, Platform: PlatformAnthropic, RateMultiplier: 2.0},
			account: &Account{ID: 2, Platform: PlatformAnthropic},
			want:    2.0,
		},
		{
			name:     "account sell rate overrides user-group override",
			group:    &Group{ID: 43, Platform: PlatformAnthropic, RateMultiplier: 2.0},
			userRate: sellRatePtr(1.8),
			account:  &Account{ID: 3, Platform: PlatformAnthropic, SellRateMultiplier: sellRatePtr(1.25)},
			want:     1.25,
		},
		{
			name:     "empty sell rate keeps user-group override",
			group:    &Group{ID: 44, Platform: PlatformAnthropic, RateMultiplier: 2.0},
			userRate: sellRatePtr(1.8),
			account:  &Account{ID: 4, Platform: PlatformAnthropic},
			want:     1.8,
		},
		{
			name: "peak factor multiplies account sell rate",
			group: &Group{
				ID: 45, Platform: PlatformAnthropic, RateMultiplier: 2.0,
				SubscriptionType: SubscriptionTypeSubscription,
				PeakRateEnabled:  true, PeakStart: "00:00", PeakEnd: "01:00", PeakRateMultiplier: 3.0,
			},
			account:   &Account{ID: 5, Platform: PlatformAnthropic, SellRateMultiplier: sellRatePtr(1.1)},
			pricingAt: time.Date(2026, time.January, 1, 0, 30, 0, 0, timezone.Location()),
			want:      3.3,
		},
		{
			name:    "zero sell rate bills nothing",
			group:   &Group{ID: 46, Platform: PlatformAnthropic, RateMultiplier: 2.0},
			account: &Account{ID: 6, Platform: PlatformAnthropic, SellRateMultiplier: sellRatePtr(0)},
			want:    0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			userRepo := &openAIRecordUsageUserRepoStub{}
			svc := newGatewaySellRateServiceForTest(usageRepo, userRepo, &openAIUserGroupRateRepoStub{rate: tc.userRate})
			pricingAt := tc.pricingAt
			if pricingAt.IsZero() {
				pricingAt = offPeak
			}
			tc.account.RateMultiplier = sellRatePtr(0.7) // 成本倍率与售价互不干扰

			err := svc.RecordUsage(context.Background(), sellRateGatewayInput("gw_sell_"+tc.name, tc.group, tc.account, pricingAt))
			require.NoError(t, err)
			log := usageRepo.lastLog
			require.NotNil(t, log)
			require.InDelta(t, tc.want, log.RateMultiplier, 1e-12)
			require.Greater(t, log.TotalCost, 0.0)
			require.InDelta(t, log.TotalCost*tc.want, log.ActualCost, 1e-10)
			require.NotNil(t, log.AccountRateMultiplier)
			require.Equal(t, 0.7, *log.AccountRateMultiplier, "账号成本倍率仍按 rate_multiplier 记录")
			if tc.want > 0 {
				require.InDelta(t, log.ActualCost, userRepo.lastAmount, 1e-10)
			}
		})
	}
}

func TestOpenAIRecordUsage_AccountSellRate(t *testing.T) {
	offPeak := time.Date(2026, time.January, 1, 12, 0, 0, 0, timezone.Location())
	usage := OpenAIUsage{InputTokens: 2000, OutputTokens: 400}

	cases := []struct {
		name      string
		group     *Group
		userRate  *float64
		account   *Account
		pricingAt time.Time
		want      float64
	}{
		{
			name:    "account sell rate overrides group rate",
			group:   &Group{ID: 51, Platform: PlatformOpenAI, RateMultiplier: 1.2},
			account: &Account{ID: 11, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.6)},
			want:    1.6,
		},
		{
			name:    "empty sell rate falls back to group rate",
			group:   &Group{ID: 52, Platform: PlatformOpenAI, RateMultiplier: 1.2},
			account: &Account{ID: 12, Platform: PlatformOpenAI},
			want:    1.2,
		},
		{
			name:     "account sell rate overrides user-group override",
			group:    &Group{ID: 53, Platform: PlatformOpenAI, RateMultiplier: 1.2},
			userRate: sellRatePtr(0.9),
			account:  &Account{ID: 13, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.5)},
			want:     1.5,
		},
		{
			name: "peak factor multiplies account sell rate",
			group: &Group{
				ID: 54, Platform: PlatformOpenAI, RateMultiplier: 1.2,
				SubscriptionType: SubscriptionTypeSubscription,
				PeakRateEnabled:  true, PeakStart: "00:00", PeakEnd: "01:00", PeakRateMultiplier: 2.0,
			},
			account:   &Account{ID: 14, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.5)},
			pricingAt: time.Date(2026, time.January, 1, 0, 30, 0, 0, timezone.Location()),
			want:      3.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			userRepo := &openAIRecordUsageUserRepoStub{}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, &openAIUserGroupRateRepoStub{rate: tc.userRate})
			pricingAt := tc.pricingAt
			if pricingAt.IsZero() {
				pricingAt = offPeak
			}
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{
					RequestID: "oa_sell_" + tc.name,
					Usage:     usage,
					Model:     "gpt-5.1",
					Duration:  time.Second,
				},
				APIKey:    &APIKey{ID: 911, GroupID: i64p(tc.group.ID), Group: tc.group},
				User:      &User{ID: 912},
				Account:   tc.account,
				PricingAt: pricingAt,
			})
			require.NoError(t, err)
			log := usageRepo.lastLog
			require.NotNil(t, log)
			require.InDelta(t, tc.want, log.RateMultiplier, 1e-12)
			expected := expectedOpenAICost(t, svc, "gpt-5.1", usage, tc.want)
			require.InDelta(t, expected.ActualCost, log.ActualCost, 1e-12)
			require.InDelta(t, log.TotalCost*tc.want, log.ActualCost, 1e-10)
			require.InDelta(t, log.ActualCost, userRepo.lastAmount, 1e-12)
		})
	}
}

// failover 换号后按最终账号计价的端到端覆盖见
// handler/openai_sell_rate_failover_test.go（首跳 5xx → 换号成功 → 只按最终账号售价记一条用量）。

// composite 分组：一个 key 同时调 Claude 与 GPT。扣费按最终服务账号的售价，
// 而不是父分组价；未配售价的账号仍按父分组价。
func TestRecordUsage_CompositeGroupBillsAccountSellRate(t *testing.T) {
	offPeak := time.Date(2026, time.January, 1, 12, 0, 0, 0, timezone.Location())
	composite := &Group{ID: 71, Platform: PlatformComposite, RateMultiplier: 2.0}

	t.Run("anthropic account in composite group", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			account *Account
			want    float64
		}{
			{"sell rate", &Account{ID: 201, Platform: PlatformAnthropic, SellRateMultiplier: sellRatePtr(1.1)}, 1.1},
			{"no sell rate", &Account{ID: 202, Platform: PlatformAnthropic}, 2.0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				userRepo := &openAIRecordUsageUserRepoStub{}
				svc := newGatewaySellRateServiceForTest(usageRepo, userRepo, &openAIUserGroupRateRepoStub{})
				err := svc.RecordUsage(context.Background(), sellRateGatewayInput("composite_claude_"+tc.name, composite, tc.account, offPeak))
				require.NoError(t, err)
				require.InDelta(t, tc.want, usageRepo.lastLog.RateMultiplier, 1e-12)
				require.Greater(t, usageRepo.lastLog.TotalCost, 0.0)
				require.InDelta(t, usageRepo.lastLog.TotalCost*tc.want, usageRepo.lastLog.ActualCost, 1e-10)
			})
		}
	})

	t.Run("openai account in composite group", func(t *testing.T) {
		usage := OpenAIUsage{InputTokens: 2000, OutputTokens: 400}
		for _, tc := range []struct {
			name    string
			account *Account
			want    float64
		}{
			{"sell rate", &Account{ID: 211, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.2)}, 1.2},
			{"no sell rate", &Account{ID: 212, Platform: PlatformOpenAI}, 2.0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				userRepo := &openAIRecordUsageUserRepoStub{}
				svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, &openAIUserGroupRateRepoStub{})
				err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{
						RequestID: "composite_gpt_" + tc.name,
						Usage:     usage,
						Model:     "gpt-5.1",
						Duration:  time.Second,
					},
					APIKey:    &APIKey{ID: 921, GroupID: i64p(composite.ID), Group: composite},
					User:      &User{ID: 922},
					Account:   tc.account,
					PricingAt: offPeak,
				})
				require.NoError(t, err)
				require.InDelta(t, tc.want, usageRepo.lastLog.RateMultiplier, 1e-12)
				require.Greater(t, usageRepo.lastLog.TotalCost, 0.0)
				require.InDelta(t, usageRepo.lastLog.TotalCost*tc.want, usageRepo.lastLog.ActualCost, 1e-10)
			})
		}
	})
}

// 利润门逐账号阈值：有售价的账号按 售价 × 高峰 × (1 − margin − buffer) 判定，
// 无售价的账号沿用分组阈值。
func TestProfitControl_AccountSellRatePerAccountThreshold(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(81)
	group := profitControlTestGroup(groupID, 0.1, 0.1) // keep = 0.8
	group.RateMultiplier = 2.0                         // 分组阈值 = 1.6
	ctx := svc.withOpenAIProfitControlGate(profitControlTestCtx(group), &groupID)

	gate, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.True(t, ok)
	require.InDelta(t, 1.6, gate.threshold, 1e-12)

	cases := []struct {
		name    string
		cost    float64
		sell    *float64
		vetoed  bool
		comment string
	}{
		{"no sell rate under group threshold", 1.5, nil, false, "1.5 <= 1.6"},
		{"no sell rate over group threshold", 1.9, nil, true, "1.9 > 1.6"},
		{"cheap sell rate vetoes account the group price would admit", 1.5, sellRatePtr(1.2), true, "1.5 > 1.2*0.8=0.96"},
		{"premium sell rate admits account the group price would veto", 3.5, sellRatePtr(4.5), false, "3.5 <= 4.5*0.8=3.6"},
		{"premium sell rate still vetoes when margin is not met", 3.7, sellRatePtr(4.5), true, "3.7 > 3.6"},
		{"invalid sell rate falls back to group threshold", 1.5, sellRatePtr(-1), false, "1.5 <= 1.6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := profitControlTestAccountWithRate(&Account{ID: 1, Platform: PlatformOpenAI, SellRateMultiplier: tc.sell}, tc.cost)
			vetoed, _ := OpenAIProfitControlVeto(ctx, account)
			require.Equal(t, tc.vetoed, vetoed, tc.comment)
		})
	}
}

func TestProfitControl_AccountSellRateThresholdIncludesPeak(t *testing.T) {
	svc := &OpenAIGatewayService{}
	groupID := int64(82)
	group := profitControlTestGroup(groupID, 0.2, 0) // keep = 0.8
	group.RateMultiplier = 1.0
	group.SubscriptionType = SubscriptionTypeSubscription
	group.PeakRateEnabled = true
	group.PeakStart = "00:00"
	group.PeakEnd = "23:59"
	group.PeakRateMultiplier = 2.0

	ctx := context.WithValue(profitControlTestCtx(group), openAIPricingAtCtxKey{}, time.Date(2026, 1, 1, 12, 0, 0, 0, timezone.Location()))
	ctx = svc.withOpenAIProfitControlGate(ctx, &groupID)
	gate, ok := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.True(t, ok)
	require.InDelta(t, 1.6, gate.threshold, 1e-12)     // 1.0 × 2.0 × 0.8
	require.InDelta(t, 1.6, gate.sellRateScale, 1e-12) // 2.0 × 0.8

	account := profitControlTestAccountWithRate(&Account{ID: 2, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.5)}, 2.3)
	vetoed, _ := OpenAIProfitControlVeto(ctx, account) // 阈值 1.5 × 2.0 × 0.8 = 2.4
	require.False(t, vetoed)
	account = profitControlTestAccountWithRate(&Account{ID: 3, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.5)}, 2.5)
	vetoed, _ = OpenAIProfitControlVeto(ctx, account)
	require.True(t, vetoed)
}

func TestGatewayProfitControl_AccountSellRatePerAccountThreshold(t *testing.T) {
	group := gatewayProfitTestGroup(91, PlatformAnthropic)
	group.RateMultiplier = 2.0
	group.ProfitMinMargin = 0.1 // keep = 0.9, 分组阈值 1.8
	groupID := group.ID
	svc := &GatewayService{}
	ctx := svc.withGatewayProfitControlGate(gatewayProfitTestContext(group), &groupID)
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gate)
	require.InDelta(t, 1.8, gate.threshold, 1e-12)

	// p4 档：成本 3.5，分组价 2.0 必挡；账号售价 4.5 时 4.5×0.9=4.05 放行。
	premium := gatewayProfitTestAccount(1, PlatformAnthropic, 3.5, groupID)
	premium.SellRateMultiplier = sellRatePtr(4.5)
	require.True(t, svc.isGatewayAccountProfitEligible(ctx, &premium))
	plain := gatewayProfitTestAccount(2, PlatformAnthropic, 3.5, groupID)
	require.False(t, svc.isGatewayAccountProfitEligible(ctx, &plain))
	// 低价号：成本 1.2，分组价放行；账号售价 1.1 时 1.1×0.9=0.99 必挡（否则每条都亏）。
	cheap := gatewayProfitTestAccount(3, PlatformAnthropic, 1.2, groupID)
	cheap.SellRateMultiplier = sellRatePtr(1.1)
	require.False(t, svc.isGatewayAccountProfitEligible(ctx, &cheap))
}

// composite：D 的高峰因子取计费父分组，售价取账号，margin 取被调度成员分组。
func TestGatewayProfitControl_CompositeUsesAccountSellRate(t *testing.T) {
	billingGroup := &Group{
		ID:               301,
		Platform:         PlatformComposite,
		Status:           StatusActive,
		Hydrated:         true,
		RateMultiplier:   2.0,
		SubscriptionType: SubscriptionTypeStandard,
	}
	memberGroup := gatewayProfitTestGroup(302, PlatformAnthropic)
	memberGroup.RateMultiplier = 99
	memberGroup.ProfitMinMargin = 0.2

	ctx := context.WithValue(context.Background(), ctxkey.Group, billingGroup)
	ctx, _ = WithGatewayTokenRequestPricing(ctx)
	svc := &GatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(nil, nil, nil, profitControlGroupRepo{group: memberGroup}, nil),
	}
	ctx = svc.withGatewayProfitControlGate(ctx, &memberGroup.ID)
	gate, _ := ctx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gate)
	require.InDelta(t, 1.6, gate.threshold, 1e-12) // 2.0 × 0.8

	account := gatewayProfitTestAccount(1, PlatformAnthropic, 3.0, memberGroup.ID)
	account.SellRateMultiplier = sellRatePtr(4.0) // 4.0 × 0.8 = 3.2
	require.True(t, svc.isGatewayAccountProfitEligible(ctx, &account))
}

func TestProfitPreview_AccountSellRate(t *testing.T) {
	group := profitControlTestGroup(401, 0.1, 0.1) // keep 0.8
	group.RateMultiplier = 2.0
	cheapSell := profitControlTestAccountWithRate(&Account{ID: 1, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(1.1)}, 1.0)
	premiumSell := profitControlTestAccountWithRate(&Account{ID: 2, Platform: PlatformOpenAI, SellRateMultiplier: sellRatePtr(4.5)}, 3.5)
	plain := profitControlTestAccountWithRate(&Account{ID: 3, Platform: PlatformOpenAI}, 1.5)

	reports := PreviewProfitAdmission([]ProfitPreviewGroupInput{{
		Group:         group,
		Accounts:      []*Account{cheapSell, premiumSell, plain},
		UserOverrides: map[int64]float64{7: 0.5}, // 最低 D = 0.5 × 0.8 = 0.4
	}}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	require.Len(t, reports, 1)
	verdicts := reports[0].Verdicts
	require.Len(t, verdicts, 3)

	require.Equal(t, ProfitPreviewClassRejectedThreshold, verdicts[0].Class, "1.0 > 1.1×0.8")
	require.NotNil(t, verdicts[0].SellRate)
	require.Equal(t, 1.1, *verdicts[0].SellRate)

	require.Equal(t, ProfitPreviewClassAdmitted, verdicts[1].Class, "3.5 <= 4.5×0.8")
	require.False(t, verdicts[1].RejectedUnderMinD, "账号售价覆盖用户覆盖，最低 D 口径同样按售价")

	require.Equal(t, ProfitPreviewClassAdmitted, verdicts[2].Class, "1.5 <= 2.0×0.8")
	require.True(t, verdicts[2].RejectedUnderMinD, "无售价账号仍受用户覆盖最低 D 影响")
	require.Nil(t, verdicts[2].SellRate)
}
