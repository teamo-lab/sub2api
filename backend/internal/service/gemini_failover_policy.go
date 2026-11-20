package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Gemini 换号原因。这些失败只和当前账号（或它背后的中转）有关，同组的其它账号可能接得住，
// 所以直接换下一个账号，不在同一个账号上重试，也不冷却账号。
const (
	// GeminiCapabilityMissReason：账号接不了这个请求（模型不存在/没映射、中转不认的字段或用法），
	// 同组其它账号可能接得住。所有账号都这样时，客户端拿到最后一个上游的原始 4xx。
	GeminiCapabilityMissReason GatewayFailureReason = "capability_miss"
	// GeminiPaymentRequiredReason：上游 402（预付额度用完、欠费）。
	GeminiPaymentRequiredReason GatewayFailureReason = "payment_required"
	// GeminiUpstreamNoCapacityReason：中转自己没有可用渠道/账号（另一个 Sub2API 或 new-api 回的
	// 「No available ... accounts」「Failed to get available channel」），在同一个账号上重试没有意义。
	GeminiUpstreamNoCapacityReason GatewayFailureReason = "upstream_no_capacity"
	// GeminiUpstreamRequestErrorReason：连接层失败（拨号失败、连接被重置、TLS 失败等）。
	GeminiUpstreamRequestErrorReason GatewayFailureReason = "upstream_request_error"
	// GeminiFirstByteTimeoutReason：在 gateway.gemini_stream_first_byte_timeout 内没有收到响应体的第一个字节。
	GeminiFirstByteTimeoutReason GatewayFailureReason = "first_byte_timeout"
	// GeminiStreamIdleTimeoutReason：流里两次数据之间超过 gateway.stream_data_interval_timeout。
	GeminiStreamIdleTimeoutReason GatewayFailureReason = "stream_idle_timeout"
	// GeminiStreamReadErrorReason：在任何内容交给客户端之前读上游响应体出错。
	GeminiStreamReadErrorReason GatewayFailureReason = "stream_read_error"
)

// geminiMaxTransientRetries 是同一个账号上对连接错误、429/5xx 的最多重试次数（不含首次）。
// 重试多了只是在一个坏账号上空等（原来是 5 次、最长约 15 秒），换号更快。
const geminiMaxTransientRetries = 1

// geminiCapabilityMissPatterns 是「这个账号接不了、换个账号可能接得住」的上游错误信息（小写匹配）。
// 只放账号/中转能力差异，不放请求本身的错误（内容安全、超长、参数取值非法等）。
var geminiCapabilityMissPatterns = []string{
	"unknown provider for model",
	"model_not_found",
	"model not found",
	"model_not_supported",
	"is not supported by this model",
	"requests ending with a model turn are not supported",
	"thinking budget is not supported",
	`unknown name "propertynames"`,
	`unknown name \"propertynames\"`,
	`unknown name "const"`,
	`unknown name \"const\"`,
	`unknown name "parallel_tool_calls"`,
	`unknown name \"parallel_tool_calls\"`,
	"must include at least one parts field",
}

// geminiNoCapacityPatterns 是中转「自己没有可用资源」的错误信息（小写匹配）。
var geminiNoCapacityPatterns = []string{
	"no available gemini accounts",
	"no available accounts",
	"failed to get available channel",
	"no available channel",
	"all available accounts exhausted",
	"当前分组上游负载已饱和",
}

type geminiErrorClass int

const (
	geminiErrorDefault geminiErrorClass = iota
	geminiErrorCapabilityMiss
	geminiErrorPaymentRequired
	geminiErrorNoCapacity
)

func (k geminiErrorClass) reason() GatewayFailureReason {
	switch k {
	case geminiErrorCapabilityMiss:
		return GeminiCapabilityMissReason
	case geminiErrorPaymentRequired:
		return GeminiPaymentRequiredReason
	case geminiErrorNoCapacity:
		return GeminiUpstreamNoCapacityReason
	default:
		return ""
	}
}

// classifyGeminiUpstreamError 判断一个非 2xx 上游响应是否应该立即换号。
func classifyGeminiUpstreamError(statusCode int, body []byte) geminiErrorClass {
	lower := strings.ToLower(string(body))
	switch {
	case statusCode == http.StatusPaymentRequired:
		return geminiErrorPaymentRequired
	case statusCode == http.StatusNotFound:
		return geminiErrorCapabilityMiss
	case statusCode == http.StatusBadRequest:
		if containsAny(lower, geminiCapabilityMissPatterns...) {
			return geminiErrorCapabilityMiss
		}
	case statusCode == http.StatusTooManyRequests || statusCode == 529 || statusCode >= 500:
		if containsAny(lower, geminiNoCapacityPatterns...) {
			return geminiErrorNoCapacity
		}
	}
	return geminiErrorDefault
}

// geminiImmediateFailover 对需要立即换号的上游错误记 ops 事件并返回 UpstreamFailoverError；
// 其它错误返回 nil，由调用方按原有逻辑处理。
func (s *GeminiMessagesCompatService) geminiImmediateFailover(c *gin.Context, account *Account, statusCode int, respBody []byte, upstreamRequestID string) *UpstreamFailoverError {
	class := classifyGeminiUpstreamError(statusCode, respBody)
	if class == geminiErrorDefault {
		return nil
	}
	reason := class.reason()
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusCode,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "failover",
		Stage:              string(GatewayFailureStageInference),
		Scope:              string(GatewayFailureScopeAccount),
		Reason:             string(reason),
		Message:            upstreamMsg,
		Detail:             s.upstreamErrorDetail(respBody),
	})
	return &UpstreamFailoverError{
		StatusCode:   statusCode,
		ResponseBody: respBody,
		Stage:        GatewayFailureStageInference,
		Scope:        GatewayFailureScopeAccount,
		Reason:       reason,
	}
}

// IsGeminiCapabilityMissExhausted 判断换号耗尽时的最后一个错误是否是「账号接不了」的 4xx。
// 这种情况下每个账号都拒绝了同一个请求，handler 应把上游真实的 4xx 交给客户端。
func IsGeminiCapabilityMissExhausted(err *UpstreamFailoverError) bool {
	return err != nil &&
		err.Reason == GeminiCapabilityMissReason &&
		err.StatusCode >= 400 && err.StatusCode < 500 &&
		len(err.ResponseBody) > 0
}

// geminiSyntheticErrorBody 生成 Google 格式的错误体，供换号耗尽时的错误映射使用。
func geminiSyntheticErrorBody(code int, status, message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"status":  status,
		},
	})
	return b
}

// geminiTransportFailover 处理连接层失败、首字超时、预读读错：记 ops 事件并返回换号错误。
func geminiTransportFailover(c *gin.Context, account *Account, reason GatewayFailureReason, cause string) *UpstreamFailoverError {
	statusCode := http.StatusBadGateway
	status := "UNAVAILABLE"
	message := "Upstream request failed"
	switch reason {
	case GeminiFirstByteTimeoutReason:
		statusCode = http.StatusGatewayTimeout
		status = "DEADLINE_EXCEEDED"
		message = "Upstream first byte timeout"
	case GeminiStreamIdleTimeoutReason:
		statusCode = http.StatusGatewayTimeout
		status = "DEADLINE_EXCEEDED"
		message = "Upstream stream idle timeout"
	case GeminiStreamReadErrorReason:
		message = "Upstream response read failed"
	}
	var accountID int64
	accountName, platform := "", PlatformGemini
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		platform = account.Platform
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:     opsUpstreamProxyID(account),
		ProxyName:   opsUpstreamProxyName(account),
		Platform:    platform,
		AccountID:   accountID,
		AccountName: accountName,
		Kind:        "failover",
		Stage:       string(GatewayFailureStageInference),
		Scope:       string(GatewayFailureScopeAccount),
		Reason:      string(reason),
		Message:     sanitizeUpstreamErrorMessage(cause),
	})
	return &UpstreamFailoverError{
		StatusCode:   statusCode,
		ResponseBody: geminiSyntheticErrorBody(statusCode, status, message),
		Stage:        GatewayFailureStageInference,
		Scope:        GatewayFailureScopeAccount,
		Reason:       reason,
	}
}

// sleepGeminiBackoffCtx 和 sleepGeminiBackoff 一样退避，但客户端断开（ctx 结束）时立即返回 ctx 的错误。
func sleepGeminiBackoffCtx(ctx context.Context, attempt int) error {
	d := geminiBackoffDelay(attempt)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// geminiStreamFirstByteTimeout 返回流式上游请求的首字超时；非流式请求不限（整包生成完才有首字节）。
func (s *GeminiMessagesCompatService) geminiStreamFirstByteTimeout(upstreamStream bool) time.Duration {
	if !upstreamStream || s == nil || s.cfg == nil || s.cfg.Gateway.GeminiStreamFirstByteTimeout <= 0 {
		return 0
	}
	return time.Duration(s.cfg.Gateway.GeminiStreamFirstByteTimeout) * time.Second
}

// geminiStreamIdleTimeout 返回流式上游请求两次数据之间的最长间隔（复用 gateway.stream_data_interval_timeout）。
func (s *GeminiMessagesCompatService) geminiStreamIdleTimeout(upstreamStream bool) time.Duration {
	if !upstreamStream || s == nil || s.cfg == nil || s.cfg.Gateway.StreamDataIntervalTimeout <= 0 {
		return 0
	}
	return time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
}

// geminiUpstreamWatchdog 给一次上游请求加首字超时和流空闲超时：超时就取消这次请求的 ctx，
// 并记下原因。调用方据此区分「我们超时了（换号）」和「客户端断开（停止）」。
// 所有方法对 nil 接收者都是空操作，没有配置超时时不创建。
//
// 两段计时互不替代：
//   - 第一个响应体字节到来之前，只由首字超时（gateway.gemini_stream_first_byte_timeout）约束；
//   - 第一个字节到来之后，才开始流空闲计时（gateway.stream_data_interval_timeout）。
//
// 空闲计时故意不从「收到响应头」开始：Gemini 中转通常立刻回响应头、思考完才发第一个字节，
// 从响应头起算的空闲计时实际上就是首字超时。首字超时默认关闭（0）时，首字节前不设上限，
// 和这次改动之前一样，由 router 的首包超时兜底。原因见 gateway.gemini_stream_first_byte_timeout 的说明。
type geminiUpstreamWatchdog struct {
	cancel context.CancelFunc
	idle   time.Duration

	mu     sync.Mutex
	timer  *time.Timer
	reason GatewayFailureReason
	done   bool
}

func newGeminiUpstreamWatchdog(parent context.Context, firstByte, idle time.Duration) (context.Context, *geminiUpstreamWatchdog) {
	if firstByte <= 0 && idle <= 0 {
		return parent, nil
	}
	ctx, cancel := context.WithCancel(parent)
	w := &geminiUpstreamWatchdog{cancel: cancel, idle: idle}
	if firstByte > 0 {
		w.timer = time.AfterFunc(firstByte, func() { w.fire(GeminiFirstByteTimeoutReason) })
	}
	return ctx, w
}

func (w *geminiUpstreamWatchdog) fire(reason GatewayFailureReason) {
	w.mu.Lock()
	if w.done || w.reason != "" {
		w.mu.Unlock()
		return
	}
	w.reason = reason
	w.mu.Unlock()
	w.cancel()
}

// observeRead 在每次读到数据时调用：停掉首字计时，并重新开始空闲计时。
func (w *geminiUpstreamWatchdog) observeRead(n int) {
	if w == nil || n <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.reason != "" {
		return
	}
	if w.idle <= 0 {
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
		return
	}
	if w.timer == nil {
		w.timer = time.AfterFunc(w.idle, func() { w.fire(GeminiStreamIdleTimeoutReason) })
		return
	}
	w.timer.Stop()
	w.timer = time.AfterFunc(w.idle, func() { w.fire(GeminiStreamIdleTimeoutReason) })
}

// timedOut 返回超时原因；没有超时时 ok 为 false。
func (w *geminiUpstreamWatchdog) timedOut() (GatewayFailureReason, bool) {
	if w == nil {
		return "", false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason, w.reason != ""
}

// release 停止计时并释放这次请求的 ctx。请求处理完（包括把响应体转发完）后调用。
func (w *geminiUpstreamWatchdog) release() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	w.cancel()
}

// wrapBody 让响应体的每次读取都喂给 watchdog。
func (w *geminiUpstreamWatchdog) wrapBody(body io.ReadCloser) io.ReadCloser {
	if w == nil || body == nil {
		return body
	}
	return &geminiWatchedBody{ReadCloser: body, w: w}
}

type geminiWatchedBody struct {
	io.ReadCloser
	w *geminiUpstreamWatchdog
}

func (b *geminiWatchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.w.observeRead(n)
	if err != nil && !errors.Is(err, io.EOF) {
		if reason, ok := b.w.timedOut(); ok {
			return n, &geminiWatchdogError{reason: reason, cause: err}
		}
	}
	return n, err
}

// geminiWatchdogError 是被 watchdog 取消后读响应体得到的错误，带上超时原因。
type geminiWatchdogError struct {
	reason GatewayFailureReason
	cause  error
}

func (e *geminiWatchdogError) Error() string {
	return string(e.reason) + ": " + e.cause.Error()
}

func (e *geminiWatchdogError) Unwrap() error { return e.cause }

// geminiTransportFailureReason 判断一次上游调用（Do 或读响应体）的错误该怎么处理：
//   - 客户端断开（父 ctx 结束）：返回 ok=false，调用方停止并返回 ctx 错误；
//   - watchdog 超时：返回对应的超时原因；
//   - 其它：返回 fallback。
func geminiTransportFailureReason(parent context.Context, w *geminiUpstreamWatchdog, fallback GatewayFailureReason) (GatewayFailureReason, bool) {
	if parent != nil && parent.Err() != nil {
		return "", false
	}
	if reason, ok := w.timedOut(); ok {
		return reason, true
	}
	return fallback, true
}
