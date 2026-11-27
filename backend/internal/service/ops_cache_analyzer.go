package service

import (
	"container/list"
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/cespare/xxhash/v2"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// 缓存命中根因分析。
//
// 流程：网关 handler 在请求成功、提交 usage 记录前把请求体转成 IR（ops_cache_ir.go），
// 以 CacheProbe 挂进 request context；usage 落库的统一出口 writeUsageLogBestEffort 把
// (probe, usage) 投进本分析器的有界队列（满了直接丢，绝不阻塞计费）。分析 worker 对着 IR 查
// 前缀存储，给这次请求的 prompt token 归因，再按 5 分钟桶累加，定期写进 ops_cache_analysis_5m。
//
// 前缀存储分两层：
//   - L1：进程内分片 LRU（容量可配），绝大多数查找在这里完成；
//   - L2：Redis，带 TTL 的写回缓存。蓝绿切换、重启、多进程之间靠它接上同一段对话的状态。
//
// 有状态协议（OpenAI Responses 的 previous_response_id）额外维护 response_id → 前缀 的状态表：
// 本轮完成时登记 response_id，下一轮带 previous_response_id 进来时据此接上上一轮的前缀。

// CacheCause 是一次请求 prompt token 的缓存归因。
type CacheCause string

const (
	CacheCauseHit           CacheCause = "hit"            // 命中（cache_read 部分）
	CacheCauseIncremental   CacheCause = "incremental"    // 命中请求里新追加、本来就不可能命中的部分
	CacheCauseSingleTurn    CacheCause = "single_turn"    // 单轮请求，没有可复用的历史
	CacheCauseBelowMin      CacheCause = "below_min"      // prompt 低于上游可缓存的最小长度
	CacheCauseNoBreakpoint  CacheCause = "no_breakpoint"  // Anthropic 请求没声明 cache_control
	CacheCauseNewSession    CacheCause = "new_session"    // 多轮对话但之前没见过它的前缀（冷启动 / 别处来的历史）
	CacheCauseStateLost     CacheCause = "state_lost"     // previous_response_id 指向的上一轮不在观测窗口
	CacheCauseTTLExpired    CacheCause = "ttl_expired"    // 前缀见过，但离上次已超过上游缓存 TTL
	CacheCauseAccountSwitch CacheCause = "account_switch" // 前缀上次在别的号上，换号导致缓存不在
	CacheCausePrefixDrift   CacheCause = "prefix_drift"   // 同一会话的早期内容变了（时间戳、工具顺序等）
	CacheCauseUpstreamMiss  CacheCause = "upstream_miss"  // 同号、同前缀、TTL 内仍未命中：上游没缓存
	CacheCauseUnanalyzed    CacheCause = "unanalyzed"     // 没有请求体可分析（WS、图片、超长、采样跳过）
)

// AllCacheCauses 按展示顺序列出全部归因。
var AllCacheCauses = []CacheCause{
	CacheCauseHit, CacheCauseIncremental, CacheCauseSingleTurn, CacheCauseBelowMin, CacheCauseNoBreakpoint,
	CacheCauseNewSession, CacheCauseStateLost, CacheCauseTTLExpired, CacheCauseAccountSwitch,
	CacheCausePrefixDrift, CacheCauseUpstreamMiss, CacheCauseUnanalyzed,
}

// cacheReusableCauses 是“前缀之前出现过、缓存理论上应该在”的部分，用来算可复用前缀命中率。
var cacheReusableCauses = map[CacheCause]bool{
	CacheCauseHit: true, CacheCauseTTLExpired: true, CacheCauseAccountSwitch: true,
	CacheCausePrefixDrift: true, CacheCauseUpstreamMiss: true,
}

// CacheProbe 是挂在 request context 上的分析输入。IR 为 nil 表示没法分析。
type CacheProbe struct {
	IR *CacheRequestIR
	// ResponseID 由 OpenAI 的 usage 记录补上，用于登记 Responses 状态机。
	ResponseID string
	// Skipped 说明为什么没有 IR（oversize / sampled / invalid），只用于调试。
	Skipped string
	// AccountPlatform 是最终上游账号的平台；只有真发给 Anthropic 时“没打 cache_control”才算原因。
	AccountPlatform string
}

type cacheProbeContextKey struct{}

// CacheProbeFromContext 读出 handler 挂上的 probe。
func CacheProbeFromContext(ctx context.Context) *CacheProbe {
	if ctx == nil {
		return nil
	}
	probe, _ := ctx.Value(cacheProbeContextKey{}).(*CacheProbe)
	return probe
}

// WithCacheProbe 把 probe 挂进 context（usage 记录的 detached context 需要显式复制它）。
func WithCacheProbe(ctx context.Context, probe *CacheProbe) context.Context {
	if probe == nil {
		return ctx
	}
	return context.WithValue(ctx, cacheProbeContextKey{}, probe)
}

// AttachCacheProbe 在 handler 里调用：分析器开着时把请求体转成 IR 挂到 c.Request 上。
// 只做哈希不存原文；超过大小上限或被采样跳过时挂一个空 probe，记为 unanalyzed。
//
// 必须在上游响应结束后、提交 usage 任务前调用，不占首字时间；同一请求重复调用（failover 部分入账）只算一次。
func AttachCacheProbe(c *gin.Context, protocol CacheProtocol, accountPlatform string, body []byte) {
	analyzer := defaultOpsCacheAnalyzer.Load()
	if analyzer == nil || c == nil || c.Request == nil {
		return
	}
	if CacheProbeFromContext(c.Request.Context()) != nil {
		return
	}
	probe := analyzer.buildProbe(protocol, body)
	probe.AccountPlatform = accountPlatform
	c.Request = c.Request.WithContext(WithCacheProbe(c.Request.Context(), probe))
}

// SetCacheProbeResponseID 记下本轮上游返回的 response_id（OpenAI Responses 状态机用）。
func SetCacheProbeResponseID(ctx context.Context, responseID string) {
	if probe := CacheProbeFromContext(ctx); probe != nil && probe.IR != nil {
		probe.ResponseID = strings.TrimSpace(responseID)
	}
}

var defaultOpsCacheAnalyzer atomic.Pointer[OpsCacheAnalyzer]

// observeCacheUsage 由 usage 落库出口调用；分析器没开或队列满时什么都不做。
func observeCacheUsage(ctx context.Context, usageLog *UsageLog) {
	if analyzer := defaultOpsCacheAnalyzer.Load(); analyzer != nil {
		analyzer.Observe(CacheProbeFromContext(ctx), usageLog)
	}
}

// OpsCacheAnalysisStore 是分析结果的持久化接口（opsRepository 实现）。
type OpsCacheAnalysisStore interface {
	UpsertCacheAnalysisBuckets(ctx context.Context, rows []OpsCacheAnalysisBucket) error
	QueryCacheAnalysis(ctx context.Context, filter *OpsDashboardFilter) (*OpsCacheAnalysisResponse, error)
}

// OpsCacheAnalysisBucket 是一行 5 分钟聚合（同一维度多次写入时累加）。
type OpsCacheAnalysisBucket struct {
	BucketStart  time.Time
	UserID       int64
	GroupID      int64
	AccountID    int64
	Model        string
	Protocol     string
	Cause        CacheCause
	RequestCount int64
	Tokens       int64
	SavedUSD     float64
}

type cacheBucketKey struct {
	bucket    int64
	userID    int64
	groupID   int64
	accountID int64
	model     string
	protocol  string
	cause     CacheCause
}

type cacheBucketValue struct {
	requests int64
	tokens   int64
	saved    float64
}

type cacheObservation struct {
	probe *CacheProbe
	// 只拷贝需要的标量，不保活整个 UsageLog。
	at                  time.Time
	userID              int64
	groupID             int64
	accountID           int64
	model               string
	inputTokens         int64
	cacheCreationTokens int64
	cacheReadTokens     int64
	inputCost           float64
	cacheReadCost       float64
}

// OpsCacheAnalyzer 见文件头注释。
type OpsCacheAnalyzer struct {
	cfg   config.OpsCacheAnalysisConfig
	store OpsCacheAnalysisStore
	rdb   *redis.Client

	prefixes  *cacheLRU // 前缀哈希（含模型）→ 最近一次出现
	sessions  *cacheLRU // 会话键（含模型）→ 最近一次的深度/账号
	responses *cacheLRU // response_id 哈希 → 那一轮的前缀

	queues  []chan cacheObservation
	dropped atomic.Int64

	l2Writes chan cacheL2Write

	mu   sync.Mutex
	agg  map[cacheBucketKey]*cacheBucketValue
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
	now  func() time.Time
}

type cacheEntry struct {
	AtMs      int64
	AccountID int64
	Depth     int32
	Tip       uint64
}

type cacheL2Write struct {
	key   string
	entry cacheEntry
}

const (
	cacheAnalysisBucketSeconds = 300
	cacheL2KeyPrefix           = "ops:cache_analysis:v1:"
	cacheL2MaxLookups          = 32
	// 上游可缓存的最小 prompt 长度（token）。各家都是 1024 起步（部分模型更高），按最宽松的算。
	cacheMinCacheableTokens = 1024
)

// NewOpsCacheAnalyzer 构造分析器；store 为 nil 时不落库（测试用）。
func NewOpsCacheAnalyzer(cfg config.OpsCacheAnalysisConfig, store OpsCacheAnalysisStore, rdb *redis.Client) *OpsCacheAnalyzer {
	cfg = cfg.WithDefaults()
	a := &OpsCacheAnalyzer{
		cfg:       cfg,
		store:     store,
		rdb:       rdb,
		prefixes:  newCacheLRU(cfg.L1Entries),
		sessions:  newCacheLRU(cfg.L1Entries / 4),
		responses: newCacheLRU(cfg.L1Entries / 4),
		agg:       make(map[cacheBucketKey]*cacheBucketValue),
		stop:      make(chan struct{}),
		now:       time.Now,
	}
	a.queues = make([]chan cacheObservation, cfg.Workers)
	for i := range a.queues {
		a.queues[i] = make(chan cacheObservation, cfg.QueueSize/cfg.Workers+1)
	}
	if rdb != nil {
		a.l2Writes = make(chan cacheL2Write, 4096)
	}
	return a
}

// Start 启动 worker、L2 写回和定期落库，并注册为全局分析器。
func (a *OpsCacheAnalyzer) Start() {
	if a == nil {
		return
	}
	for i := range a.queues {
		a.wg.Add(1)
		go a.runWorker(a.queues[i])
	}
	if a.l2Writes != nil {
		a.wg.Add(1)
		go a.runL2Writer()
	}
	a.wg.Add(1)
	go a.runFlusher()
	defaultOpsCacheAnalyzer.Store(a)
}

// Stop 注销全局分析器、排空队列并把剩余聚合落库。
func (a *OpsCacheAnalyzer) Stop() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		defaultOpsCacheAnalyzer.CompareAndSwap(a, nil)
		close(a.stop)
		a.wg.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.flush(ctx)
	})
}

func (a *OpsCacheAnalyzer) buildProbe(protocol CacheProtocol, body []byte) *CacheProbe {
	if a.cfg.MaxBodyBytes > 0 && int64(len(body)) > a.cfg.MaxBodyBytes {
		return &CacheProbe{Skipped: "oversize"}
	}
	if a.cfg.SampleRate < 1 && rand.Float64() >= a.cfg.SampleRate {
		return &CacheProbe{Skipped: "sampled"}
	}
	ir, ok := BuildCacheRequestIR(protocol, body)
	if !ok {
		return &CacheProbe{Skipped: "invalid"}
	}
	return &CacheProbe{IR: ir}
}

// Observe 非阻塞地投递一次成功请求的 usage；队列满时丢弃并计数。
func (a *OpsCacheAnalyzer) Observe(probe *CacheProbe, usageLog *UsageLog) {
	if a == nil || usageLog == nil {
		return
	}
	prompt := int64(usageLog.InputTokens) + int64(usageLog.CacheCreationTokens) + int64(usageLog.CacheReadTokens)
	if prompt <= 0 {
		return
	}
	obs := cacheObservation{
		probe:               probe,
		at:                  usageLog.CreatedAt,
		userID:              usageLog.UserID,
		accountID:           usageLog.AccountID,
		model:               strings.TrimSpace(usageLog.RequestedModel),
		inputTokens:         int64(usageLog.InputTokens),
		cacheCreationTokens: int64(usageLog.CacheCreationTokens),
		cacheReadTokens:     int64(usageLog.CacheReadTokens),
		inputCost:           usageLog.InputCost,
		cacheReadCost:       usageLog.CacheReadCost,
	}
	if obs.model == "" {
		obs.model = strings.TrimSpace(usageLog.Model)
	}
	if usageLog.GroupID != nil {
		obs.groupID = *usageLog.GroupID
	}
	if obs.at.IsZero() {
		obs.at = a.now()
	}
	// 同一会话固定落到同一个 worker，保证轮次的先后顺序。
	shard := 0
	if probe != nil && probe.IR != nil && len(a.queues) > 1 {
		shard = int(probe.IR.SessionKey % uint64(len(a.queues)))
	}
	select {
	case a.queues[shard] <- obs:
	default:
		if a.dropped.Add(1)%1000 == 1 {
			logger.L().Warn("ops.cache_analysis.queue_full", zap.Int64("dropped_total", a.dropped.Load()))
		}
	}
}

func (a *OpsCacheAnalyzer) runWorker(queue chan cacheObservation) {
	defer a.wg.Done()
	for {
		select {
		case obs := <-queue:
			a.process(obs)
		case <-a.stop:
			for {
				select {
				case obs := <-queue:
					a.process(obs)
				default:
					return
				}
			}
		}
	}
}

// process 归因并累加；对外只暴露给测试的同步入口。
func (a *OpsCacheAnalyzer) process(obs cacheObservation) {
	causes := a.classify(obs)
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := obs.at.Unix() / cacheAnalysisBucketSeconds * cacheAnalysisBucketSeconds
	protocol := ""
	if obs.probe != nil && obs.probe.IR != nil {
		protocol = string(obs.probe.IR.Protocol)
	}
	for _, part := range causes {
		key := cacheBucketKey{bucket: bucket, userID: obs.userID, groupID: obs.groupID, accountID: obs.accountID, model: obs.model, protocol: protocol, cause: part.cause}
		v := a.agg[key]
		if v == nil {
			v = &cacheBucketValue{}
			a.agg[key] = v
		}
		v.requests += part.requests
		v.tokens += part.tokens
		v.saved += part.saved
	}
}

type cacheCausePart struct {
	cause    CacheCause
	requests int64
	tokens   int64
	saved    float64
}

func cacheTTLFor(protocol CacheProtocol) time.Duration {
	switch protocol {
	case CacheProtocolOpenAIChat, CacheProtocolOpenAIResponses:
		// OpenAI 文档：前缀缓存闲置 5–10 分钟后失效，取上限。
		return 10 * time.Minute
	default:
		// Anthropic 默认 5 分钟；Gemini 隐式缓存未公开，按同量级算。
		return 5 * time.Minute
	}
}

// classify 给一次请求的 prompt token 归因，并更新前缀/会话/状态机。
func (a *OpsCacheAnalyzer) classify(obs cacheObservation) []cacheCausePart {
	uncached := obs.inputTokens + obs.cacheCreationTokens
	prompt := uncached + obs.cacheReadTokens
	if obs.probe == nil || obs.probe.IR == nil {
		if obs.cacheReadTokens > 0 {
			return []cacheCausePart{
				{cause: CacheCauseHit, requests: 1, tokens: obs.cacheReadTokens, saved: cacheSavedUSD(obs)},
				{cause: CacheCauseUnanalyzed, tokens: uncached},
			}
		}
		return []cacheCausePart{{cause: CacheCauseUnanalyzed, requests: 1, tokens: prompt}}
	}

	ir := obs.probe.IR
	nowMs := obs.at.UnixMilli()
	modelSalt := hashString(obs.model)
	ttl := cacheTTLFor(ir.Protocol)

	// 1) 找本请求“上一轮”的前缀：有状态协议走 response 状态表，其余在前缀链上找最深的已见前缀。
	var matched *cacheEntry
	matchedDepth := -1
	stateLost := false
	if ir.PreviousResponseID != "" {
		if entry, ok := a.lookup(a.responses, "r", mixCacheHash(modelSalt, hashString(ir.PreviousResponseID))); ok {
			matched = &entry
			matchedDepth = int(entry.Depth)
		} else {
			stateLost = true
		}
	} else {
		matched, matchedDepth = a.deepestPrefix(modelSalt, ir.Chain)
	}
	sessionKey := mixCacheHash(modelSalt, ir.SessionKey)
	session, sessionSeen := a.lookup(a.sessions, "s", sessionKey)

	// 2) 归因。
	var cause CacheCause
	switch {
	case obs.cacheReadTokens > 0:
		cause = CacheCauseHit
	case prompt < cacheMinCacheableTokens:
		cause = CacheCauseBelowMin
	case ir.Protocol == CacheProtocolAnthropic && !ir.ExplicitBreakpoint && obs.probe.AccountPlatform == PlatformAnthropic:
		cause = CacheCauseNoBreakpoint
	case stateLost:
		cause = CacheCauseStateLost
	case matched == nil || matchedDepth <= 0:
		// 只命中了头部（或什么都没见过）。
		switch {
		case ir.AssistantTurns == 0 && ir.PreviousResponseID == "":
			cause = CacheCauseSingleTurn
		case sessionSeen && nowMs-session.AtMs <= time.Hour.Milliseconds() && int(session.Depth) > 0:
			cause = CacheCausePrefixDrift
		default:
			cause = CacheCauseNewSession
		}
	case nowMs-matched.AtMs > ttl.Milliseconds():
		cause = CacheCauseTTLExpired
	case matched.AccountID != obs.accountID:
		cause = CacheCauseAccountSwitch
	case sessionSeen && int(session.Depth) > matchedDepth+1 && session.AccountID == obs.accountID && ir.PreviousResponseID == "":
		// 同一会话上次已经到了更深的位置，这次只对上前面一截：中间的历史被改写了。
		cause = CacheCausePrefixDrift
	default:
		cause = CacheCauseUpstreamMiss
	}

	// 3) 登记本轮，供下一轮查找。
	depth := ir.MessageCount()
	tip := ir.Tip()
	if ir.PreviousResponseID != "" && matched != nil {
		depth += int(matched.Depth)
		tip = mixCacheHash(matched.Tip, tip)
	}
	entry := cacheEntry{AtMs: nowMs, AccountID: obs.accountID, Depth: int32(depth), Tip: tip}
	a.remember(a.prefixes, "p", mixCacheHash(modelSalt, tip), entry)
	a.remember(a.sessions, "s", sessionKey, entry)
	if obs.probe.ResponseID != "" {
		a.remember(a.responses, "r", mixCacheHash(modelSalt, hashString(obs.probe.ResponseID)), entry)
	}

	if cause == CacheCauseHit {
		parts := []cacheCausePart{{cause: CacheCauseHit, requests: 1, tokens: obs.cacheReadTokens, saved: cacheSavedUSD(obs)}}
		if uncached > 0 {
			parts = append(parts, cacheCausePart{cause: CacheCauseIncremental, tokens: uncached})
		}
		return parts
	}
	return []cacheCausePart{{cause: cause, requests: 1, tokens: prompt}}
}

// cacheSavedUSD 估算命中省下的钱：命中 token 按本次未命中输入单价计，减去实际的缓存读取费用。
func cacheSavedUSD(obs cacheObservation) float64 {
	if obs.inputTokens <= 0 || obs.inputCost <= 0 || obs.cacheReadTokens <= 0 {
		return 0
	}
	saved := float64(obs.cacheReadTokens)*(obs.inputCost/float64(obs.inputTokens)) - obs.cacheReadCost
	if saved < 0 {
		return 0
	}
	return saved
}

// deepestPrefix 从链尾往前找最深的已见前缀（只看头部之后的位置；头部单独命中不算可复用历史）。
func (a *OpsCacheAnalyzer) deepestPrefix(modelSalt uint64, chain []uint64) (*cacheEntry, int) {
	for i := len(chain) - 1; i >= 1; i-- {
		if entry, ok := a.prefixes.get(mixCacheHash(modelSalt, chain[i])); ok {
			return &entry, i
		}
	}
	if a.rdb == nil || len(chain) <= 1 {
		return nil, -1
	}
	// L1 没有：去 L2 批量查链尾最近的若干个位置（对话追加时上一轮的末端总在链尾附近）。
	lo := len(chain) - cacheL2MaxLookups
	if lo < 1 {
		lo = 1
	}
	keys := make([]string, 0, len(chain)-lo)
	for i := len(chain) - 1; i >= lo; i-- {
		keys = append(keys, cacheL2Key("p", mixCacheHash(modelSalt, chain[i])))
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.L2Timeout)
	defer cancel()
	values, err := a.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, -1
	}
	for idx, raw := range values {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		entry, ok := decodeCacheEntry(s)
		if !ok {
			continue
		}
		pos := len(chain) - 1 - idx
		a.prefixes.put(mixCacheHash(modelSalt, chain[pos]), entry)
		return &entry, pos
	}
	return nil, -1
}

func (a *OpsCacheAnalyzer) lookup(l1 *cacheLRU, kind string, key uint64) (cacheEntry, bool) {
	if entry, ok := l1.get(key); ok {
		return entry, true
	}
	if a.rdb == nil {
		return cacheEntry{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.L2Timeout)
	defer cancel()
	raw, err := a.rdb.Get(ctx, cacheL2Key(kind, key)).Result()
	if err != nil {
		return cacheEntry{}, false
	}
	entry, ok := decodeCacheEntry(raw)
	if ok {
		l1.put(key, entry)
	}
	return entry, ok
}

func (a *OpsCacheAnalyzer) remember(l1 *cacheLRU, kind string, key uint64, entry cacheEntry) {
	l1.put(key, entry)
	if a.l2Writes == nil {
		return
	}
	select {
	case a.l2Writes <- cacheL2Write{key: cacheL2Key(kind, key), entry: entry}:
	default:
		// L2 只是加速跨进程衔接，写满了就让它退化成 L1。
	}
}

func (a *OpsCacheAnalyzer) runL2Writer() {
	defer a.wg.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	pending := make(map[string]cacheEntry, 256)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pipe := a.rdb.Pipeline()
		for key, entry := range pending {
			pipe.Set(ctx, key, encodeCacheEntry(entry), a.cfg.L2TTL)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			logger.L().Debug("ops.cache_analysis.l2_write_failed", zap.Error(err))
		}
		cancel()
		clear(pending)
	}
	for {
		select {
		case w := <-a.l2Writes:
			pending[w.key] = w.entry
			if len(pending) >= 256 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-a.stop:
			flush()
			return
		}
	}
}

func (a *OpsCacheAnalyzer) runFlusher() {
	defer a.wg.Done()
	ticker := time.NewTicker(a.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			a.flush(ctx)
			cancel()
		case <-a.stop:
			return
		}
	}
}

// flush 把内存聚合写进 ops_cache_analysis_5m（累加写入）；失败时并回内存等下次。
func (a *OpsCacheAnalyzer) flush(ctx context.Context) {
	a.mu.Lock()
	if len(a.agg) == 0 {
		a.mu.Unlock()
		return
	}
	snapshot := a.agg
	a.agg = make(map[cacheBucketKey]*cacheBucketValue, len(snapshot))
	a.mu.Unlock()
	if a.store == nil {
		return
	}
	rows := make([]OpsCacheAnalysisBucket, 0, len(snapshot))
	for key, v := range snapshot {
		rows = append(rows, OpsCacheAnalysisBucket{
			BucketStart: time.Unix(key.bucket, 0).UTC(), UserID: key.userID, GroupID: key.groupID, AccountID: key.accountID,
			Model: key.model, Protocol: key.protocol, Cause: key.cause,
			RequestCount: v.requests, Tokens: v.tokens, SavedUSD: v.saved,
		})
	}
	if err := a.store.UpsertCacheAnalysisBuckets(ctx, rows); err != nil {
		logger.L().Warn("ops.cache_analysis.flush_failed", zap.Error(err), zap.Int("rows", len(rows)))
		a.mu.Lock()
		for key, v := range snapshot {
			cur := a.agg[key]
			if cur == nil {
				a.agg[key] = v
				continue
			}
			cur.requests += v.requests
			cur.tokens += v.tokens
			cur.saved += v.saved
		}
		a.mu.Unlock()
	}
}

func hashString(s string) uint64 { return xxhash.Sum64String(s) }

func cacheL2Key(kind string, key uint64) string {
	return cacheL2KeyPrefix + kind + ":" + strconv.FormatUint(key, 16)
}

func encodeCacheEntry(e cacheEntry) string {
	return fmt.Sprintf("%d:%d:%d:%x", e.AtMs, e.AccountID, e.Depth, e.Tip)
}

func decodeCacheEntry(raw string) (cacheEntry, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 4 {
		return cacheEntry{}, false
	}
	at, err1 := strconv.ParseInt(parts[0], 10, 64)
	account, err2 := strconv.ParseInt(parts[1], 10, 64)
	depth, err3 := strconv.ParseInt(parts[2], 10, 32)
	tip, err4 := strconv.ParseUint(parts[3], 16, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return cacheEntry{}, false
	}
	return cacheEntry{AtMs: at, AccountID: account, Depth: int32(depth), Tip: tip}, true
}

// cacheLRU 是按 key 分片的定长 LRU，只存小结构体，不存原文。
type cacheLRU struct {
	shards [16]cacheLRUShard
}

type cacheLRUShard struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	items map[uint64]*list.Element
}

type cacheLRUItem struct {
	key   uint64
	entry cacheEntry
}

func newCacheLRU(capacity int) *cacheLRU {
	if capacity < 16 {
		capacity = 16
	}
	l := &cacheLRU{}
	for i := range l.shards {
		l.shards[i] = cacheLRUShard{cap: capacity / len(l.shards), ll: list.New(), items: make(map[uint64]*list.Element)}
	}
	return l
}

func (l *cacheLRU) shard(key uint64) *cacheLRUShard { return &l.shards[key%uint64(len(l.shards))] }

func (l *cacheLRU) get(key uint64) (cacheEntry, bool) {
	s := l.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.ll.MoveToFront(el)
		return el.Value.(*cacheLRUItem).entry, true
	}
	return cacheEntry{}, false
}

func (l *cacheLRU) put(key uint64, entry cacheEntry) {
	s := l.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		el.Value.(*cacheLRUItem).entry = entry
		s.ll.MoveToFront(el)
		return
	}
	s.items[key] = s.ll.PushFront(&cacheLRUItem{key: key, entry: entry})
	for s.ll.Len() > s.cap {
		oldest := s.ll.Back()
		s.ll.Remove(oldest)
		delete(s.items, oldest.Value.(*cacheLRUItem).key)
	}
}
