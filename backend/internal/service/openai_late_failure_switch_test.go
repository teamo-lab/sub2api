package service

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAILateFailureSwitchScopeAndFloor(t *testing.T) {
	for _, name := range []string{"allowed_502", "allowed_503", "allowed_504", "off", "missing_enabled", "string_enabled", "missing_groups", "empty_groups", "string_group", "fractional_group", "nonpositive_group", "wrong_group", "wrong_account", "other_account_enabled", "other_group_enabled", "wrong_platform", "oauth", "unauthenticated", "body_header_spoof", "not_first", "no_handler_switch", "no_rule_switch", "no_retry", "return_mode", "default_mode", "short", "60s_boundary", "budget_floor", "budget_boundary", "429", "400", "500", "committed", "written_answer", "certified_reasoning_only", "canceled_original", "canceled_current"} {
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"group_id":3,"account_id":22}`))
			c.Request.Header.Set("X-Group-Id", "3")
			id := int64(3)
			c.Set("api_key", &APIKey{ID: 901, GroupID: &id})
			a := &Account{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openAILateFailureSwitchEnabledKey: true, openAILateFailureSwitchGroupsKey: []int64{3}}}
			failure := &UpstreamFailoverError{StatusCode: 502}
			p := &model.ErrorRecoveryPolicy{Mode: "limited", SameAccountRetries: 1, AccountSwitches: 3, BudgetSeconds: 30}
			attempt := &ErrorRecoveryAttempt{First: true, Elapsed: time.Minute, OriginalContext: context.Background(), HandlerCanSwitch: true, WrittenSizeBeforeForward: OpenAICompactKeepaliveAdjustedWrittenSize(c)}
			want := false
			switch name {
			case "allowed_502", "60s_boundary":
				want = true
			case "allowed_503":
				failure.StatusCode = 503
				want = true
			case "allowed_504":
				failure.StatusCode = 504
				want = true
			case "off":
				a.Extra[openAILateFailureSwitchEnabledKey] = false
			case "missing_enabled":
				delete(a.Extra, openAILateFailureSwitchEnabledKey)
			case "string_enabled":
				a.Extra[openAILateFailureSwitchEnabledKey] = "true"
			case "missing_groups":
				delete(a.Extra, openAILateFailureSwitchGroupsKey)
			case "empty_groups":
				a.Extra[openAILateFailureSwitchGroupsKey] = []int64{}
			case "string_group":
				a.Extra[openAILateFailureSwitchGroupsKey] = []string{"3"}
			case "fractional_group":
				a.Extra[openAILateFailureSwitchGroupsKey] = []float64{3, 3.1}
			case "nonpositive_group":
				a.Extra[openAILateFailureSwitchGroupsKey] = []int64{3, 0}
			case "wrong_group", "body_header_spoof":
				id = 2
			case "wrong_account":
				a.ID = 23
				a.Extra = nil
			case "other_account_enabled":
				a.ID = 24
				want = true
			case "other_group_enabled":
				id = 8
				a.Extra[openAILateFailureSwitchGroupsKey] = []int64{8}
				want = true
			case "wrong_platform":
				a.Platform = PlatformAnthropic
			case "oauth":
				a.Type = AccountTypeOAuth
			case "unauthenticated":
				c.Set("api_key", nil)
			case "not_first":
				attempt.First = false
			case "no_handler_switch":
				attempt.HandlerCanSwitch = false
			case "no_rule_switch":
				p.AccountSwitches = 0
			case "no_retry":
				p.SameAccountRetries = 0
			case "return_mode":
				p.Mode = "return"
			case "default_mode":
				p.Mode = "default"
			case "short":
				attempt.Elapsed = time.Minute - time.Nanosecond
			case "budget_floor":
				p.BudgetSeconds = 120
				attempt.Elapsed = 119 * time.Second
			case "budget_boundary":
				p.BudgetSeconds = 120
				attempt.Elapsed = 120 * time.Second
				want = true
			case "429":
				failure.StatusCode = 429
			case "400":
				failure.StatusCode = 400
			case "500":
				failure.StatusCode = 500
			case "committed":
				MarkResponseCommitted(c)
			case "written_answer":
				_, _ = c.Writer.WriteString("answer")
			case "certified_reasoning_only":
				_, _ = c.Writer.WriteString("reasoning only")
				failure.SafeToFailoverAfterWrite = true
				want = true
			case "canceled_original":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				attempt.OriginalContext = ctx
			case "canceled_current":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			require.Equal(t, want, openAILateFailureSwitchAllowed(c, a, failure, p, attempt))
		})
	}
}

func TestOpenAILateFailureSwitchReusesBudgetAndOnlySkipsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _, a, failure := recoveryFixture(t, "apikey", 1, 2)
		a.ID = 22
		a.Extra = map[string]any{openAILateFailureSwitchEnabledKey: true, openAILateFailureSwitchGroupsKey: []int64{3}}
		gid := int64(3)
		c.Set("api_key", &APIKey{ID: 901, GroupID: &gid})
		attempt := ErrorRecoveryAttempt{First: true, Elapsed: 5 * time.Minute, OriginalContext: c.Request.Context(), HandlerCanSwitch: true, WrittenSizeBeforeForward: OpenAICompactKeepaliveAdjustedWrittenSize(c)}
		require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecoveryAfterAttempt(c, a, "gpt", failure, attempt))
		s := recoveryState(c)
		deadline, timer, policy := s.deadline, s.timer, s.rule.RecoveryPolicy
		require.Equal(t, 1, s.switches)
		require.Empty(t, s.retries, "skipping is not an actual same-account retry")
		require.Equal(t, 1, policy.SameAccountRetries, "the cached rule remains immutable")
		// Even if a caller repeats the metadata, an existing recovery state may
		// not skip again. It consumes the original retry and original deadline.
		require.Equal(t, ErrorRecoveryRetry, ApplyErrorRecoveryAfterAttempt(c, a, "gpt", failure, attempt))
		require.Equal(t, 1, s.retries[22])
		require.Equal(t, deadline, s.deadline)
		require.Same(t, timer, s.timer)
		require.Same(t, policy, s.rule.RecoveryPolicy)
		require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, a, "gpt", failure))
		require.False(t, ReserveErrorRecoveryAccountSwitch(c), "local capacity reselect shares the exhausted switch counter")
		time.Sleep(600 * time.Millisecond)
		require.Equal(t, ErrorRecoveryStop, ApplyErrorRecoveryAfterAttempt(c, a, "gpt", failure, attempt))
		require.Equal(t, deadline, s.deadline)
		require.Equal(t, 2, s.switches)
	})
}
