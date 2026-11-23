package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type poolModelFailureRepoStub struct {
	AccountRepository // 未实现的方法会 panic（不应被调用）

	account          *Account
	getByIDCalls     int
	tempUnschedCalls int
	modelLimits      map[string]time.Time
}

func (r *poolModelFailureRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	r.getByIDCalls++
	return r.account, nil
}

func (r *poolModelFailureRepoStub) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.tempUnschedCalls++
	return nil
}

func (r *poolModelFailureRepoStub) SetModelRateLimit(_ context.Context, _ int64, scope string, resetAt time.Time, _ ...string) error {
	if r.modelLimits == nil {
		r.modelLimits = map[string]time.Time{}
	}
	r.modelLimits[scope] = resetAt
	return nil
}

// poolModelFailureCacheStub 模拟 Redis 滑动窗口：每个（账号, 模型）独立计数，达到阈值即触发并清零。
type poolModelFailureCacheStub struct {
	counts map[string]int64
}

func (c *poolModelFailureCacheStub) SetTempUnsched(context.Context, int64, *TempUnschedState) error {
	return nil
}
func (c *poolModelFailureCacheStub) GetTempUnsched(context.Context, int64) (*TempUnschedState, error) {
	return nil, nil
}
func (c *poolModelFailureCacheStub) DeleteTempUnsched(context.Context, int64) error { return nil }

func (c *poolModelFailureCacheStub) RecordPoolModelFailure(_ context.Context, _ int64, modelKey string, _ int, threshold int) (int64, bool, error) {
	if c.counts == nil {
		c.counts = map[string]int64{}
	}
	c.counts[modelKey]++
	count := c.counts[modelKey]
	if count >= int64(threshold) {
		c.counts[modelKey] = 0
		return count, true, nil
	}
	return count, false, nil
}

func newPoolModelFailureFixture(account *Account) (*GatewayService, *poolModelFailureRepoStub) {
	repo := &poolModelFailureRepoStub{account: account}
	rls := &RateLimitService{accountRepo: repo, tempUnschedCache: &poolModelFailureCacheStub{}}
	return &GatewayService{accountRepo: repo, rateLimitService: rls}, repo
}

func geminiPoolAccount(mapping map[string]any) *Account {
	creds := map[string]any{"pool_mode": true}
	if mapping != nil {
		creds["model_mapping"] = mapping
	}
	return &Account{ID: 1133, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: creds}
}

func pool502() *UpstreamFailoverError {
	return &UpstreamFailoverError{StatusCode: http.StatusBadGateway, RetryableOnSameAccount: true}
}

// 池模式账号一次请求失败不该把整个账号摘掉：只计数，聚集到阈值才冷却这一个模型。
func TestTempUnscheduleRetryableErrorCoolsDownOnlyThePoolModelAfterThreshold(t *testing.T) {
	svc, repo := newPoolModelFailureFixture(geminiPoolAccount(nil))
	ctx := WithTempUnschedulableModel(context.Background(), "gemini-3.8-flash")

	for i := 1; i < poolModelFailureThreshold; i++ {
		svc.TempUnscheduleRetryableError(ctx, 1133, pool502())
	}
	require.Zero(t, repo.tempUnschedCalls, "pool failures must never ban the whole account")
	require.Empty(t, repo.modelLimits, "below the threshold nothing cools down")

	before := time.Now()
	svc.TempUnscheduleRetryableError(ctx, 1133, pool502())
	require.Zero(t, repo.tempUnschedCalls)
	require.Len(t, repo.modelLimits, 1)
	resetAt, ok := repo.modelLimits["gemini-3.8-flash"]
	require.True(t, ok)
	require.WithinDuration(t, before.Add(poolModelFailureCooldown), resetAt, 2*time.Second)
}

// 冷却 key 必须是调度器检查时用的映射后模型名，否则调度器看不到这条冷却。
func TestPoolModelCooldownUsesMappedModelKey(t *testing.T) {
	account := geminiPoolAccount(map[string]any{"gemini-3.8-flash": "gemini-3.8-flash-preview"})
	svc, repo := newPoolModelFailureFixture(account)
	ctx := WithTempUnschedulableModel(context.Background(), "gemini-3.8-flash")

	for i := 0; i < poolModelFailureThreshold; i++ {
		svc.TempUnscheduleRetryableError(ctx, account.ID, pool502())
	}
	require.Contains(t, repo.modelLimits, "gemini-3.8-flash-preview")
	require.True(t, account.IsSchedulableForModel("gemini-3.8-flash"), "before the cooldown is loaded the model is schedulable")
	account.Extra = map[string]any{modelRateLimitsKey: map[string]any{
		"gemini-3.8-flash-preview": map[string]any{"rate_limit_reset_at": repo.modelLimits["gemini-3.8-flash-preview"].UTC().Format(time.RFC3339)},
	}}
	require.False(t, account.IsSchedulableForModel("gemini-3.8-flash"), "scheduler must see the cooldown")
	require.True(t, account.IsSchedulableForModel("gemini-4-carbon"), "other models keep running")
}

// 不在新策略范围内的情况保持原有语义：非池模式账号、调用方没给模型名。
func TestTempUnscheduleRetryableErrorKeepsLegacyBanOutsidePoolPolicy(t *testing.T) {
	t.Run("非池模式 Gemini 账号仍整号临时摘掉", func(t *testing.T) {
		account := &Account{ID: 7, Platform: PlatformGemini, Type: AccountTypeOAuth, Credentials: map[string]any{}}
		svc, repo := newPoolModelFailureFixture(account)
		svc.TempUnscheduleRetryableError(WithTempUnschedulableModel(context.Background(), "gemini-3.8-flash"), 7, pool502())
		require.Equal(t, 1, repo.tempUnschedCalls)
		require.Empty(t, repo.modelLimits)
	})

	t.Run("没有模型名时不查账号、走原逻辑", func(t *testing.T) {
		svc, repo := newPoolModelFailureFixture(geminiPoolAccount(nil))
		svc.TempUnscheduleRetryableError(context.Background(), 1133, pool502())
		require.Zero(t, repo.getByIDCalls)
		require.Equal(t, 1, repo.tempUnschedCalls)
	})
}

// 拿不到共享窗口（没有 Redis 扩展）时宁可不封：单次失败已经由 failover 换号处理。
func TestPoolModelFailureWithoutSharedWindowNeverBans(t *testing.T) {
	repo := &poolModelFailureRepoStub{account: geminiPoolAccount(nil)}
	svc := &GatewayService{accountRepo: repo, rateLimitService: &RateLimitService{accountRepo: repo}}
	ctx := WithTempUnschedulableModel(context.Background(), "gemini-3.8-flash")
	for i := 0; i < poolModelFailureThreshold*2; i++ {
		svc.TempUnscheduleRetryableError(ctx, 1133, pool502())
	}
	require.Zero(t, repo.tempUnschedCalls)
	require.Empty(t, repo.modelLimits)
}

func geminiRelayAccount() *Account {
	// 生产上的 Gemini 中转号大多没开 pool_mode。
	return &Account{ID: 1133, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{}}
}

// withReplyHealth 换一个干净的进程内回复计数器，并按给定次数预置有回答 / 坏回复。
func withReplyHealth(t *testing.T, accountID int64, model string, usable, broken int) {
	t.Helper()
	prev := geminiReplyHealth
	geminiReplyHealth = newGeminiReplyHealthTracker()
	t.Cleanup(func() { geminiReplyHealth = prev })
	for i := 0; i < usable; i++ {
		geminiReplyHealth.observe(accountID, model, true)
	}
	for i := 0; i < broken; i++ {
		geminiReplyHealth.observe(accountID, model, false)
	}
}

func emptyResponseFailover() *UpstreamFailoverError {
	return &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Reason: GeminiEmptyResponseReason}
}

// 空 200 不在同账号重试，但聚集起来要冷却这个（账号, 模型），否则每个请求都先在这个号上白撞一跳。
func TestEmptyResponseCoolsDownAccountModelAfterThreshold(t *testing.T) {
	withReplyHealth(t, 1133, "gemini-4-carbon", 1, 9)
	svc, repo := newPoolModelFailureFixture(geminiRelayAccount())
	ctx := WithTempUnschedulableModel(context.Background(), "gemini-4-carbon")

	for i := 1; i < poolModelFailureThreshold; i++ {
		svc.TempUnscheduleRetryableError(ctx, 1133, emptyResponseFailover())
	}
	require.Empty(t, repo.modelLimits, "below the threshold nothing cools down")

	before := time.Now()
	svc.TempUnscheduleRetryableError(ctx, 1133, emptyResponseFailover())
	require.Zero(t, repo.tempUnschedCalls, "empty replies must never ban the whole account")
	require.Len(t, repo.modelLimits, 1)
	require.WithinDuration(t, before.Add(poolModelFailureCooldown), repo.modelLimits["gemini-4-carbon"], 2*time.Second)
}

func TestErrorIn200CoolsDownOnlyForGatewayFailures(t *testing.T) {
	t.Run("排队超时/限流（按 502 换号）计入冷却", func(t *testing.T) {
		withReplyHealth(t, 1133, "gemini-4-argon", 0, 6)
		svc, repo := newPoolModelFailureFixture(geminiRelayAccount())
		ctx := WithTempUnschedulableModel(context.Background(), "gemini-4-argon")
		for i := 0; i < poolModelFailureThreshold; i++ {
			svc.TempUnscheduleRetryableError(ctx, 1133, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Reason: GeminiErrorIn200Reason})
		}
		require.Contains(t, repo.modelLimits, "gemini-4-argon")
		require.Zero(t, repo.tempUnschedCalls)
	})

	t.Run("请求级 4xx（参数、内容策略）不算账号的问题", func(t *testing.T) {
		withReplyHealth(t, 1133, "gemini-4-argon", 0, 6)
		svc, repo := newPoolModelFailureFixture(geminiRelayAccount())
		ctx := WithTempUnschedulableModel(context.Background(), "gemini-4-argon")
		for i := 0; i < poolModelFailureThreshold*2; i++ {
			svc.TempUnscheduleRetryableError(ctx, 1133, &UpstreamFailoverError{StatusCode: http.StatusBadRequest, Reason: GeminiErrorIn200Reason})
		}
		require.Zero(t, repo.getByIDCalls)
		require.Empty(t, repo.modelLimits)
		require.Zero(t, repo.tempUnschedCalls)
	})
}

func TestNonRetryableFailuresOutsideEmptyPolicyStayUntouched(t *testing.T) {
	cases := map[string]*UpstreamFailoverError{
		"普通 5xx 换号":       {StatusCode: http.StatusBadGateway},
		"请求级瞬时故障的空回复":     {StatusCode: http.StatusBadGateway, Reason: GeminiEmptyResponseReason, RequestScopedTransient: true},
		"capability_miss": {StatusCode: http.StatusNotFound, Reason: GeminiCapabilityMissReason},
	}
	for name, failoverErr := range cases {
		t.Run(name, func(t *testing.T) {
			withReplyHealth(t, 1133, "gemini-4-carbon", 0, 10)
			svc, repo := newPoolModelFailureFixture(geminiRelayAccount())
			ctx := WithTempUnschedulableModel(context.Background(), "gemini-4-carbon")
			for i := 0; i < poolModelFailureThreshold*2; i++ {
				svc.TempUnscheduleRetryableError(ctx, 1133, failoverErr)
			}
			require.Zero(t, repo.getByIDCalls)
			require.Empty(t, repo.modelLimits)
			require.Zero(t, repo.tempUnschedCalls)
		})
	}
}

// 非 Gemini 账号不受影响（空回复原因只由 Gemini 路径产生，这里防御性地确认）。
func TestEmptyResponseCooldownIgnoresNonGeminiAccounts(t *testing.T) {
	withReplyHealth(t, 9, "gpt-5", 0, 10)
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	svc, repo := newPoolModelFailureFixture(account)
	ctx := WithTempUnschedulableModel(context.Background(), "gpt-5")
	for i := 0; i < poolModelFailureThreshold*2; i++ {
		svc.TempUnscheduleRetryableError(ctx, 9, emptyResponseFailover())
	}
	require.Empty(t, repo.modelLimits)
	require.Zero(t, repo.tempUnschedCalls)
}

// 11-23 回滚的原因：大部分时间能用的号（1132 的 3.8 成功率约 89%）偶尔一分钟回 5 次空，不能冷却。
func TestEmptyResponseDoesNotCoolMostlyHealthyAccount(t *testing.T) {
	withReplyHealth(t, 1132, "gemini-3.8-flash", 40, 5)
	account := geminiRelayAccount()
	account.ID = 1132
	svc, repo := newPoolModelFailureFixture(account)
	ctx := WithTempUnschedulableModel(context.Background(), "gemini-3.8-flash")
	for i := 0; i < poolModelFailureThreshold*3; i++ {
		svc.TempUnscheduleRetryableError(ctx, 1132, emptyResponseFailover())
	}
	require.Empty(t, repo.modelLimits, "a mostly healthy account must keep serving")
	require.Zero(t, repo.tempUnschedCalls)
}

// 池模式号的可重试错误（#187 的原有策略）不受回复计数影响。
func TestReplyHealthGateLeavesRetryablePoolFailuresAlone(t *testing.T) {
	withReplyHealth(t, 1133, "gemini-3.8-flash", 100, 0)
	svc, repo := newPoolModelFailureFixture(geminiPoolAccount(nil))
	ctx := WithTempUnschedulableModel(context.Background(), "gemini-3.8-flash")
	for i := 0; i < poolModelFailureThreshold; i++ {
		svc.TempUnscheduleRetryableError(ctx, 1133, pool502())
	}
	require.Contains(t, repo.modelLimits, "gemini-3.8-flash")
}

func TestGeminiReplyHealthWindow(t *testing.T) {
	tr := newGeminiReplyHealthTracker()
	now := time.Unix(1_800_000_000, 0)
	tr.now = func() time.Time { return now }

	require.False(t, tr.mostlyUnusable(1, "m"), "no samples, no cooldown")
	tr.observe(1, "m", false)
	tr.observe(1, "m", false)
	tr.observe(1, "m", true)
	require.True(t, tr.mostlyUnusable(1, "m"))
	require.False(t, tr.mostlyUnusable(1, "other"), "models are counted separately")
	require.False(t, tr.mostlyUnusable(2, "m"), "accounts are counted separately")

	now = now.Add(geminiReplyHealthBucket * geminiReplyHealthBuckets)
	usable, broken := tr.counts(1, "m")
	require.Zero(t, usable+broken, "old buckets fall out of the window")
	tr.observe(1, "m", true)
	require.False(t, tr.mostlyUnusable(1, "m"))
}
