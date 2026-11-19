package service

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// GeminiEmptyResponseReason 标记上游返回 HTTP 200 但没有任何回答内容的失败。
// 这种响应原样转给客户端会被当成成功计费，所以按上游故障处理并换号重试。
const GeminiEmptyResponseReason GatewayFailureReason = "empty_response"

// geminiEmptyResponsePeekLimit 是判定空回复前最多缓冲的字节数。真正的空回复只有
// 几百字节；超过上限说明上游已经吐了大量数据（很长的 thought 或大图片行），不再拦截，
// 直接按原样转发。每个请求的预读内存峰值约为 limit + geminiEmptyResponsePeekChunk。
const geminiEmptyResponsePeekLimit = 1 << 20

// geminiEmptyResponsePeekChunk 是流式预读的单次读取上限，也是超长行的分段大小。
const geminiEmptyResponsePeekChunk = 32 << 10

// geminiEmptyResponseClientBody 是所有账号都返回空回复时交给 handler 的错误体。
var geminiEmptyResponseClientBody = []byte(`{"error":{"code":502,"message":"Upstream returned an empty response","status":"UNAVAILABLE"}}`)

// geminiEmptyResponseDetector 逐个观察 Gemini 响应对象（非流式整包，或流式的每个
// SSE data 帧），判断这次回复里有没有真正的回答。
//
// 空回复：没有任何非 thought 的 text（去掉首尾空白后非空）/ functionCall / inlineData
// 等内容 part，candidatesTokenCount 为 0（或只出现了空白文本），并且没有 MAX_TOKENS、SAFETY 等「有意义」的结束原因。
type geminiEmptyResponseDetector struct {
	sawEvent       bool   // 至少解析到一个 Gemini 响应对象
	sawUnknownData bool   // 有非空但无法识别为 Gemini 响应的数据（格式未知，不拦）
	hasContent     bool   // 出现了真正的回答 part
	passthrough    bool   // 出现了必须原样转发的结束原因 / 拦截 / 错误
	finishReason   string // 最近看到的 finishReason
	outputTokens   int64  // 观察到的最大 candidatesTokenCount
	sawBlankText   bool   // 出现过只有空白字符（空格/换行/制表符）的非 thought text
}

// decided 表示已经可以确定「不是空回复」，后续数据不用再缓冲。
func (d *geminiEmptyResponseDetector) decided() bool {
	return d.hasContent || d.passthrough || d.sawUnknownData
}

// empty 只在数据读完之后调用，判断整个回复是否为空。
func (d *geminiEmptyResponseDetector) empty() bool {
	if d.decided() {
		return false
	}
	// 只有空白文本时，上游通常会为这些空白计 1~2 个 candidatesTokenCount，
	// 客户端看到的仍是空白回复，同样按空回复处理。
	return d.outputTokens == 0 || d.sawBlankText
}

// observeJSON 观察一个 Gemini 响应 JSON（自动解开 Code Assist 的 response 包装）。
func (d *geminiEmptyResponseDetector) observeJSON(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	if !gjson.ValidBytes(raw) {
		d.sawUnknownData = true
		return
	}
	root := gjson.ParseBytes(raw)
	if inner := root.Get("response"); inner.Exists() && inner.IsObject() {
		root = inner
	}
	if !root.IsObject() {
		d.sawUnknownData = true
		return
	}
	d.sawEvent = true

	if root.Get("error").Exists() {
		// 流内错误交给既有逻辑处理。
		d.passthrough = true
		return
	}
	if strings.TrimSpace(root.Get("promptFeedback.blockReason").String()) != "" {
		// 提示词被拦截是真实的回答（candidates 为空也要原样返回）。
		d.passthrough = true
		return
	}

	if n := root.Get("usageMetadata.candidatesTokenCount").Int(); n > d.outputTokens {
		d.outputTokens = n
	}

	root.Get("candidates").ForEach(func(_, cand gjson.Result) bool {
		if fr := strings.TrimSpace(cand.Get("finishReason").String()); fr != "" {
			d.finishReason = fr
			if !geminiFinishReasonAllowsEmpty(fr) {
				d.passthrough = true
			}
		}
		cand.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if geminiPartHasAnswer(part) {
				d.hasContent = true
				return false
			}
			if geminiPartIsBlankText(part) {
				d.sawBlankText = true
			}
			return true
		})
		return true
	})
}

// geminiFinishReasonAllowsEmpty 返回 true 表示这个结束原因不能解释「没有内容」：
// 正常结束（STOP）或缺省。其它原因（MAX_TOKENS、SAFETY、RECITATION、
// PROHIBITED_CONTENT、MALFORMED_FUNCTION_CALL 等）都是需要原样转发的真实结果。
func geminiFinishReasonAllowsEmpty(fr string) bool {
	switch strings.ToUpper(strings.TrimSpace(fr)) {
	case "", "STOP", "FINISH_REASON_UNSPECIFIED":
		return true
	default:
		return false
	}
}

// geminiPartHasAnswer 判断一个 part 是否算作回答内容。thought part 不算。
func geminiPartHasAnswer(part gjson.Result) bool {
	if !part.IsObject() {
		return false
	}
	if part.Get("thought").Bool() {
		return false
	}
	for _, key := range []string{"functionCall", "inlineData", "fileData", "executableCode", "codeExecutionResult"} {
		if v := part.Get(key); v.Exists() && v.Type != gjson.Null {
			return true
		}
	}
	return strings.TrimSpace(part.Get("text").String()) != ""
}

// geminiPartIsBlankText 判断 part 是否为非空但只含空白字符的非 thought text。
func geminiPartIsBlankText(part gjson.Result) bool {
	if !part.IsObject() || part.Get("thought").Bool() {
		return false
	}
	text := part.Get("text").String()
	return text != "" && strings.TrimSpace(text) == ""
}

// geminiReplayBody 把已经预读的数据放回响应体前面，Close 时关闭原始响应体。
type geminiReplayBody struct {
	io.Reader
	closer io.Closer
}

func (b *geminiReplayBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

type geminiStickyErrReader struct{ err error }

func (r geminiStickyErrReader) Read([]byte) (int, error) { return 0, r.err }

// peekGeminiResponseForEmpty 在把任何字节交给客户端之前预读上游 200 响应：
//   - 流式：逐行读 SSE，直到出现第一个回答 part、必须透传的结束原因，或者流结束；
//   - 非流式：读完整个 JSON。
//
// 返回的 body 会先重放已读数据，再接着读剩余部分，调用方必须用它替换 resp.Body。
// 预读过程中出现读错误时，错误会在重放完已读数据之后原样返回给下游，不改变既有处理。
func peekGeminiResponseForEmpty(body io.ReadCloser, upstreamStream bool, limit int) (io.ReadCloser, *geminiEmptyResponseDetector, error) {
	det := &geminiEmptyResponseDetector{}
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), det, nil
	}
	if limit <= 0 {
		limit = geminiEmptyResponsePeekLimit
	}

	if !upstreamStream {
		buf, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
		replay := &geminiReplayBody{closer: body}
		if err != nil {
			replay.Reader = io.MultiReader(bytes.NewReader(buf), geminiStickyErrReader{err: err})
			return replay, det, err
		}
		if len(buf) > limit {
			// 超出预读上限：不可能是空回复，剩余部分接着读。
			det.sawUnknownData = true
			replay.Reader = io.MultiReader(bytes.NewReader(buf), body)
			return replay, det, nil
		}
		replay.Reader = bytes.NewReader(buf)
		det.observeJSON(buf)
		return replay, det, nil
	}

	// ReadSlice 每次最多返回一个缓冲区大小的片段，超长行（大 thought / inlineData、
	// 或迟迟不换行的异常上游）会被分段读入，每段之后都检查预算，内存峰值不超过
	// limit + geminiEmptyResponsePeekChunk。
	br := bufio.NewReaderSize(body, geminiEmptyResponsePeekChunk)
	var buf bytes.Buffer
	lineStart := 0
	passthrough := func() io.ReadCloser {
		return &geminiReplayBody{
			Reader: io.MultiReader(bytes.NewReader(buf.Bytes()), br),
			closer: body,
		}
	}
	for {
		frag, err := br.ReadSlice('\n')
		buf.Write(frag)
		if errors.Is(err, bufio.ErrBufferFull) {
			// 当前行还没读完。
			if buf.Len() > limit {
				det.sawUnknownData = true
				return passthrough(), det, nil
			}
			continue
		}
		if buf.Len() > lineStart {
			det.observeSSELine(buf.Bytes()[lineStart:])
			lineStart = buf.Len()
		}
		if err != nil {
			replay := &geminiReplayBody{closer: body}
			if errors.Is(err, io.EOF) {
				replay.Reader = bytes.NewReader(buf.Bytes())
				return replay, det, nil
			}
			replay.Reader = io.MultiReader(bytes.NewReader(buf.Bytes()), geminiStickyErrReader{err: err})
			return replay, det, err
		}
		if det.decided() {
			return passthrough(), det, nil
		}
		if buf.Len() > limit {
			det.sawUnknownData = true
			return passthrough(), det, nil
		}
	}
}

// observeSSELine 观察一行完整的 SSE 数据。
func (d *geminiEmptyResponseDetector) observeSSELine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			d.observeJSON(payload)
		}
		return
	}
	if !geminiSSEMetaLine(trimmed) {
		// 不是 SSE 格式（例如 JSON 数组流），无法判断，放行。
		d.sawUnknownData = true
	}
}

func geminiSSEMetaLine(line []byte) bool {
	return line[0] == ':' ||
		bytes.HasPrefix(line, []byte("event:")) ||
		bytes.HasPrefix(line, []byte("id:")) ||
		bytes.HasPrefix(line, []byte("retry:"))
}

// guardGeminiEmptyResponse 在上游 200 响应写给客户端之前检查是否为空回复。
// 是空回复时记录 ops 上游错误，并返回 UpstreamFailoverError 让 handler 换下一个账号；
// 否则把预读数据放回 resp.Body，调用方照常处理。
func (s *GeminiMessagesCompatService) guardGeminiEmptyResponse(
	c *gin.Context,
	account *Account,
	resp *http.Response,
	upstreamStream bool,
	upstreamRequestID string,
	model string,
	path string,
) error {
	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	body, det, err := peekGeminiResponseForEmpty(resp.Body, upstreamStream, geminiEmptyResponsePeekLimit)
	resp.Body = body
	if err != nil || !det.empty() {
		return nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(nil))

	var accountID int64
	accountName, platform := "", PlatformGemini
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		platform = account.Platform
	}
	logger.L().Warn("gemini.empty_response",
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.String("model", model),
		zap.String("path", path),
		zap.Bool("upstream_stream", upstreamStream),
		zap.Bool("saw_event", det.sawEvent),
		zap.String("finish_reason", det.finishReason),
		zap.String("upstream_request_id", upstreamRequestID),
	)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           platform,
		AccountID:          accountID,
		AccountName:        accountName,
		UpstreamStatusCode: http.StatusOK,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "failover",
		Stage:              string(GatewayFailureStageInference),
		Scope:              string(GatewayFailureScopeAccount),
		Reason:             string(GeminiEmptyResponseReason),
		Message:            "upstream returned HTTP 200 with an empty response",
		Detail:             "path=" + path + " model=" + model + " finish_reason=" + det.finishReason,
	})
	return &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: geminiEmptyResponseClientBody,
		Stage:        GatewayFailureStageInference,
		Scope:        GatewayFailureScopeAccount,
		Reason:       GeminiEmptyResponseReason,
	}
}
