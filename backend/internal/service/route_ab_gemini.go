package service

// Gemini 分组接入选路实验（影子 / A/B）。
//
// 观测、拟合、打分、软分配、配置（settings.openai_route_ab）全部复用 openai_route_ab.go，
// 这里只有 Gemini 的适配：
//
//   - 请求特征（WithGeminiRouteRequest）：handler 解析请求体后挂到 ctx。档位 = 模型 ×
//     thinking 档 × 请求体档；非流式另成一档（首字样本取整条响应耗时，只和非流式比）。
//   - 能力需求：请求要求结构化输出（responseSchema / JSON mime）或输出限制
//     （maxOutputTokens / stopSequences）时，优先排除在 accounts.extra.route_missing_caps
//     里声明缺这项能力的号（例如不认 generationConfig 的中转）。没有号满足时不排除，
//     宁可降级也不让请求无号可用。这层过滤不依赖实验开关。
//   - 挂点：GatewayService.SelectAccountWithLoadAwareness 的粘性层记 layer=sticky，
//     负载层按评分（B 组）或老顺序（影子 / A 组）选号；首字取第一个非 thought 的回答片段。

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	// RouteCapResponseSchema：会照 responseSchema / responseMimeType=application/json 输出。
	RouteCapResponseSchema = "response_schema"
	// RouteCapOutputLimits：会照 maxOutputTokens / stopSequences 截断。
	RouteCapOutputLimits = "output_limits"

	// AccountRouteMissingCapsExtraKey：accounts.extra 里声明该号缺的能力（字符串数组或逗号分隔）。
	AccountRouteMissingCapsExtraKey = "route_missing_caps"

	routeShapeNonStreamSuffix = "|ns"
)

// GeminiRouteRequest 是一次 Gemini 请求在选路里用到的特征。
type GeminiRouteRequest struct {
	Thinking  string
	Stream    bool
	BodyBytes int
	Needs     []string
}

type geminiRouteRequestContextKey struct{}

// WithGeminiRouteRequest 把请求特征挂到 ctx；只有挂了特征的请求才参与 Gemini 选路实验
// 和能力过滤。
func WithGeminiRouteRequest(ctx context.Context, req GeminiRouteRequest) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, geminiRouteRequestContextKey{}, req)
}

func geminiRouteRequestFromContext(ctx context.Context) (GeminiRouteRequest, bool) {
	if ctx == nil {
		return GeminiRouteRequest{}, false
	}
	req, ok := ctx.Value(geminiRouteRequestContextKey{}).(GeminiRouteRequest)
	return req, ok
}

// ParseGeminiNativeRouteRequest 解析原生 generateContent / streamGenerateContent 请求体。
func ParseGeminiNativeRouteRequest(body []byte, stream bool) GeminiRouteRequest {
	req := GeminiRouteRequest{Stream: stream, BodyBytes: len(body)}
	gc := gjson.GetBytes(body, "generationConfig")
	if !gc.Exists() {
		gc = gjson.GetBytes(body, "generation_config")
	}
	if !gc.IsObject() {
		return req
	}
	field := func(camel, snake string) gjson.Result {
		if v := gc.Get(camel); v.Exists() {
			return v
		}
		return gc.Get(snake)
	}
	tc := field("thinkingConfig", "thinking_config")
	if level := strings.ToLower(strings.TrimSpace(firstExisting(tc, "thinkingLevel", "thinking_level").String())); level != "" {
		req.Thinking = level
	} else if b := firstExisting(tc, "thinkingBudget", "thinking_budget"); b.Exists() {
		req.Thinking = geminiThinkingBudgetBand(b.Int())
	}
	mime := strings.ToLower(strings.TrimSpace(field("responseMimeType", "response_mime_type").String()))
	if mime == "application/json" || field("responseSchema", "response_schema").Exists() ||
		field("responseJsonSchema", "response_json_schema").Exists() || field("_responseJsonSchema", "_response_json_schema").Exists() {
		req.Needs = append(req.Needs, RouteCapResponseSchema)
	}
	if field("maxOutputTokens", "max_output_tokens").Exists() || field("stopSequences", "stop_sequences").Exists() {
		req.Needs = append(req.Needs, RouteCapOutputLimits)
	}
	return req
}

// ParseChatCompletionsRouteRequest 解析 OpenAI 兼容的 /v1/chat/completions 请求体（转发给 Gemini 号时用）。
func ParseChatCompletionsRouteRequest(body []byte, stream bool) GeminiRouteRequest {
	req := GeminiRouteRequest{Stream: stream, BodyBytes: len(body)}
	req.Thinking = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String()))
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "response_format.type").String())) {
	case "json_schema", "json_object":
		req.Needs = append(req.Needs, RouteCapResponseSchema)
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "stop"} {
		if v := gjson.GetBytes(body, key); v.Exists() && v.Type != gjson.Null {
			req.Needs = append(req.Needs, RouteCapOutputLimits)
			break
		}
	}
	return req
}

func firstExisting(obj gjson.Result, keys ...string) gjson.Result {
	for _, k := range keys {
		if v := obj.Get(k); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}

// geminiThinkingBudgetBand 把 thinkingBudget 分档，避免每个预算值各成一格。
func geminiThinkingBudgetBand(budget int64) string {
	switch {
	case budget < 0:
		return "auto"
	case budget == 0:
		return "off"
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// RouteMissingCapability 返回账号是否声明缺少某项能力。
func (a *Account) RouteMissingCapability(capability string) bool {
	if a == nil || a.Extra == nil || capability == "" {
		return false
	}
	switch v := a.Extra[AccountRouteMissingCapsExtraKey].(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.EqualFold(strings.TrimSpace(s), capability) {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if strings.EqualFold(strings.TrimSpace(s), capability) {
				return true
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(s), capability) {
				return true
			}
		}
	}
	return false
}

// routeAccountFitsRequest 判断账号是否具备 ctx 里请求声明需要的全部能力。
func routeAccountFitsRequest(ctx context.Context, account *Account) bool {
	req, ok := geminiRouteRequestFromContext(ctx)
	if !ok || len(req.Needs) == 0 {
		return true
	}
	for _, need := range req.Needs {
		if account.RouteMissingCapability(need) {
			return false
		}
	}
	return true
}

// preferRouteFittingAccounts 只留下能力满足的号；一个都没有时原样返回（降级而不是无号可用）。
func preferRouteFittingAccounts(ctx context.Context, candidates []*Account) []*Account {
	req, ok := geminiRouteRequestFromContext(ctx)
	if !ok || len(req.Needs) == 0 || len(candidates) == 0 {
		return candidates
	}
	fitting := make([]*Account, 0, len(candidates))
	for _, acc := range candidates {
		if routeAccountFitsRequest(ctx, acc) {
			fitting = append(fitting, acc)
		}
	}
	if len(fitting) == 0 {
		return candidates
	}
	return fitting
}

// routeStats 是 GatewayService（Gemini）自己的一份实时观测，和 OpenAI 的分开（账号不重叠）。
func (s *GatewayService) routeStats() *openAIRouteStats {
	if s == nil {
		return nil
	}
	if st := s.routeStatsState.Load(); st != nil {
		return st
	}
	s.routeStatsState.CompareAndSwap(nil, newOpenAIRouteStats())
	return s.routeStatsState.Load()
}

func (s *GatewayService) routeSettingRepo() SettingRepository {
	if s == nil || s.settingService == nil {
		return nil
	}
	return s.settingService.settingRepo
}

// newGeminiRouteSelection 判断本次 Gemini 选号是否在实验里；不在（非 Gemini 平台、ctx 没有
// 请求特征、分组没配置）时返回 nil，调用方保持老逻辑。
func (s *GatewayService) newGeminiRouteSelection(ctx context.Context, group *Group, groupID *int64, platform, sessionHash, requestedModel string) *openAIRouteSelection {
	if s == nil || groupID == nil || platform != PlatformGemini {
		return nil
	}
	req, ok := geminiRouteRequestFromContext(ctx)
	if !ok {
		return nil
	}
	cfg, enabled := loadRouteABConfig(ctx, s.routeSettingRepo())
	if !enabled {
		return nil
	}
	groupCfg, ok := cfg.groupConfig(groupID)
	if !ok {
		return nil
	}
	groupRate := 0.0
	if group != nil && group.ID == *groupID {
		groupRate = group.RateMultiplier
	}
	model := strings.TrimSpace(requestedModel)
	shape := openAIRouteShape(model, req.Thinking, openAIRouteBodyBand(int64(req.BodyBytes)))
	if !req.Stream {
		shape += routeShapeNonStreamSuffix
	}
	return newRouteSelection(s.routeStats(), cfg.forGroup(groupCfg, groupRate), groupCfg, sessionHash, model, shape)
}

// StartRouteAttempt 在转发前调用（Gemini 入口），语义同 StartOpenAIRouteAttempt。
func (s *GatewayService) StartRouteAttempt(c *gin.Context, selection *AccountSelectionResult, stream bool) {
	if s == nil || c == nil {
		return
	}
	startRouteAttempt(c, selection, stream, s.routeStats, func() openAIRouteABConfig {
		cfg, _ := loadRouteABConfig(c.Request.Context(), s.routeSettingRepo())
		return cfg
	})
}

// FinishRouteAttempt 在转发返回后调用；非流式成功时把整条响应耗时记作本档的首字样本。
func (s *GatewayService) FinishRouteAttempt(c *gin.Context, stream, success bool) {
	t := openAIRouteTicketFromGin(c)
	if t == nil {
		return
	}
	now := time.Now()
	if success && !stream {
		t.markFirstToken(now)
	}
	t.finish(success, now)
}

// RouteDecisionJSON 返回最近一次尝试的决策 JSON（写 usage_logs.route_decision）。
func RouteDecisionJSON(c *gin.Context) []byte {
	return OpenAIRouteDecisionJSON(c)
}

// markGeminiRouteFirstTokenIfAnswer 在 Gemini 流式事件里遇到第一个回答片段（非 thought）时记首字。
// 不在实验里或已经记过时不解析事件。
func markGeminiRouteFirstTokenIfAnswer(c *gin.Context, payload []byte) {
	t := openAIRouteTicketFromGin(c)
	if t == nil || !t.wantsFirstToken() {
		return
	}
	if geminiPayloadHasAnswer(payload) {
		t.markFirstToken(time.Now())
	}
}

func (t *openAIRouteTicket) wantsFirstToken() bool {
	if t == nil || t.stats == nil {
		return false
	}
	t.stats.mu.Lock()
	defer t.stats.mu.Unlock()
	return !t.done && t.firstAt.IsZero()
}

// geminiPayloadHasAnswer 判断一个 Gemini 响应片段里是否有回答内容（thought 不算）。
func geminiPayloadHasAnswer(payload []byte) bool {
	found := false
	gjson.GetBytes(payload, "candidates").ForEach(func(_, cand gjson.Result) bool {
		cand.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if geminiPartHasAnswer(part) {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

// sortAvailableByLegacyOrder 按老逻辑的偏好排好负载层候选：priority → 负载率 → LRU。
// 评分只重排参与评分的号，其余号按这个顺序排在后面兜底。
func sortAvailableByLegacyOrder(available []accountWithLoad, preferOAuth bool) {
	sort.SliceStable(available, func(i, j int) bool {
		a, b := available[i], available[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		if a.loadInfo != nil && b.loadInfo != nil && a.loadInfo.LoadRate != b.loadInfo.LoadRate {
			return a.loadInfo.LoadRate < b.loadInfo.LoadRate
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt == nil && b.account.LastUsedAt == nil:
			return preferOAuth && a.account.Type == AccountTypeOAuth && b.account.Type != AccountTypeOAuth
		default:
			return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
		}
	})
}

// withRouteDecision 把决策（复制一份，填上实际选中的号）挂到选号结果上。
func withRouteDecision(res *AccountSelectionResult, err error, d *OpenAIRouteDecision) (*AccountSelectionResult, error) {
	if err != nil || res == nil || d == nil || res.Account == nil {
		return res, err
	}
	decision := *d
	decision.Picked = res.Account.ID
	res.RouteDecision = &decision
	return res, nil
}
