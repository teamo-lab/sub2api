package service

// Gemini 号的「上游走 OpenAI Chat Completions」模式。
//
// 有的 Gemini 供应商（例如 llmapi.ai，开源 LLM Gateway 换皮）只提供 OpenAI 兼容的
// /v1/chat/completions，没有原生 generateContent。账号 credentials 里写
// upstream_protocol=openai_chat 后，三条 Gemini 转发路径（原生 v1beta、Claude 兼容、
// Chat Completions 兼容）照常构造原生请求，在真正发给上游之前由这里翻成 OpenAI Chat
// 请求，收到的响应再翻回 Gemini 原生格式（JSON 或 SSE）。换号、计费、首字超时、空回复判定
// 等后续逻辑都不用改。
//
// Gemini 3 / 4 的工具调用要求历史里的 functionCall 带 thoughtSignature；OpenAI 兼容接口
// 把它放在 tool_calls[].extra_content.google.thought_signature（Google 官方 OpenAI 兼容
// 接口的写法），这里双向对上。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// GeminiUpstreamProtocolCredentialKey 选择 Gemini apikey 号调上游用的协议，缺省为原生。
	GeminiUpstreamProtocolCredentialKey = "upstream_protocol"
	// GeminiUpstreamProtocolOpenAIChat 表示上游只认 OpenAI Chat Completions。
	GeminiUpstreamProtocolOpenAIChat = "openai_chat"
	// GeminiOpenAIChatModelPrefixCredentialKey 给发往上游的模型名加前缀（例如 "google-vertex/"，
	// 用来把聚合网关锁在某一家上游）。模型映射里不能写斜杠（URL 路径段校验），所以单独配。
	GeminiOpenAIChatModelPrefixCredentialKey = "openai_chat_model_prefix"
)

// IsGeminiOpenAIChatUpstream 判断账号是否走 OpenAI Chat 上游模式。
func (a *Account) IsGeminiOpenAIChatUpstream() bool {
	if a == nil || a.Platform != PlatformGemini || a.Type != AccountTypeAPIKey {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.GetCredential(GeminiUpstreamProtocolCredentialKey)), GeminiUpstreamProtocolOpenAIChat)
}

// doGeminiUpstream 是 Gemini 转发路径发上游请求的统一入口：普通号原样交给 httpUpstream，
// OpenAI Chat 上游模式的号在这里做协议转换。
func (s *GeminiMessagesCompatService) doGeminiUpstream(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if !account.IsGeminiOpenAIChatUpstream() {
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	return doGeminiViaOpenAIChat(s.httpUpstream, req, proxyURL, account)
}

func doGeminiViaOpenAIChat(upstream HTTPUpstream, req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	model, action, ok := parseGeminiModelActionPath(req.URL)
	if !ok || req.Method != http.MethodPost {
		return geminiSyntheticErrorResponse(http.StatusNotFound, "NOT_FOUND", "this account only serves generateContent / streamGenerateContent (upstream is OpenAI chat completions)"), nil
	}
	body, err := readGeminiUpstreamRequestBody(req)
	if err != nil {
		return nil, err
	}
	if action == "countTokens" {
		payload, _ := json.Marshal(map[string]any{"totalTokens": estimateGeminiCountTokens(body)})
		return geminiSyntheticResponse(http.StatusOK, "application/json", payload), nil
	}
	stream := action == "streamGenerateContent"

	upstreamModel := strings.TrimSpace(account.GetCredential(GeminiOpenAIChatModelPrefixCredentialKey)) + model
	chatBody, err := convertGeminiRequestToOpenAIChat(body, upstreamModel, stream)
	if err != nil {
		return geminiSyntheticErrorResponse(http.StatusBadRequest, "INVALID_ARGUMENT", "failed to convert request for openai chat upstream: "+err.Error()), nil
	}

	apiKey := strings.TrimSpace(req.Header.Get("x-goog-api-key"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(account.GetCredential("api_key"))
	}
	chatURL := openAIChatCompletionsURL(req.URL)
	upstreamReq, err := newGeminiUpstreamRequest(req.Context(), http.MethodPost, chatURL, chatBody)
	if err != nil {
		return nil, err
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	if stream {
		upstreamReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := upstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	releaseGeminiUpstreamRequestBody(upstreamReq)
	if err != nil {
		return nil, err
	}
	return adaptOpenAIChatResponseToGemini(resp, model, stream, geminiRequestIncludesThoughts(body)), nil
}

// OpenAI 兼容网关不管有没有要都会回思考摘要；原生 Gemini 只有 includeThoughts=true 才回，按客户端的要求来。
func geminiRequestIncludesThoughts(body []byte) bool {
	return gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Bool() ||
		gjson.GetBytes(body, "generation_config.thinking_config.include_thoughts").Bool()
}

// parseGeminiModelActionPath 从 .../models/{model}:{action} 里取出模型和动作。
func parseGeminiModelActionPath(u *url.URL) (model, action string, ok bool) {
	if u == nil {
		return "", "", false
	}
	path := u.Path
	idx := strings.LastIndex(path, "/models/")
	if idx < 0 {
		return "", "", false
	}
	rest := path[idx+len("/models/"):]
	colon := strings.LastIndex(rest, ":")
	if colon <= 0 || colon == len(rest)-1 {
		return "", "", false
	}
	model, action = rest[:colon], rest[colon+1:]
	switch action {
	case "generateContent", "streamGenerateContent", "countTokens":
		return model, action, true
	}
	return "", "", false
}

// openAIChatCompletionsURL 用原生请求的 scheme+host+base 路径拼出 chat completions 地址：
// base_url 以 /v1 结尾时只补 /chat/completions，否则补 /v1/chat/completions。
func openAIChatCompletionsURL(u *url.URL) string {
	base := u.Path
	if idx := strings.Index(base, "/v1beta/models/"); idx >= 0 {
		base = base[:idx]
	} else if idx := strings.LastIndex(base, "/models/"); idx >= 0 {
		base = base[:idx]
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		base += "/chat/completions"
	} else {
		base += "/v1/chat/completions"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: base}).String()
}

func readGeminiUpstreamRequestBody(req *http.Request) ([]byte, error) {
	if req.GetBody != nil {
		rc, err := req.GetBody()
		if err == nil {
			defer func() { _ = rc.Close() }()
			return io.ReadAll(rc)
		}
	}
	if req.Body == nil {
		return nil, errors.New("empty gemini request body")
	}
	return io.ReadAll(req.Body)
}

func geminiSyntheticResponse(status int, contentType string, body []byte) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", contentType)
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func geminiSyntheticErrorResponse(status int, statusName, message string) *http.Response {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": message, "status": statusName}})
	return geminiSyntheticResponse(status, "application/json", payload)
}

// ---------------------------------------------------------------------------
// 请求：Gemini generateContent → OpenAI chat completions
// ---------------------------------------------------------------------------

type geminiToOpenAIState struct {
	nextID  int
	pending map[string][]string // functionCall 名字 → 还没被 functionResponse 用掉的 id
}

func (st *geminiToOpenAIState) newCallID() string {
	st.nextID++
	return "call_" + strconv.Itoa(st.nextID)
}

func (st *geminiToOpenAIState) takeCallID(name string) string {
	if ids := st.pending[name]; len(ids) > 0 {
		st.pending[name] = ids[1:]
		return ids[0]
	}
	return ""
}

func convertGeminiRequestToOpenAIChat(body []byte, model string, stream bool) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid gemini request json: %w", err)
	}
	out := map[string]any{"model": model}
	messages := make([]any, 0, 8)

	if sys := geminiInstructionText(firstPresent(req, "systemInstruction", "system_instruction")); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}

	st := &geminiToOpenAIState{pending: map[string][]string{}}
	contents, _ := req["contents"].([]any)
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		if content == nil {
			continue
		}
		role, _ := content["role"].(string)
		parts, _ := content["parts"].([]any)
		if role == "model" || role == "assistant" {
			if msg := geminiModelTurnToOpenAI(parts, st); msg != nil {
				messages = append(messages, msg)
			}
			continue
		}
		messages = append(messages, geminiUserTurnToOpenAI(parts, st)...)
	}
	if len(messages) == 0 {
		return nil, errors.New("request has no contents")
	}
	out["messages"] = messages

	if tools := geminiToolsToOpenAI(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if choice := geminiToolChoiceToOpenAI(firstPresent(req, "toolConfig", "tool_config")); choice != nil {
			out["tool_choice"] = choice
		}
	}

	if gc, _ := firstPresent(req, "generationConfig", "generation_config").(map[string]any); gc != nil {
		applyGeminiGenerationConfigToOpenAI(gc, out)
	}

	if stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func geminiInstructionText(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		parts, _ := t["parts"].([]any)
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if pm, _ := p.(map[string]any); pm != nil {
				if s, _ := pm["text"].(string); s != "" {
					texts = append(texts, s)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func geminiPartToOpenAIMedia(pm map[string]any) map[string]any {
	if inline, _ := firstPresent(pm, "inlineData", "inline_data").(map[string]any); inline != nil {
		mime, _ := firstPresent(inline, "mimeType", "mime_type").(string)
		data, _ := inline["data"].(string)
		if data == "" {
			return nil
		}
		if mime == "" {
			mime = "application/octet-stream"
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," + data}}
	}
	if file, _ := firstPresent(pm, "fileData", "file_data").(map[string]any); file != nil {
		uri, _ := firstPresent(file, "fileUri", "file_uri").(string)
		if uri == "" {
			return nil
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}
	}
	return nil
}

func geminiPartIsThoughtMap(pm map[string]any) bool {
	b, _ := pm["thought"].(bool)
	return b
}

func geminiModelTurnToOpenAI(parts []any, st *geminiToOpenAIState) map[string]any {
	var text strings.Builder
	toolCalls := make([]any, 0)
	for _, raw := range parts {
		pm, _ := raw.(map[string]any)
		if pm == nil {
			continue
		}
		if fc, _ := firstPresent(pm, "functionCall", "function_call").(map[string]any); fc != nil {
			name, _ := fc["name"].(string)
			id, _ := fc["id"].(string)
			if id == "" {
				id = st.newCallID()
			}
			st.pending[name] = append(st.pending[name], id)
			args := "{}"
			if a, ok := fc["args"]; ok && a != nil {
				if b, err := json.Marshal(a); err == nil {
					args = string(b)
				}
			}
			call := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
			if sig, _ := firstPresent(pm, "thoughtSignature", "thought_signature").(string); sig != "" {
				call["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": sig}}
			}
			toolCalls = append(toolCalls, call)
			continue
		}
		if geminiPartIsThoughtMap(pm) {
			continue // 思考摘要不回传
		}
		if s, _ := pm["text"].(string); s != "" {
			text.WriteString(s)
		}
	}
	if text.Len() == 0 && len(toolCalls) == 0 {
		return nil
	}
	msg := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		msg["content"] = text.String()
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// geminiUserTurnToOpenAI 把一个 user 轮拆成：先若干 tool 消息（functionResponse），
// 再一条 user 消息（文本、图片）。工具结果里带的图片 OpenAI 的 tool 消息装不下，
// 挪到紧随其后的 user 消息里，免得被上游静默丢掉。
func geminiUserTurnToOpenAI(parts []any, st *geminiToOpenAIState) []any {
	var out []any
	userParts := make([]any, 0, len(parts))
	toolImages := make([]any, 0)
	for _, raw := range parts {
		pm, _ := raw.(map[string]any)
		if pm == nil {
			continue
		}
		if fr, _ := firstPresent(pm, "functionResponse", "function_response").(map[string]any); fr != nil {
			name, _ := fr["name"].(string)
			id, _ := fr["id"].(string)
			if id == "" {
				id = st.takeCallID(name)
			} else {
				st.takeCallID(name)
			}
			if id == "" {
				id = st.newCallID()
			}
			content := "{}"
			if r, ok := fr["response"]; ok && r != nil {
				if b, err := json.Marshal(r); err == nil {
					content = string(b)
				}
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
			if frParts, _ := fr["parts"].([]any); len(frParts) > 0 {
				for _, fp := range frParts {
					if fpm, _ := fp.(map[string]any); fpm != nil {
						if media := geminiPartToOpenAIMedia(fpm); media != nil {
							toolImages = append(toolImages, media)
						}
					}
				}
			}
			continue
		}
		if media := geminiPartToOpenAIMedia(pm); media != nil {
			userParts = append(userParts, media)
			continue
		}
		if geminiPartIsThoughtMap(pm) {
			continue
		}
		if s, ok := pm["text"].(string); ok && s != "" {
			userParts = append(userParts, map[string]any{"type": "text", "text": s})
		}
	}
	if len(toolImages) > 0 {
		userParts = append([]any{map[string]any{"type": "text", "text": "[images returned by the tool call above]"}}, append(toolImages, userParts...)...)
	}
	if len(userParts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": collapseOpenAITextParts(userParts)})
	}
	return out
}

// 只有文本时用字符串，兼容性最好。
func collapseOpenAITextParts(parts []any) any {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm == nil || pm["type"] != "text" {
			return parts
		}
		s, _ := pm["text"].(string)
		texts = append(texts, s)
	}
	return strings.Join(texts, "\n")
}

func geminiToolsToOpenAI(v any) []any {
	tools, _ := v.([]any)
	out := make([]any, 0)
	for _, raw := range tools {
		tm, _ := raw.(map[string]any)
		if tm == nil {
			continue
		}
		decls, _ := firstPresent(tm, "functionDeclarations", "function_declarations").([]any)
		for _, d := range decls {
			dm, _ := d.(map[string]any)
			if dm == nil {
				continue
			}
			name, _ := dm["name"].(string)
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if desc, _ := dm["description"].(string); desc != "" {
				fn["description"] = desc
			}
			if params := firstPresent(dm, "parametersJsonSchema", "parameters_json_schema", "parameters"); params != nil {
				fn["parameters"] = normalizeGeminiSchemaForOpenAI(params)
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
		// googleSearch / codeExecution 等内置工具 OpenAI Chat 没有对应写法，丢弃。
	}
	return out
}

// normalizeGeminiSchemaForOpenAI 把 Gemini OpenAPI 子集里的大写类型（OBJECT/STRING…）改成
// JSON Schema 的小写写法，其余字段原样保留。
func normalizeGeminiSchemaForOpenAI(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if k == "type" {
				if s, ok := val.(string); ok {
					out[k] = strings.ToLower(s)
					continue
				}
			}
			out[k] = normalizeGeminiSchemaForOpenAI(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeGeminiSchemaForOpenAI(val)
		}
		return out
	}
	return v
}

func geminiToolChoiceToOpenAI(v any) any {
	tc, _ := v.(map[string]any)
	if tc == nil {
		return nil
	}
	fcc, _ := firstPresent(tc, "functionCallingConfig", "function_calling_config").(map[string]any)
	if fcc == nil {
		return nil
	}
	mode, _ := fcc["mode"].(string)
	switch strings.ToUpper(mode) {
	case "ANY":
		if names, _ := firstPresent(fcc, "allowedFunctionNames", "allowed_function_names").([]any); len(names) == 1 {
			if n, _ := names[0].(string); n != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": n}}
			}
		}
		return "required"
	case "NONE":
		return "none"
	case "AUTO", "VALIDATED":
		return "auto"
	}
	return nil
}

func applyGeminiGenerationConfigToOpenAI(gc map[string]any, out map[string]any) {
	copyNum := func(src, dst string) {
		if v, ok := gc[src]; ok && v != nil {
			out[dst] = v
		}
	}
	copyNum("temperature", "temperature")
	copyNum("topP", "top_p")
	copyNum("top_p", "top_p")
	copyNum("maxOutputTokens", "max_tokens")
	copyNum("max_output_tokens", "max_tokens")
	copyNum("seed", "seed")
	copyNum("presencePenalty", "presence_penalty")
	copyNum("frequencyPenalty", "frequency_penalty")
	if stops := firstPresent(gc, "stopSequences", "stop_sequences"); stops != nil {
		if arr, _ := stops.([]any); len(arr) > 0 {
			out["stop"] = arr
		}
	}

	mime, _ := firstPresent(gc, "responseMimeType", "response_mime_type").(string)
	schema := firstPresent(gc, "responseJsonSchema", "response_json_schema", "responseSchema", "response_schema")
	if strings.EqualFold(strings.TrimSpace(mime), "application/json") || schema != nil {
		if schema != nil {
			out["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": normalizeGeminiSchemaForOpenAI(schema)}}
		} else {
			out["response_format"] = map[string]any{"type": "json_object"}
		}
	}

	if tc, _ := firstPresent(gc, "thinkingConfig", "thinking_config").(map[string]any); tc != nil {
		if effort := geminiThinkingToReasoningEffort(tc); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
}

func geminiThinkingToReasoningEffort(tc map[string]any) string {
	if level, _ := firstPresent(tc, "thinkingLevel", "thinking_level").(string); level != "" {
		switch strings.ToLower(strings.TrimSpace(level)) {
		case "minimal", "low":
			return "low"
		case "medium":
			return "medium"
		case "high":
			return "high"
		}
	}
	if b, ok := firstPresent(tc, "thinkingBudget", "thinking_budget").(float64); ok {
		switch {
		case b < 0:
			return ""
		case b <= 2048:
			return "low"
		case b <= 12288:
			return "medium"
		default:
			return "high"
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 响应：OpenAI chat completions → Gemini generateContent
// ---------------------------------------------------------------------------

func adaptOpenAIChatResponseToGemini(resp *http.Response, model string, stream, includeThoughts bool) *http.Response {
	header := make(http.Header)
	for _, k := range []string{"X-Request-Id", "Retry-After", "Date"} {
		if v := resp.Header.Get(k); v != "" {
			header.Set(k, v)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		payload := openAIErrorBodyToGemini(resp.StatusCode, raw)
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: header, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload))}
	}
	if stream {
		header.Set("Content-Type", "text/event-stream; charset=utf-8")
		return &http.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: header, Body: newOpenAIChatToGeminiSSE(resp.Body, model, includeThoughts), ContentLength: -1}
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	header.Set("Content-Type", "application/json")
	if err != nil {
		payload := openAIErrorBodyToGemini(http.StatusBadGateway, []byte(err.Error()))
		return &http.Response{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway", Header: header, Body: io.NopCloser(bytes.NewReader(payload))}
	}
	payload, convErr := convertOpenAIChatCompletionToGemini(raw, model, includeThoughts)
	if convErr != nil {
		payload = openAIErrorBodyToGemini(http.StatusBadGateway, raw)
		return &http.Response{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway", Header: header, Body: io.NopCloser(bytes.NewReader(payload))}
	}
	return &http.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: header, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload))}
}

func geminiStatusNameForHTTP(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 402, 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 408, 504:
		return "DEADLINE_EXCEEDED"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 503, 529:
		return "UNAVAILABLE"
	}
	if status >= 500 {
		return "INTERNAL"
	}
	return "UNKNOWN"
}

func openAIErrorBodyToGemini(status int, raw []byte) []byte {
	msg := strings.TrimSpace(string(raw))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		switch {
		case parsed.Error.Message != "":
			msg = parsed.Error.Message
			if code, ok := parsed.Error.Code.(string); ok && code != "" {
				msg += " (" + code + ")"
			}
		case parsed.Message != "":
			msg = parsed.Message
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": msg, "status": geminiStatusNameForHTTP(status)}})
	return payload
}

func openAIFinishToGemini(reason string) string {
	switch reason {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "", "stop", "tool_calls", "function_call":
		return "STOP"
	}
	return "STOP"
}

type openAIChatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	ReasoningTokens     int `json:"reasoning_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *openAIChatUsage) toGemini() map[string]any {
	reasoning := u.CompletionTokensDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = u.ReasoningTokens
	}
	candidates := u.CompletionTokens - reasoning
	if candidates < 0 {
		candidates = 0
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	m := map[string]any{
		"promptTokenCount":     u.PromptTokens,
		"candidatesTokenCount": candidates,
		"totalTokenCount":      total,
	}
	if reasoning > 0 {
		m["thoughtsTokenCount"] = reasoning
	}
	if u.PromptTokensDetails.CachedTokens > 0 {
		m["cachedContentTokenCount"] = u.PromptTokensDetails.CachedTokens
	}
	return m
}

type openAIChatToolCall struct {
	Index        *int   `json:"index"`
	ID           string `json:"id"`
	ExtraContent struct {
		Google struct {
			ThoughtSignature string `json:"thought_signature"`
		} `json:"google"`
	} `json:"extra_content"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func geminiFunctionCallPart(name, args, signature string) map[string]any {
	var parsed any = map[string]any{}
	if strings.TrimSpace(args) != "" {
		var v any
		if err := json.Unmarshal([]byte(args), &v); err == nil && v != nil {
			parsed = v
		} else {
			parsed = map[string]any{"_raw_arguments": args}
		}
	}
	part := map[string]any{"functionCall": map[string]any{"name": name, "args": parsed}}
	if signature != "" {
		part["thoughtSignature"] = signature
	}
	return part
}

// 模型名里带聚合网关的上游前缀（google-vertex/xxx）时只留最后一段，跟客户端请求的名字对上。
func geminiModelVersionFromOpenAI(upstreamModel, fallback string) string {
	if upstreamModel == "" {
		return fallback
	}
	if idx := strings.LastIndex(upstreamModel, "/"); idx >= 0 {
		return upstreamModel[idx+1:]
	}
	return upstreamModel
}

func convertOpenAIChatCompletionToGemini(raw []byte, model string, includeThoughts bool) ([]byte, error) {
	var resp struct {
		ID      string           `json:"id"`
		Model   string           `json:"model"`
		Usage   *openAIChatUsage `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          any                  `json:"content"`
				Reasoning        string               `json:"reasoning"`
				ReasoningContent string               `json:"reasoning_content"`
				ToolCalls        []openAIChatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("openai chat response has no choices")
	}
	ch := resp.Choices[0]
	parts := make([]any, 0, 3)
	reasoning := ch.Message.Reasoning
	if reasoning == "" {
		reasoning = ch.Message.ReasoningContent
	}
	if reasoning != "" && includeThoughts {
		parts = append(parts, map[string]any{"text": reasoning, "thought": true})
	}
	if text := openAIContentText(ch.Message.Content); text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, tc := range ch.Message.ToolCalls {
		parts = append(parts, geminiFunctionCallPart(tc.Function.Name, tc.Function.Arguments, tc.ExtraContent.Google.ThoughtSignature))
	}
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": openAIFinishToGemini(ch.FinishReason),
			"index":        0,
		}},
		"modelVersion": geminiModelVersionFromOpenAI(resp.Model, model),
	}
	if resp.ID != "" {
		out["responseId"] = resp.ID
	}
	if resp.Usage != nil {
		out["usageMetadata"] = resp.Usage.toGemini()
	}
	return json.Marshal(out)
}

func openAIContentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if pm, _ := p.(map[string]any); pm != nil {
				if s, _ := pm["text"].(string); s != "" {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ---------------------------------------------------------------------------
// 流式：OpenAI chat SSE → Gemini SSE
// ---------------------------------------------------------------------------

type openAIToGeminiSSE struct {
	pr  *io.PipeReader
	src io.ReadCloser
}

func (r *openAIToGeminiSSE) Read(p []byte) (int, error) { return r.pr.Read(p) }

func (r *openAIToGeminiSSE) Close() error {
	_ = r.pr.Close()
	return r.src.Close()
}

func newOpenAIChatToGeminiSSE(src io.ReadCloser, model string, includeThoughts bool) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := pumpOpenAIChatSSEToGemini(src, pw, model, includeThoughts)
		_ = src.Close()
		_ = pw.CloseWithError(err)
	}()
	return &openAIToGeminiSSE{pr: pr, src: src}
}

type pendingToolCall struct {
	name, args, signature string
}

func pumpOpenAIChatSSEToGemini(src io.Reader, w io.Writer, model string, includeThoughts bool) error {
	reader := bufio.NewReaderSize(src, 64*1024)
	var (
		responseID   string
		modelVersion = model
		finishReason string
		usage        *openAIChatUsage
		toolCalls    = map[int]*pendingToolCall{}
		finished     bool
	)
	emit := func(chunk map[string]any) error {
		if responseID != "" {
			chunk["responseId"] = responseID
		}
		chunk["modelVersion"] = modelVersion
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	emitParts := func(parts []any) error {
		return emit(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "index": 0}}})
	}
	finish := func() error {
		if finished {
			return nil
		}
		finished = true
		parts := make([]any, 0, len(toolCalls))
		idx := make([]int, 0, len(toolCalls))
		for i := range toolCalls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			tc := toolCalls[i]
			parts = append(parts, geminiFunctionCallPart(tc.name, tc.args, tc.signature))
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"text": ""})
		}
		chunk := map[string]any{"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": openAIFinishToGemini(finishReason),
			"index":        0,
		}}}
		if usage != nil {
			chunk["usageMetadata"] = usage.toGemini()
		}
		return emit(chunk)
	}

	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				return finish()
			}
			if payload != "" {
				if err := handleOpenAIChatChunk([]byte(payload), includeThoughts, &responseID, &modelVersion, model, &finishReason, &usage, toolCalls, w, emitParts); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if finishReason != "" || usage != nil {
					return finish()
				}
				return nil // 上游没给结束标记就断了：不伪造 finishReason，交给下游按截断处理
			}
			return readErr
		}
	}
}

func handleOpenAIChatChunk(payload []byte, includeThoughts bool, responseID, modelVersion *string, model string, finishReason *string, usage **openAIChatUsage, toolCalls map[int]*pendingToolCall, w io.Writer, emitParts func([]any) error) error {
	var chunk struct {
		ID      string           `json:"id"`
		Model   string           `json:"model"`
		Usage   *openAIChatUsage `json:"usage"`
		Error   json.RawMessage  `json:"error"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content          any                  `json:"content"`
				Reasoning        string               `json:"reasoning"`
				ReasoningContent string               `json:"reasoning_content"`
				ToolCalls        []openAIChatToolCall `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil // 跳过无法解析的行（注释、心跳）
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		status := http.StatusBadGateway
		_, err := fmt.Fprintf(w, "data: %s\n\n", openAIErrorBodyToGemini(status, append(append([]byte(`{"error":`), chunk.Error...), '}')))
		return err
	}
	if chunk.ID != "" && *responseID == "" {
		*responseID = chunk.ID
	}
	if chunk.Model != "" {
		*modelVersion = geminiModelVersionFromOpenAI(chunk.Model, model)
	}
	if chunk.Usage != nil {
		*usage = chunk.Usage
	}
	for _, ch := range chunk.Choices {
		parts := make([]any, 0, 2)
		reasoning := ch.Delta.Reasoning
		if reasoning == "" {
			reasoning = ch.Delta.ReasoningContent
		}
		if reasoning != "" && includeThoughts {
			parts = append(parts, map[string]any{"text": reasoning, "thought": true})
		}
		if text := openAIContentText(ch.Delta.Content); text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		if len(parts) > 0 {
			if err := emitParts(parts); err != nil {
				return err
			}
		}
		for pos, tc := range ch.Delta.ToolCalls {
			i := pos
			if tc.Index != nil {
				i = *tc.Index
			}
			p := toolCalls[i]
			if p == nil {
				p = &pendingToolCall{}
				toolCalls[i] = p
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.args += tc.Function.Arguments
			if sig := tc.ExtraContent.Google.ThoughtSignature; sig != "" {
				p.signature = sig
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			*finishReason = *ch.FinishReason
		}
	}
	return nil
}
