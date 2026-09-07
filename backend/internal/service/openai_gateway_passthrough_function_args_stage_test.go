package service

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 这一组守住「下游只会看到完整的工具调用」：参数增量扣在 pendingLines 里，
// 直到携带完整参数的 output_item.done 一起放行；参数中途上游失败则整体丢弃
// 缓冲并换号（线上账号 69 的 overloaded 流中期失败类）。

func TestOpenAIStreamingPassthroughHoldsFunctionArgsUntilItemDone(t *testing.T) {
	preamble := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_fc"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"exec","call_id":"c1","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"cmd\":"}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}` + "\n\n" +
		"event: response.function_call_arguments.done\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"cmd\":\"ls\"}"}` + "\n\n"
	itemDone := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"exec","call_id":"c1","arguments":"{\"cmd\":\"ls\"}"}}` + "\n\n"
	terminal := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_fc","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"
	upstream := preamble + itemDone + terminal

	result, recorder, writer, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	require.NoError(t, err)
	require.NotNil(t, result)
	// 字节原样、顺序不变地到达下游
	require.Equal(t, upstream, recorder.Body.String())
	// 但第一次 flush 发生在 item.done 边界：参数增量之前一个字节都没写出
	require.NotEmpty(t, writer.flushBodyLengths)
	require.Equal(t, len(preamble)+len(itemDone), writer.flushBodyLengths[0])
	require.Equal(t, 3, result.usage.InputTokens)
}

func TestOpenAIStreamingPassthroughFailsOverWhenUpstreamDiesMidFunctionArgs(t *testing.T) {
	upstream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_fc"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"exec","call_id":"c1","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"cmd\":"}` + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_fc","status":"failed","error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later."}}}` + "\n\n"

	_, recorder, writer, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	// 半截参数一个字节都不能漏给下游
	require.Empty(t, recorder.Body.String())
	require.Empty(t, writer.flushBodyLengths)
}

func TestOpenAIStreamingPassthroughVisibleTextStillFlushesImmediately(t *testing.T) {
	first := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"
	upstream := first + "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_txt","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

	_, recorder, writer, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	require.NoError(t, err)
	require.Equal(t, upstream, recorder.Body.String())
	require.Equal(t, len(first), writer.flushBodyLengths[0])
}

func TestOpenAIStreamingPassthroughPendingCapFailsOpen(t *testing.T) {
	big := strings.Repeat("x", openAIPassthroughPendingMaxBytes)
	upstream := "event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"` + big + `"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_big","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

	_, recorder, _, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	require.NoError(t, err)
	// 超上限后放弃保护，内容仍完整送达
	require.Equal(t, upstream, recorder.Body.String())
	require.False(t, errors.As(err, new(*UpstreamFailoverError)))
}

// 裸 error 帧（上游只发 {"type":"error",...} 后关流，不补 response.failed）此前
// 从不经过透传规则判定：读取分支先置位 sawBareError，规则检查被 `!sawBareError`
// 短路。命中 skip_monitoring 的规则因此抑制不了这类失败的落库。线上回放样本：
// {"type":"error","code":"context_too_large","message":"Your input exceeds the
// context window of this model. ..."}
func TestOpenAIStreamingPassthroughBareErrorConsultsPassthroughRule(t *testing.T) {
	upstream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_ctx"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","code":"context_too_large","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","sequence_number":0}` + "\n\n"

	_, recorder, _, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	// 上下文超限不换号（isOpenAIContextWindowError），所以这里不是 failover 错误。
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "上下文超限不得触发换号")
	// 客户端仍收到合成的终态，行为不变。
	require.Contains(t, recorder.Body.String(), "response.failed")
}
