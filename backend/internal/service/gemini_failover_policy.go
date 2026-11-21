package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

// GeminiCapabilityMissMixedReason：最后一个账号是 capability_miss，但本次请求前面有账号因为别的原因失败
// （503、超时等）。由 handler 的 FailoverState 打上，换号耗尽时按上游故障映射，不透传 4xx。
const GeminiCapabilityMissMixedReason GatewayFailureReason = "capability_miss_after_other_failures"

// GeminiCapabilityMissSwitchLimitReason：最后一个账号是 capability_miss，但换号是因为次数用完结束的，
// 池里可能还有没试过、能接的账号。同样按上游故障映射，不透传 4xx。
const GeminiCapabilityMissSwitchLimitReason GatewayFailureReason = "capability_miss_switch_limit"

// GeminiCapabilityMissUnverifiedReason：选号耗尽了，但没能证明分组里每个支持该模型的账号都试过
// （剩下的可能只是暂时限流、额度不够等）。同样按上游故障映射，不透传 4xx。
const GeminiCapabilityMissUnverifiedReason GatewayFailureReason = "capability_miss_pool_unverified"

// IsGeminiCapabilityMissInconclusive 判断换号耗尽时最后一个错误是不是「没能证明所有账号都不支持」的
// capability_miss（前面有别的失败，或换号次数用完）。handler 用它把状态码改成 502。
func IsGeminiCapabilityMissInconclusive(err *UpstreamFailoverError) bool {
	return err != nil && (err.Reason == GeminiCapabilityMissMixedReason ||
		err.Reason == GeminiCapabilityMissSwitchLimitReason ||
		err.Reason == GeminiCapabilityMissUnverifiedReason)
}

// IsGeminiCapabilityMissExhausted 判断换号耗尽时的最后一个错误是否是「账号接不了」的 4xx。
// FailoverState 只在每个尝试过的账号都是 capability_miss、选号耗尽、并且 GeminiCapabilityMissCoversPool
// 证明分组里每个支持该模型的账号都试过了时保留这个 reason（否则降级成 GeminiCapabilityMissMixedReason /
// GeminiCapabilityMissSwitchLimitReason），所以返回 true 意味着所有账号都拒绝了同一个请求，
// handler 应把上游真实的 4xx 交给客户端。
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
	// gen 是当前计时器的代数。每次重新计时（或停掉计时）都加一，回调只在代数仍是自己那一代时才生效。
	// time.Timer.Stop 撤销不了已经开始执行、正在等锁的回调：首字或下一块数据恰好在阈值附近到达时，
	// 旧回调会在 observeRead 换上新计时器之后才拿到锁，没有代数校验就会把刚收到数据的流取消掉。
	gen uint64
}

func newGeminiUpstreamWatchdog(parent context.Context, firstByte, idle time.Duration) (context.Context, *geminiUpstreamWatchdog) {
	if firstByte <= 0 && idle <= 0 {
		return parent, nil
	}
	ctx, cancel := context.WithCancel(parent)
	w := &geminiUpstreamWatchdog{cancel: cancel, idle: idle}
	if firstByte > 0 {
		w.mu.Lock()
		w.armLocked(firstByte, GeminiFirstByteTimeoutReason)
		w.mu.Unlock()
	}
	return ctx, w
}

// armLocked 停掉当前计时器、作废它的回调，并按 d 启动新一代计时器。调用方持有 w.mu。
func (w *geminiUpstreamWatchdog) armLocked(d time.Duration, reason GatewayFailureReason) {
	w.stopLocked()
	gen := w.gen
	w.timer = time.AfterFunc(d, func() { w.fire(reason, gen) })
}

// stopLocked 停掉当前计时器并作废它的回调（包括已经开始执行、正在等锁的）。调用方持有 w.mu。
func (w *geminiUpstreamWatchdog) stopLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.gen++
}

func (w *geminiUpstreamWatchdog) fire(reason GatewayFailureReason, gen uint64) {
	w.mu.Lock()
	if w.done || w.reason != "" || gen != w.gen {
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
		w.stopLocked()
		return
	}
	w.armLocked(w.idle, GeminiStreamIdleTimeoutReason)
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
	w.stopLocked()
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

// geminiStreamCutMessage 是流已经开始转发后被 watchdog 截断时，写给客户端的终止错误文案。
const geminiStreamCutMessage = "Upstream stream idle timeout"

// geminiStreamCutByWatchdog 判断流式转发途中的读错误是不是 watchdog 超时造成的。
func geminiStreamCutByWatchdog(err error) bool {
	var watchdogErr *geminiWatchdogError
	return errors.As(err, &watchdogErr)
}

// 三种客户端协议各自的终止错误帧。
const (
	geminiStreamCutFrameNative   = "native"
	geminiStreamCutFrameMessages = "messages"
	geminiStreamCutFrameChat     = "chat"
)

// endGeminiStreamOnWatchdogCut 处理「内容已经转发给客户端之后，上游停了超过空闲超时」：
// 这时不能换号（会拼接两个账号的输出），也不能当成普通错误返回——那样 handler 不走 RecordUsage，
// 上游已经产生、客户端已经收到的用量就漏计了。
//
// 做法：按客户端协议写一个终止错误帧、记到 ops 流内错误，返回 true 让流处理器带着已累计的
// usage 正常返回，Forward 照常产出 ForwardResult 计费。不是 watchdog 超时的读错误返回 false，
// 保持原来的报错路径。
func endGeminiStreamOnWatchdogCut(c *gin.Context, err error, frame string) bool {
	if !geminiStreamCutByWatchdog(err) {
		return false
	}
	var payload string
	switch frame {
	case geminiStreamCutFrameMessages:
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": geminiStreamCutMessage},
		})
		payload = "event: error\ndata: " + string(b) + "\n\n"
	case geminiStreamCutFrameChat:
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"type": "upstream_error", "code": string(GeminiStreamIdleTimeoutReason), "message": geminiStreamCutMessage},
		})
		payload = "data: " + string(b) + "\n\n"
	default:
		payload = "data: " + string(geminiSyntheticErrorBody(http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", geminiStreamCutMessage)) + "\n\n"
	}
	_, _ = io.WriteString(c.Writer, payload)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	MarkOpsStreamError(c, "upstream_error", geminiStreamCutMessage, http.StatusGatewayTimeout)
	return true
}

// logGeminiStreamCut 记一条「流转发途中被空闲超时截断、按已产生用量计费」的日志。
func logGeminiStreamCut(account *Account, path string, usage *ClaudeUsage) {
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	var inputTokens, outputTokens int
	if usage != nil {
		inputTokens, outputTokens = usage.InputTokens, usage.OutputTokens
	}
	slog.Warn("gemini_stream_cut_after_output",
		"account_id", accountID,
		"path", path,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens)
}

// bufferGeminiErrorBody 在上游返回非 2xx 时，先把错误体完整读进内存，再交给后面的分类、
// 冷却和错误策略。
//
// readUpstreamErrorBody 会吞掉读错误：响应头已到、错误体读到一半被 watchdog 取消（或连接断开）时，
// 调用方只拿到空的或截断的 body。no-capacity 的 429 因此认不出来，会落到普通 429 路径把健康账号冷却掉；
// 能力类 400 认不出来时不换号，客户端拿到一个空错误。所以读错误必须在这里处理：
//   - 客户端断开：返回 ctx 错误，调用方直接停止；
//   - watchdog 超时或其它读错误：按传输层失败换号，不做任何账号状态写入。
//
// 成功时 resp.Body 被换成内存里的副本，后面多次读取都能拿到同样的内容。
func (s *GeminiMessagesCompatService) bufferGeminiErrorBody(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, watchdog *geminiUpstreamWatchdog) error {
	if resp == nil || resp.Body == nil || resp.StatusCode < 400 {
		return nil
	}
	limit := gatewayUpstreamErrorBodyReadLimit
	if s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody && s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes > int(limit) {
		limit = int64(s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	_ = resp.Body.Close()
	if err != nil {
		reason, ok := geminiTransportFailureReason(ctx, watchdog, GeminiStreamReadErrorReason)
		if !ok {
			return ctx.Err()
		}
		return geminiTransportFailover(c, account, reason, sanitizeUpstreamErrorMessage(err.Error()))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

// geminiCollectReadFailure 处理「非流式客户端、上游走流式」时收集上游流途中的读错误
// （空闲超时、连接断开）。这时客户端还没收到任何内容，换下一个账号是安全的，
// 比直接回 502 更可能拿到回答。客户端已断开则直接停止。
func geminiCollectReadFailure(ctx context.Context, c *gin.Context, account *Account, watchdog *geminiUpstreamWatchdog, err error) error {
	reason, ok := geminiTransportFailureReason(ctx, watchdog, GeminiStreamReadErrorReason)
	if !ok {
		return ctx.Err()
	}
	return geminiTransportFailover(c, account, reason, sanitizeUpstreamErrorMessage(err.Error()))
}
