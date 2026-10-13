package service

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 跨供应商加密推理内容（invalid_encrypted_content）恢复。
//
// 客户端代理把同一 Codex 会话分给多家供应商时，请求里会带别家生成的
// reasoning.encrypted_content。上游拒绝时可能回 HTTP 400，也可能回 HTTP 200 +
// SSE response.failed。这里按 (group, session, account) 记录被拒密文摘要，
// 并记录本网关上游生成的密文来历；只对"拒绝过它/不可能解开它"的账号剥离。
//
// 按账号 Extra 开关，默认关闭：
//   - openai_encrypted_recovery_mode: off | shadow | on
//   - openai_encrypted_recovery_group_ids: 可选分组白名单
//   - openai_encrypted_recovery_retry_per_minute: 账号级重试令牌桶（默认 60）
//
// shadow 只记录"会剥哪些、会重试哪些"，不改请求，原有恢复逻辑照旧；on 时由本
// 引擎接管该账号的 HTTP（非 passthrough、非 WS）恢复。

const (
	OpenAIEncryptedRecoveryModeOff    = "off"
	OpenAIEncryptedRecoveryModeShadow = "shadow"
	OpenAIEncryptedRecoveryModeOn     = "on"

	// OpenAIEncryptedRecoveryHeader 在恢复用尽时写入响应头，供上游路由停止换渠道。
	OpenAIEncryptedRecoveryHeader          = "X-Sub2API-Encrypted-Recovery"
	OpenAIEncryptedRecoveryHeaderExhausted = "exhausted"

	accountExtraOpenAIEncryptedRecoveryMode           = "openai_encrypted_recovery_mode"
	accountExtraOpenAIEncryptedRecoveryGroupIDs       = "openai_encrypted_recovery_group_ids"
	accountExtraOpenAIEncryptedRecoveryRetryPerMinute = "openai_encrypted_recovery_retry_per_minute"

	openAIEncryptedRecoveryDefaultRetryPerMinute = 60
	openAIEncryptedRecoveryMaxEntriesPerSession  = 512
	openAIEncryptedRecoveryStoreTimeout          = 500 * time.Millisecond
	openAIEncryptedRecoveryMetricTTL             = 8 * 24 * time.Hour
	openAIEncryptedRecoveryContextKey            = "openai_encrypted_recovery"
)

// OpenAIEncryptedRecoveryMode 返回账号在该分组下的恢复模式；配置缺失或非法时为 off。
func (a *Account) OpenAIEncryptedRecoveryMode(groupID int64) string {
	if a == nil || !a.IsOpenAI() {
		return OpenAIEncryptedRecoveryModeOff
	}
	mode := strings.ToLower(strings.TrimSpace(a.getExtraString(accountExtraOpenAIEncryptedRecoveryMode)))
	if mode != OpenAIEncryptedRecoveryModeShadow && mode != OpenAIEncryptedRecoveryModeOn {
		return OpenAIEncryptedRecoveryModeOff
	}
	raw, configured := a.Extra[accountExtraOpenAIEncryptedRecoveryGroupIDs]
	if !configured || raw == nil {
		return mode
	}
	var groups []any
	switch v := raw.(type) {
	case []any:
		groups = v
	case []int64:
		for _, id := range v {
			groups = append(groups, id)
		}
	default:
		// 白名单格式不合法时按关闭处理，避免误把开关扩大到所有分组。
		return OpenAIEncryptedRecoveryModeOff
	}
	if len(groups) == 0 {
		return mode
	}
	for _, g := range groups {
		if groupID > 0 && int64(parseExtraFloat64(g)) == groupID {
			return mode
		}
	}
	return OpenAIEncryptedRecoveryModeOff
}

func (a *Account) openAIEncryptedRecoveryRetryPerMinute() int {
	if n := a.getExtraInt(accountExtraOpenAIEncryptedRecoveryRetryPerMinute); n > 0 {
		return n
	}
	return openAIEncryptedRecoveryDefaultRetryPerMinute
}

// openAIEncryptedCipherDomain 是"谁能解开这段密文"的近似：OpenAI 官方（OAuth
// 与 api.openai.com）互通；其他 API Key 上游按 base_url 主机划分。
func openAIEncryptedCipherDomain(a *Account) string {
	if a == nil {
		return ""
	}
	if a.Type == AccountTypeOAuth {
		return "openai"
	}
	base := strings.TrimSpace(a.GetOpenAIBaseURL())
	if base == "" {
		return "openai"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" {
		return "acct:" + strconv.FormatInt(a.ID, 10)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "api.openai.com" {
		return "openai"
	}
	return "host:" + host
}

func openAIEncryptedOriginDomain(origin string) string {
	if i := strings.LastIndex(origin, "#"); i >= 0 {
		return origin[:i]
	}
	return origin
}

// ---------------------------------------------------------------------------
// 上游错误解析与请求项定位
// ---------------------------------------------------------------------------

var (
	openAIEncryptedRejectionItemPattern     = regexp.MustCompile(`(?i)encrypted content for item ([A-Za-z0-9_\-.:]+)`)
	openAIEncryptedRejectionFragmentPattern = regexp.MustCompile(`(?i)encrypted content ([A-Za-z0-9_\-+/=]{1,32})\.\.\.([A-Za-z0-9_\-+/=]{1,32}) could not`)
)

type openAIEncryptedRejection struct {
	code           string
	message        string
	itemID         string
	fragHead       string
	fragTail       string
	functionOutput bool
}

// parseOpenAIEncryptedRejection 只认明确的错误码：invalid_encrypted_content，
// 以及消息里提到 encrypted 的 thinking_signature_invalid。429/503、协议类错误
// 不进入这套逻辑。payload 可以是 JSON 错误体或 SSE 终止帧。
func parseOpenAIEncryptedRejection(payload []byte) (openAIEncryptedRejection, bool) {
	if len(payload) == 0 {
		return openAIEncryptedRejection{}, false
	}
	if !gjson.ValidBytes(payload) {
		kind, terminal, ok := extractOpenAISSETerminalEvent(string(payload))
		if !ok || (kind != "response.failed" && kind != "error") {
			return openAIEncryptedRejection{}, false
		}
		payload = terminal
	}
	for _, prefix := range []string{"error", "response.error"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, prefix+".code").String()))
		message := gjson.GetBytes(payload, prefix+".message").String()
		lower := strings.ToLower(message)
		switch code {
		case "invalid_encrypted_content":
		case "thinking_signature_invalid":
			if !strings.Contains(lower, "encrypted") {
				continue
			}
		default:
			continue
		}
		rej := openAIEncryptedRejection{code: code, message: message}
		rej.functionOutput = strings.Contains(lower, "encrypted function output")
		if m := openAIEncryptedRejectionItemPattern.FindStringSubmatch(message); m != nil {
			rej.itemID = strings.TrimRight(m[1], ".:")
		} else if m := openAIEncryptedRejectionFragmentPattern.FindStringSubmatch(message); m != nil {
			rej.fragHead, rej.fragTail = m[1], m[2]
		}
		return rej, true
	}
	return openAIEncryptedRejection{}, false
}

type openAIEncryptedInputItem struct {
	index    int
	itemType string
	id       string
	// reasoning 表示顶层 reasoning 密文：唯一允许剥离的形态。
	reasoning  bool
	digest     string
	ciphers    []string
	hasSummary bool
}

type openAIEncryptedInputScan struct {
	ok                 bool
	previousResponseID bool
	storeFalse         bool
	items              []openAIEncryptedInputItem
}

func (s openAIEncryptedInputScan) reasoningItems() []openAIEncryptedInputItem {
	var out []openAIEncryptedInputItem
	for _, it := range s.items {
		if it.reasoning {
			out = append(out, it)
		}
	}
	return out
}

func (s openAIEncryptedInputScan) reasoningDigests() []string {
	var out []string
	for _, it := range s.items {
		if it.reasoning {
			out = append(out, it.digest)
		}
	}
	return out
}

func (s openAIEncryptedInputScan) hasOpaqueItems() bool {
	for _, it := range s.items {
		if !it.reasoning {
			return true
		}
	}
	return false
}

// scanOpenAIEncryptedInput 列出 input 中所有携带 encrypted_content 的项。
// 顶层 reasoning 密文可剥；compaction、消息内容块、function_call_output 等
// 其他位置的密文都是不透明状态，只用于定位，不剥。
func scanOpenAIEncryptedInput(body []byte) openAIEncryptedInputScan {
	var scan openAIEncryptedInputScan
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return scan
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return scan
	}
	scan.ok = true
	scan.previousResponseID = strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != ""
	scan.storeFalse = gjson.GetBytes(body, "store").Type == gjson.False
	if !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return scan
	}
	index := -1
	input.ForEach(func(_, item gjson.Result) bool {
		index++
		if !strings.Contains(item.Raw, `"encrypted_content"`) {
			return true
		}
		entry := openAIEncryptedInputItem{
			index:    index,
			itemType: strings.TrimSpace(item.Get("type").String()),
			id:       strings.TrimSpace(item.Get("id").String()),
		}
		top := item.Get("encrypted_content")
		if entry.itemType == "reasoning" && top.Type == gjson.String && top.String() != "" {
			entry.reasoning = true
			entry.digest = openAIEncryptedContentDigest(top.String())
			entry.ciphers = []string{top.String()}
			entry.hasSummary = openAIReasoningSummaryHasText(item.Get("summary"))
		} else {
			entry.ciphers = collectOpenAIEncryptedStrings(item, 0)
			if len(entry.ciphers) == 0 {
				return true
			}
		}
		scan.items = append(scan.items, entry)
		return true
	})
	return scan
}

func collectOpenAIEncryptedStrings(value gjson.Result, depth int) []string {
	if depth > 8 {
		return nil
	}
	var out []string
	value.ForEach(func(key, child gjson.Result) bool {
		if key.String() == "encrypted_content" && child.Type == gjson.String && child.String() != "" {
			out = append(out, child.String())
			return true
		}
		if child.IsObject() || child.IsArray() {
			out = append(out, collectOpenAIEncryptedStrings(child, depth+1)...)
		}
		return true
	})
	return out
}

func openAIReasoningSummaryHasText(summary gjson.Result) bool {
	if !summary.IsArray() {
		return false
	}
	has := false
	summary.ForEach(func(_, part gjson.Result) bool {
		if strings.TrimSpace(part.Get("text").String()) != "" {
			has = true
			return false
		}
		return true
	})
	return has
}

// 这些前缀的 item 不是 reasoning：错误点名它们却在请求里找不到时，按不可恢复处理。
func openAIEncryptedNonReasoningItemID(id string) bool {
	for _, prefix := range []string{"amsg_", "msg_", "cmp_", "fc_", "fco_", "call_", "ctc_"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

type openAIEncryptedRecoveryPlan struct {
	failFast bool
	reason   string
	targets  []openAIEncryptedInputItem
}

func (p openAIEncryptedRecoveryPlan) digests() []string {
	out := make([]string, 0, len(p.targets))
	for _, it := range p.targets {
		out = append(out, it.digest)
	}
	return out
}

// planOpenAIEncryptedRecovery 把一次拒绝归因到具体 reasoning 项。能从错误信息
// 定位（item id 或密文首尾片段）就只记对上的项；定位不到才整次请求的 reasoning
// 密文都算被拒。function_call_output / agent message / compaction 里的密文、
// 带 previous_response_id 的请求都不剥、不重试。
func planOpenAIEncryptedRecovery(rej openAIEncryptedRejection, scan openAIEncryptedInputScan) openAIEncryptedRecoveryPlan {
	if rej.functionOutput {
		return openAIEncryptedRecoveryPlan{failFast: true, reason: "function_output"}
	}
	if !scan.ok {
		return openAIEncryptedRecoveryPlan{failFast: true, reason: "input_not_array"}
	}
	var targets []openAIEncryptedInputItem
	reason := ""
	switch {
	case rej.itemID != "":
		found := false
		for _, it := range scan.items {
			if it.id != rej.itemID {
				continue
			}
			found = true
			if !it.reasoning {
				return openAIEncryptedRecoveryPlan{failFast: true, reason: "non_reasoning_item"}
			}
			targets = append(targets, it)
		}
		if !found && openAIEncryptedNonReasoningItemID(rej.itemID) {
			return openAIEncryptedRecoveryPlan{failFast: true, reason: "non_reasoning_item"}
		}
		reason = "located_item"
	case rej.fragHead != "" || rej.fragTail != "":
		opaqueHit := false
		for _, it := range scan.items {
			for _, cipher := range it.ciphers {
				if !strings.HasPrefix(cipher, rej.fragHead) || !strings.HasSuffix(cipher, rej.fragTail) {
					continue
				}
				if it.reasoning {
					targets = append(targets, it)
				} else {
					opaqueHit = true
				}
				break
			}
		}
		if len(targets) == 0 && opaqueHit {
			return openAIEncryptedRecoveryPlan{failFast: true, reason: "non_reasoning_item"}
		}
		reason = "located_fragment"
	}
	if len(targets) == 0 {
		if scan.hasOpaqueItems() {
			return openAIEncryptedRecoveryPlan{failFast: true, reason: "unlocated_with_opaque_items"}
		}
		targets = scan.reasoningItems()
		if len(targets) == 0 {
			return openAIEncryptedRecoveryPlan{failFast: true, reason: "no_reasoning_cipher"}
		}
		reason = "whole_request"
	}
	if scan.previousResponseID {
		return openAIEncryptedRecoveryPlan{failFast: true, reason: "previous_response_id", targets: targets}
	}
	return openAIEncryptedRecoveryPlan{reason: reason, targets: targets}
}

// stripOpenAIEncryptedRecoveryTargets 剥离目标 reasoning 项。store=false 时整项
// 删除，避免留下上游查不到的孤立 rs_ id；store=true（含缺省）只删
// encrypted_content，保留 id 与 summary。其余字节原样保留。
func stripOpenAIEncryptedRecoveryTargets(body []byte, targets []openAIEncryptedInputItem, storeFalse bool) (out []byte, stripped int, droppedWithSummary int, err error) {
	ordered := append([]openAIEncryptedInputItem(nil), targets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index > ordered[j].index })
	out = body
	seen := make(map[int]struct{}, len(ordered))
	for _, it := range ordered {
		if _, dup := seen[it.index]; dup {
			continue
		}
		seen[it.index] = struct{}{}
		path := "input." + strconv.Itoa(it.index)
		current := gjson.GetBytes(out, path+".encrypted_content")
		if current.Type != gjson.String || openAIEncryptedContentDigest(current.String()) != it.digest {
			continue
		}
		if storeFalse {
			out, err = sjson.DeleteBytes(out, path)
			if it.hasSummary {
				droppedWithSummary++
			}
		} else {
			out, err = sjson.DeleteBytes(out, path+".encrypted_content")
		}
		if err != nil {
			return body, 0, 0, err
		}
		stripped++
	}
	return out, stripped, droppedWithSummary, nil
}

// ---------------------------------------------------------------------------
// 请求级恢复状态
// ---------------------------------------------------------------------------

type openAIEncryptedRecoveryDecision struct {
	handled   bool
	retry     bool
	failover  bool
	exhausted bool
	reason    string
	body      []byte
	message   string
}

// openAIEncryptedRecoveryFailoverSignal 表示令牌桶耗尽：不重试，直接换账号。
type openAIEncryptedRecoveryFailoverSignal struct {
	payload []byte
	message string
}

func (*openAIEncryptedRecoveryFailoverSignal) Error() string {
	return "encrypted reasoning recovery rate limited; failing over"
}

type openAIEncryptedRecovery struct {
	store OpenAIEncryptedRecoveryStore

	// 请求级：每个客户端请求最多剥一次、重试一次（跨 failover 账号也一样）。
	retried        bool
	retryPending   bool
	retryAccountID int64
	quarantine     []string

	// 账号尝试级：每次进入 Forward 的 HTTP 循环重新武装。
	armed         bool
	mode          string
	account       *Account
	key           OpenAIEncryptedRecoveryKey
	domain        string
	retryPerMin   int
	ttl           time.Duration
	body          []byte
	decided       bool
	decision      openAIEncryptedRecoveryDecision
	outputDigests map[string]struct{}
}

func openAIEncryptedRecoveryFor(c *gin.Context) *openAIEncryptedRecovery {
	if c == nil {
		return nil
	}
	v, ok := c.Get(openAIEncryptedRecoveryContextKey)
	if !ok {
		return nil
	}
	r, _ := v.(*openAIEncryptedRecovery)
	return r
}

func armedOpenAIEncryptedRecovery(c *gin.Context) *openAIEncryptedRecovery {
	if r := openAIEncryptedRecoveryFor(c); r != nil && r.armed {
		return r
	}
	return nil
}

func (s *OpenAIGatewayService) getOpenAIEncryptedRecoveryStore() OpenAIEncryptedRecoveryStore {
	s.openaiEncryptedRecoveryOnce.Do(func() {
		if s.openaiEncryptedRecoveryStore != nil {
			return
		}
		if store, ok := s.cache.(OpenAIEncryptedRecoveryStore); ok && store != nil {
			s.openaiEncryptedRecoveryStore = store
			return
		}
		s.openaiEncryptedRecoveryStore = newMemoryOpenAIEncryptedRecoveryStore()
	})
	return s.openaiEncryptedRecoveryStore
}

// armOpenAIEncryptedRecovery 为本次账号尝试启用恢复引擎；模式为 off 时返回 nil。
func (s *OpenAIGatewayService) armOpenAIEncryptedRecovery(c *gin.Context, account *Account, sessionHash string) *openAIEncryptedRecovery {
	if s == nil || c == nil || account == nil {
		return nil
	}
	groupID := getOpenAIGroupIDFromContext(c)
	mode := account.OpenAIEncryptedRecoveryMode(groupID)
	r := openAIEncryptedRecoveryFor(c)
	if mode == OpenAIEncryptedRecoveryModeOff {
		if r != nil {
			r.disarm()
		}
		return nil
	}
	if r == nil {
		r = &openAIEncryptedRecovery{}
		c.Set(openAIEncryptedRecoveryContextKey, r)
	}
	r.store = s.getOpenAIEncryptedRecoveryStore()
	r.armed = true
	r.mode = mode
	r.account = account
	r.key = OpenAIEncryptedRecoveryKey{GroupID: groupID, SessionHash: sessionHash, AccountID: account.ID}
	r.domain = openAIEncryptedCipherDomain(account)
	r.retryPerMin = account.openAIEncryptedRecoveryRetryPerMinute()
	r.ttl = s.openAIWSSessionStickyTTL()
	r.body = nil
	r.decided = false
	r.decision = openAIEncryptedRecoveryDecision{}
	r.outputDigests = nil
	return r
}

func (r *openAIEncryptedRecovery) disarm() {
	if r == nil {
		return
	}
	r.armed = false
	r.body = nil
	r.outputDigests = nil
	r.decided = false
	r.decision = openAIEncryptedRecoveryDecision{}
}

func (r *openAIEncryptedRecovery) on() bool {
	return r != nil && r.armed && r.mode == OpenAIEncryptedRecoveryModeOn
}

// beginAttempt 记录本次发往上游的请求体；拒绝归因与剥离都基于它。
func (r *openAIEncryptedRecovery) beginAttempt(body []byte) {
	if r == nil {
		return
	}
	r.body = body
	r.decided = false
	r.decision = openAIEncryptedRecoveryDecision{}
}

func (r *openAIEncryptedRecovery) storeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), openAIEncryptedRecoveryStoreTimeout)
}

func (r *openAIEncryptedRecovery) metric(ctx context.Context, name string, labels ...string) {
	if r == nil || r.store == nil {
		return
	}
	field := name + ":acct=" + strconv.FormatInt(r.key.AccountID, 10) + ":mode=" + r.mode
	for _, label := range labels {
		field += ":" + label
	}
	_ = r.store.IncrementOpenAIEncryptedRecoveryMetric(ctx, field, openAIEncryptedRecoveryMetricTTL)
}

func (r *openAIEncryptedRecovery) logf(event string, format string, args ...any) {
	prefix := fmt.Sprintf("[OpenAI] encrypted_recovery event=%s mode=%s account_id=%d group_id=%d session=%s ",
		event, r.mode, r.key.AccountID, r.key.GroupID, truncateString(r.key.SessionHash, 12))
	logger.LegacyPrintf("service.openai_gateway", prefix+format, args...)
}

// preStrip 在发往上游前剥掉：该账号拒绝过的密文；以及来历明确、由其他密文域
// 生成的密文。来历不明的原样发送，被拒后再记。
func (r *openAIEncryptedRecovery) preStrip(body []byte) []byte {
	if r == nil || !r.armed || r.store == nil {
		return body
	}
	scan := scanOpenAIEncryptedInput(body)
	reasoning := scan.reasoningItems()
	if len(reasoning) == 0 || scan.previousResponseID || !r.key.valid() {
		return body
	}
	digests := make([]string, 0, len(reasoning))
	for _, it := range reasoning {
		digests = append(digests, it.digest)
	}
	ctx, cancel := r.storeContext()
	defer cancel()
	state, err := r.store.LoadOpenAIEncryptedRecoveryState(ctx, r.key, digests)
	if err != nil {
		r.logf("pre_strip_skip", "reason=store_error cause=%s", truncateString(err.Error(), 200))
		return body
	}
	var targets []openAIEncryptedInputItem
	rejected, foreign := 0, 0
	for _, it := range reasoning {
		if _, hit := state.Rejected[it.digest]; hit {
			targets = append(targets, it)
			rejected++
			continue
		}
		if origin := state.Provenance[it.digest]; origin != "" && r.domain != "" && openAIEncryptedOriginDomain(origin) != r.domain {
			targets = append(targets, it)
			foreign++
		}
	}
	if len(targets) == 0 {
		return body
	}
	if r.mode != OpenAIEncryptedRecoveryModeOn {
		r.metric(ctx, "pre_strip")
		r.logf("pre_strip", "would_strip=%d rejected=%d foreign=%d store_false=%v", len(targets), rejected, foreign, scan.storeFalse)
		return body
	}
	out, stripped, withSummary, err := stripOpenAIEncryptedRecoveryTargets(body, targets, scan.storeFalse)
	if err != nil || stripped == 0 {
		return body
	}
	r.metric(ctx, "pre_strip")
	if withSummary > 0 {
		r.metric(ctx, "drop_with_summary")
	}
	r.logf("pre_strip", "stripped=%d rejected=%d foreign=%d store_false=%v dropped_with_summary=%d", stripped, rejected, foreign, scan.storeFalse, withSummary)
	return out
}

// onRejection 处理一次上游拒绝（HTTP 400 JSON 或 SSE 终止事件），同一次尝试内
// 幂等。只在尚未向客户端提交任何字节时被调用。
func (r *openAIEncryptedRecovery) onRejection(c *gin.Context, payload []byte) openAIEncryptedRecoveryDecision {
	if r == nil || !r.armed {
		return openAIEncryptedRecoveryDecision{}
	}
	if r.decided {
		return r.decision
	}
	rej, ok := parseOpenAIEncryptedRejection(payload)
	if !ok {
		return openAIEncryptedRecoveryDecision{}
	}
	r.decided = true
	r.decision = r.decide(c, rej)
	return r.decision
}

func (r *openAIEncryptedRecovery) decide(c *gin.Context, rej openAIEncryptedRejection) openAIEncryptedRecoveryDecision {
	ctx, cancel := r.storeContext()
	defer cancel()
	r.metric(ctx, "invalid_encrypted_content")
	scan := scanOpenAIEncryptedInput(r.body)
	plan := planOpenAIEncryptedRecovery(rej, scan)
	if digests := plan.digests(); len(digests) > 0 && r.key.valid() && r.store != nil {
		if err := r.store.AddOpenAIEncryptedRejected(ctx, r.key, digests, openAIEncryptedRecoveryMaxEntriesPerSession, r.ttl); err != nil {
			r.logf("lineage_write_error", "cause=%s", truncateString(err.Error(), 200))
		}
	}
	d := openAIEncryptedRecoveryDecision{handled: true, message: rej.message}
	retryTargets := 0
	legacyTried := false
	if state := openAIEncryptedSemanticRetryStateFor(c); state != nil && state.tried {
		legacyTried = true
	}
	switch {
	case r.retried && r.retryPending && r.retryAccountID == r.key.AccountID:
		// 本账号剥离重试后仍被拒：只熔断执行过剥离重试的这个账号。
		d.reason = "rejected_after_retry"
		if r.key.valid() && r.store != nil {
			_ = r.store.MarkOpenAIEncryptedUnrecoverable(ctx, r.key, r.ttl)
			r.metric(ctx, "circuit_open")
		}
		r.settleRetry(ctx, "rejected_again")
	case r.retried:
		// 剥离重试发生在别的账号上（随后 failover 到这里）：不再重试，
		// 也不污染本账号的持久熔断状态。
		d.reason = "already_retried"
	case plan.failFast:
		d.reason = plan.reason
	case legacyTried:
		// 本请求已在别的账号上用过一次加密恢复重试。
		d.reason = "already_retried"
	default:
		var state OpenAIEncryptedRecoveryState
		if r.key.valid() && r.store != nil {
			var err error
			if state, err = r.store.LoadOpenAIEncryptedRecoveryState(ctx, r.key, scan.reasoningDigests()); err == nil && state.Unrecoverable {
				d.reason = "circuit_open"
				break
			}
		}
		// 上游一次只点名第一段坏密文（离线重放 Zero 实测：剥掉它后紧接着点名下一段）。
		// 为了"只剥一次、只重试一次"，重试时一并剥掉所有来历不能证明属于本密文域的
		// reasoning 密文；被点名的项之外的部分在重试成功后才隔离记入 lineage。
		targets, extra := r.expandRetryTargets(plan.targets, scan, state)
		if r.mode != OpenAIEncryptedRecoveryModeOn {
			d.reason = "shadow_would_retry"
			retryTargets = len(targets)
			break
		}
		if r.store != nil {
			// 令牌桶用独立的超时上下文，不受前面 lineage/指标调用耗时影响；存储
			// 故障时 fail closed（不重试、直接 failover），不能绕过账号级预算。
			bucketCtx, bucketCancel := r.storeContext()
			allowed, err := r.store.AllowOpenAIEncryptedRetry(bucketCtx, r.key.AccountID, r.retryPerMin, time.Now())
			bucketCancel()
			if err != nil || !allowed {
				d.reason = "rate_limited"
				if err != nil {
					d.reason = "rate_limit_unavailable"
					r.logf("rate_limit_error", "cause=%s", truncateString(err.Error(), 200))
				}
				d.failover = true
				r.metric(ctx, d.reason)
				break
			}
		}
		out, stripped, withSummary, err := stripOpenAIEncryptedRecoveryTargets(r.body, targets, scan.storeFalse)
		if err != nil || stripped == 0 {
			d.reason = "strip_noop"
			break
		}
		if withSummary > 0 {
			r.metric(ctx, "drop_with_summary")
		}
		d.retry = true
		d.body = out
		d.reason = plan.reason
		retryTargets = len(targets)
		r.retried = true
		r.retryPending = true
		r.retryAccountID = r.key.AccountID
		r.quarantine = extra
	}
	if r.mode == OpenAIEncryptedRecoveryModeOn && !d.retry && !d.failover {
		d.exhausted = true
		r.metric(ctx, "fail_fast", "reason="+d.reason)
		if c != nil && c.Writer != nil && !c.Writer.Written() {
			c.Writer.Header().Set(OpenAIEncryptedRecoveryHeader, OpenAIEncryptedRecoveryHeaderExhausted)
		}
	}
	r.logf("rejection", "action=%s reason=%s located_targets=%d retry_targets=%d items=%d store_false=%v located_item=%s located_fragment=%v",
		openAIEncryptedRecoveryAction(d), d.reason, len(plan.targets), retryTargets, len(scan.items), scan.storeFalse,
		truncateString(rej.itemID, 64), rej.fragHead != "" || rej.fragTail != "")
	return d
}

// expandRetryTargets 在被点名的项之外，补上所有来历不属于本密文域（含来历不明）
// 的 reasoning 密文；本账号/同域生成的密文保留。返回完整目标与补充部分的摘要。
func (r *openAIEncryptedRecovery) expandRetryTargets(located []openAIEncryptedInputItem, scan openAIEncryptedInputScan, state OpenAIEncryptedRecoveryState) ([]openAIEncryptedInputItem, []string) {
	targets := append([]openAIEncryptedInputItem(nil), located...)
	seen := make(map[int]struct{}, len(located))
	for _, it := range located {
		seen[it.index] = struct{}{}
	}
	var extra []string
	for _, it := range scan.reasoningItems() {
		if _, dup := seen[it.index]; dup {
			continue
		}
		if origin := state.Provenance[it.digest]; origin != "" && r.domain != "" && openAIEncryptedOriginDomain(origin) == r.domain {
			continue
		}
		targets = append(targets, it)
		extra = append(extra, it.digest)
	}
	return targets, extra
}

func openAIEncryptedRecoveryAction(d openAIEncryptedRecoveryDecision) string {
	switch {
	case d.retry:
		return "strip_retry"
	case d.failover:
		return "failover"
	case d.exhausted:
		return "fail_fast"
	default:
		return "observe"
	}
}

func (r *openAIEncryptedRecovery) settleRetry(ctx context.Context, result string) {
	if r == nil || !r.retryPending {
		return
	}
	r.retryPending = false
	if result != "success" {
		r.quarantine = nil
	}
	r.metric(ctx, "strip_retry", "result="+result)
	r.logf("strip_retry_result", "result=%s", result)
}

// observeOutput 记录上游输出里的 reasoning 密文来历（由当前账号生成）。
func (r *openAIEncryptedRecovery) observeOutput(data []byte, eventType string) {
	if r == nil || !r.armed || !bytes.Contains(data, []byte(`"encrypted_content"`)) {
		return
	}
	switch eventType {
	case "response.output_item.done":
		r.addOutputItem(gjson.GetBytes(data, "item"))
	case "response.completed", "response.done":
		gjson.GetBytes(data, "response.output").ForEach(func(_, item gjson.Result) bool {
			r.addOutputItem(item)
			return true
		})
	case "":
		gjson.GetBytes(data, "output").ForEach(func(_, item gjson.Result) bool {
			r.addOutputItem(item)
			return true
		})
	}
}

func (r *openAIEncryptedRecovery) addOutputItem(item gjson.Result) {
	if item.Get("type").String() != "reasoning" {
		return
	}
	cipher := item.Get("encrypted_content")
	if cipher.Type != gjson.String || cipher.String() == "" {
		return
	}
	if r.outputDigests == nil {
		r.outputDigests = make(map[string]struct{})
	}
	if len(r.outputDigests) >= openAIEncryptedRecoveryMaxEntriesPerSession {
		return
	}
	r.outputDigests[openAIEncryptedContentDigest(cipher.String())] = struct{}{}
}

func observeOpenAIEncryptedRecoveryOutput(c *gin.Context, data []byte, eventType string) {
	if r := armedOpenAIEncryptedRecovery(c); r != nil {
		r.observeOutput(data, eventType)
	}
}

// settleOpenAIEncryptedRecovery 在一次账号尝试结束时收尾：记录剥离重试结果、
// 写入输出密文来历，然后解除武装。
func (s *OpenAIGatewayService) settleOpenAIEncryptedRecovery(c *gin.Context, account *Account, forwardErr error) {
	r := armedOpenAIEncryptedRecovery(c)
	if r == nil {
		return
	}
	defer r.disarm()
	ctx, cancel := r.storeContext()
	defer cancel()
	if r.retryPending && account != nil && r.retryAccountID == account.ID {
		if forwardErr == nil {
			r.settleRetry(ctx, "success")
			if len(r.quarantine) > 0 && r.key.valid() && r.store != nil {
				// 重试成功后，同一段历史下一轮仍会带着这些密文：一并记入 lineage，
				// 让下一轮直接预剥，避免每轮都先被拒一次。
				_ = r.store.AddOpenAIEncryptedRejected(ctx, r.key, r.quarantine, openAIEncryptedRecoveryMaxEntriesPerSession, r.ttl)
				r.logf("quarantine", "digests=%d", len(r.quarantine))
			}
			r.quarantine = nil
		} else {
			r.settleRetry(ctx, "error")
		}
	}
	if forwardErr != nil || len(r.outputDigests) == 0 || !r.key.valid() || r.store == nil {
		return
	}
	origin := r.domain + "#" + strconv.FormatInt(r.key.AccountID, 10)
	entries := make(map[string]string, len(r.outputDigests))
	for digest := range r.outputDigests {
		entries[digest] = origin
	}
	if err := r.store.RecordOpenAIEncryptedProvenance(ctx, r.key.GroupID, r.key.SessionHash, entries, openAIEncryptedRecoveryMaxEntriesPerSession, r.ttl); err != nil {
		r.logf("provenance_write_error", "cause=%s", truncateString(err.Error(), 200))
	}
}

// clearOpenAIEncryptedRecoveryHeader 在每次账号尝试开始时清掉上一账号残留的
// exhausted 头，避免 failover 后成功的响应仍带着它。
func clearOpenAIEncryptedRecoveryHeader(c *gin.Context) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	c.Writer.Header().Del(OpenAIEncryptedRecoveryHeader)
}

// openAIEncryptedRecoverySignal 把 SSE 路径上的决策转成流处理可识别的错误：
// 重试走既有 semantic retry 通道（复用 30 秒预算与同一并发槽），令牌桶耗尽走
// failover。返回 handled=false 时由原有逻辑继续处理。
func openAIEncryptedRecoverySignal(c *gin.Context, payload []byte) (bool, error) {
	r := armedOpenAIEncryptedRecovery(c)
	if r == nil {
		return false, nil
	}
	d := r.onRejection(c, payload)
	if !d.handled || !r.on() {
		return false, nil
	}
	switch {
	case d.retry:
		return true, &openAIEncryptedSemanticRetrySignal{retryBody: d.body, reason: d.reason}
	case d.failover:
		return true, &openAIEncryptedRecoveryFailoverSignal{payload: append([]byte(nil), payload...), message: d.message}
	default:
		return true, nil
	}
}
