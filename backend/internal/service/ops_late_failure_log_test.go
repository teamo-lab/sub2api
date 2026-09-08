package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestOpsSystemLogSink_LateFailureExactInfoAllowlist(t *testing.T) {
	sink := &OpsSystemLogSink{}
	for _, name := range []string{"exact", "field_component", "wrong_component", "component_suffix", "wrong_origin", "missing_origin", "origin_nonstring", "wrong_event", "event_suffix", "debug", "skip"} {
		t.Run(name, func(t *testing.T) {
			e := &logger.LogEvent{Level: "info", Component: openAILateFailureSwitchComponent, Message: openAILateFailureSwitchEvent, Fields: map[string]any{"origin": openAILateFailureSwitchOrigin}}
			want := false
			switch name {
			case "exact":
				want = true
			case "field_component":
				e.Component = "unrelated.logger"
				e.Fields["component"] = openAILateFailureSwitchComponent
				want = true
			case "wrong_component":
				e.Component = "handler.openai_gateway.responses"
			case "component_suffix":
				e.Component += ".unrelated"
			case "wrong_origin":
				e.Fields["origin"] = "upstream"
			case "missing_origin":
				delete(e.Fields, "origin")
			case "origin_nonstring":
				e.Fields["origin"] = stringerValue(openAILateFailureSwitchOrigin)
			case "wrong_event":
				e.Message = "openai.rule_selected"
			case "event_suffix":
				e.Message += ".extra"
			case "debug":
				e.Level = "debug"
			case "skip":
				e.Fields[logger.OpsSystemLogSkipField] = true
			}
			require.Equal(t, want, sink.shouldIndex(e))
		})
	}
}

func TestOpsSystemLogSink_LateFailureActualRecoveryLoggerPersists(t *testing.T) {
	structuredLogCaptureMu.Lock()
	defer structuredLogCaptureMu.Unlock()
	require.NoError(t, logger.Init(logger.InitOptions{Level: "info", Format: "json", ServiceName: "sub2api", Environment: "test", Output: logger.OutputOptions{ToStdout: true}, Sampling: logger.SamplingOptions{Enabled: false}}))
	var persisted []*OpsInsertSystemLogInput
	repo := &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, rows []*OpsInsertSystemLogInput) (int64, error) {
		persisted = append(persisted, rows...)
		return int64(len(rows)), nil
	}}
	sink := NewOpsSystemLogSink(repo)
	defer sink.Stop()
	logger.SetSink(sink)
	defer logger.SetSink(nil)
	c, _, a, failure := recoveryFixture(t, "apikey", 1, 2)
	gid := int64(9003)
	a.ID = 9004
	a.Extra = map[string]any{openAILateFailureSwitchEnabledKey: true, openAILateFailureSwitchGroupsKey: []int64{gid}}
	c.Set("api_key", &APIKey{ID: 9002, UserID: 9001, GroupID: &gid})
	// Model/user/key/group from the actual selected account and authenticated
	// key must override any stale context fields; request IDs follow middleware.
	requestLog := logger.L().With(zap.String("component", "http"), zap.String("request_id", "late-fixture-request"), zap.String("client_request_id", "late-fixture-client"),
		zap.Int64("user_id", -1), zap.Int64("api_key_id", -2), zap.Int64("group_id", -3), zap.String("model", "stale-model"))
	c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), requestLog))
	attempt := ErrorRecoveryAttempt{First: true, Elapsed: 90 * time.Second, UpstreamAttempts: 1, OriginalContext: c.Request.Context(), HandlerCanSwitch: true, WrittenSizeBeforeForward: OpenAICompactKeepaliveAdjustedWrittenSize(c)}
	require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecoveryAfterAttempt(c, a, "gpt-5.6-sol", failure, attempt))
	// This ordinary info has the same component/origin; it must remain excluded.
	requestLog.Info("openai.rule_selected", zap.String("component", openAILateFailureSwitchComponent), zap.String("origin", openAILateFailureSwitchOrigin))
	require.Len(t, sink.queue, 1, "the real logger must deliver exactly the allowlisted decision")
	sink.Start()
	sink.Stop()
	require.Len(t, persisted, 1)
	r := persisted[0]
	require.Equal(t, "info", r.Level)
	require.Equal(t, openAILateFailureSwitchComponent, r.Component)
	require.Equal(t, openAILateFailureSwitchEvent, r.Message)
	require.Equal(t, "late-fixture-request", r.RequestID)
	require.Equal(t, "late-fixture-client", r.ClientRequestID)
	require.Equal(t, int64(9001), *r.UserID)
	require.Equal(t, int64(9002), *r.APIKeyID)
	require.Equal(t, int64(9004), *r.AccountID)
	require.Equal(t, "openai", r.Platform)
	require.Equal(t, "gpt-5.6-sol", r.Model)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.ExtraJSON), &extra))
	require.Equal(t, openAILateFailureSwitchOrigin, extra["origin"])
	require.Equal(t, float64(9003), extra["group_id"])
	require.Equal(t, float64(7), extra["rule_id"])
	require.Equal(t, float64(90000), extra["attempt_elapsed_ms"])
	require.Equal(t, float64(1), extra["upstream_attempt_count"])
	require.Equal(t, uint64(1), sink.Health().WrittenCount)
}
