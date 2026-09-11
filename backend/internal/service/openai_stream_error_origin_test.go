package service

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamErrorOriginSnapshotsFirstRuleDecision(t *testing.T) {
	for _, skipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "counted_first", true: "excluded_first"}[skipped], func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			rules := &ErrorPassthroughService{}
			rules.setLocalCache([]*model.ErrorPassthroughRule{{ID: 1, Enabled: true, Platforms: []string{PlatformOpenAI}, MatchMode: "all", ErrorCodes: []int{502}, SkipMonitoring: skipped}})
			BindErrorPassthroughService(c, rules)
			origin := svc.newOpenAIStreamErrorOrigin(c, &Account{ID: 1, Platform: PlatformOpenAI}, false, "synthetic")
			c.Set(OpsSkipPassthroughKey, skipped)
			origin.stage([]byte(originFirstFailure), "first upstream capacity failure")
			origin.observeWrite(10, 10, nil)
			origin.seal()
			c.Set(OpsSkipPassthroughKey, !skipped)
			origin.stage([]byte(originLateFailure), "late context failure")
			origin.commit() // The first pending terminal is only flushed after the tail was read.
			origin.record()
			value, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			events := value.([]*OpsUpstreamErrorEvent)
			require.Len(t, events, 1)
			require.Equal(t, 502, events[0].UpstreamStatusCode)
			require.Equal(t, "first upstream capacity failure", events[0].Message)
			require.Equal(t, skipped, events[0].SkipMonitoring)
			require.Equal(t, skipped, c.GetBool(OpsSkipPassthroughKey))
		})
	}
}

func TestOpenAIStreamErrorOriginDoesNotInheritPriorAttemptExclusion(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	c.Set(OpsSkipPassthroughKey, true)
	c.Set(OpsUpstreamErrorsKey, []*OpsUpstreamErrorEvent{{AccountID: 1, UpstreamStatusCode: 503, SkipMonitoring: true}})
	policy := &ErrorPassthroughService{}
	policy.setLocalCache([]*model.ErrorPassthroughRule{{ID: 1, Enabled: true, Platforms: []string{PlatformOpenAI}, MatchMode: "all", ErrorCodes: []int{503}, SkipMonitoring: true}})
	BindErrorPassthroughService(c, policy)
	origin := svc.newOpenAIStreamErrorOrigin(c, &Account{ID: 2, Platform: PlatformOpenAI}, false, "synthetic")
	origin.stage([]byte(originFirstFailure), "first upstream capacity failure")
	origin.observeWrite(10, 10, nil)
	origin.seal()
	origin.commit()
	origin.record()
	value, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events := value.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 2)
	require.True(t, events[0].SkipMonitoring)
	require.False(t, events[1].SkipMonitoring)
	require.False(t, c.GetBool(OpsSkipPassthroughKey))
}

func TestOpenAIStreamErrorOriginCanReplaceSuppressedBareErrorBeforeSeal(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	origin := svc.newOpenAIStreamErrorOrigin(c, &Account{ID: 1, Platform: PlatformOpenAI}, true, "synthetic")
	origin.stage([]byte(originFirstFailure), "suppressed bare error")
	origin.stage([]byte(originLateFailure), "authoritative final error")
	origin.observeWrite(10, 10, nil)
	origin.seal()
	origin.record()
	_, exists := c.Get(OpsUpstreamErrorsKey)
	require.False(t, exists, "buffering alone is not a successful flush")
	origin.commit()
	origin.record()
	value, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events := value.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.Equal(t, 400, events[0].UpstreamStatusCode)
	require.Equal(t, "authoritative final error", events[0].Message)
}

func TestOpenAIStreamErrorOriginReadOrShortWriteCannotCommit(t *testing.T) {
	for _, short := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		svc := &OpenAIGatewayService{cfg: &config.Config{}}
		origin := svc.newOpenAIStreamErrorOrigin(c, &Account{ID: 1, Platform: PlatformOpenAI}, true, "synthetic")
		origin.stage([]byte(originFirstFailure), "first upstream capacity failure")
		if short {
			origin.observeWrite(9, 10, nil)
			origin.seal()
			origin.commit()
		}
		origin.record()
		_, exists := c.Get(OpsUpstreamErrorsKey)
		require.False(t, exists)
	}
}
