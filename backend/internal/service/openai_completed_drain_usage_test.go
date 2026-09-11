package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIForwardCompletedIdleRetainsUsage(t *testing.T) {
	body := &closeUnblocksErrorBody{payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_drain\",\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}}\n\n"), closed: make(chan struct{})}
	calls := 0
	upstream := &encryptedBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}}
	svc, c, rec, account := encryptedBudgetFixture(t, false, context.Background(), upstream)
	svc.cfg.Gateway.StreamDataIntervalTimeout = 1
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"test"}`))
	require.NoError(t, err, "a completed response is not changed to failure by idle drain")
	require.NotNil(t, result)
	require.Equal(t, 17, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, "resp_drain", result.ResponseID)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
	require.NotContains(t, rec.Body.String(), "stream_timeout")
	select {
	case <-body.closed:
	default:
		t.Fatal("upstream body not closed")
	}
}

func TestOpenAIForwardPartialUsageRetainedWithoutReplay(t *testing.T) {
	upstream := &encryptedBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_partial\",\"usage\":{\"input_tokens\":17,\"output_tokens\":3},\"error\":{\"code\":\"upstream_error\",\"message\":\"failed\"}}}\n\n"))}, nil
	}}
	svc, c, _, account := encryptedBudgetFixture(t, false, context.Background(), upstream)
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"test"}`))
	require.Error(t, err)
	require.NotNil(t, result, "already parsed usage must reach handler even on failed stream")
	require.Equal(t, 17, result.Usage.InputTokens)
	var failover *UpstreamFailoverError
	require.NotErrorAs(t, err, &failover, "partial answer must not be replayed")
}

func TestOpenAIForwardUnansweredFailureDoesNotReturnBillableResult(t *testing.T) {
	upstream := &encryptedBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"upstream_error\",\"message\":\"failed\"}}}\n\n"))}, nil
	}}
	svc, c, _, account := encryptedBudgetFixture(t, false, context.Background(), upstream)
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"test"}`))
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Nil(t, result, "replayed attempt must not submit a separate usage record")
}

func TestOpenAIForwardImageFailureKeepsLegacyNilResult(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","result":"final-a"}}`,
		"",
		`data: {"type":"response.failed","response":{"id":"resp_image_failed","usage":{"input_tokens":17,"output_tokens":3},"error":{"code":"upstream_error","message":"failed"}}}`,
		"",
	}, "\n")
	upstream := &encryptedBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}}
	svc, c, _, account := encryptedBudgetFixture(t, false, context.Background(), upstream)
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-image-2","stream":true,"input":"test"}`))
	require.Error(t, err)
	require.Nil(t, result, "token-usage retention must not change partial-media error handling")
}

func TestOpenAIForwardCyberFailureKeepsDedicatedUsageOwner(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_cyber\",\"usage\":{\"input_tokens\":17,\"output_tokens\":3},\"error\":{\"code\":\"cyber_policy\",\"message\":\"blocked\"}}}\n\n"
	upstream := &encryptedBudgetUpstream{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}}
	svc, c, _, account := encryptedBudgetFixture(t, false, context.Background(), upstream)
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"test"}`))
	require.Error(t, err)
	require.NotNil(t, GetOpsCyberPolicy(c))
	require.Nil(t, result, "cyber-policy accounting must remain owned by its dedicated handler path")
}
