package service

// OpenAI 选路实验（影子模式 + 按分组 A/B）。
//
// 目标：对「新请求」在同一分钟里挑预计最快、又不贵的账号，而不是只按
// priority → 负载率 → LRU 的固定顺序。实现分三块：
//
//  1. openAIRouteStats：进程内的实时观测。首字按「账号 × 档位（模型 × effort ×
//     请求体档）」记对数均值（按时间衰减），预测时拟合「档位基线 + 账号偏移」：
//     账号只和同档位的其他号比、按账号等权，请求构成不同不会被当成快慢，占大头
//     流量的号变慢也不会把基线一起拖慢；在途请求里等首字已明显超时的条数立刻
//     计入，不用等它完成。
//  2. 评分：预计首字 + 挂住罚分 + 报错罚分 + 成本（倍率折算成毫秒）。
//  3. 分组开关（settings 表 openai_route_ab，JSON）：
//     - mode=shadow：只算不选，所有请求照旧走老逻辑，usage_logs.route_decision
//       记下「新逻辑会选谁、各号预计多少」；
//     - mode=ab：按会话哈希把 percent% 的会话分进 B 组，B 组新会话按评分做
//       软分配（含少量探索流量），A 组照旧；两组都记录影子决策；
//     - 其他值或分组不在配置里：完全不参与（默认）。
//
// 老会话（粘性命中）在本阶段两组都不迁移，只记录 layer=sticky。

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	openAIRouteABSettingKey       = "openai_route_ab"
	openAIRouteABSettingCacheTTL  = 5 * time.Second
	openAIRouteABSettingDBTimeout = 2 * time.Second

	openAIRouteModeShadow = "shadow"
	openAIRouteModeAB     = "ab"

	OpenAIRouteArmShadow = "S"
	OpenAIRouteArmA      = "A"
	OpenAIRouteArmB      = "B"

	openAIRouteLayerSticky   = "sticky"
	openAIRouteLayerLoad     = "load"
	openAIRouteLayerFallback = "fallback"

	openAIRouteTicketGinKey = "openai_route_ticket"

	// 在途票据兜底过期：handler 漏调 Finish（panic 等）时不让它永远算作挂住。
	openAIRouteTicketMaxAge = 30 * time.Minute
	// 无任何样本时的首字基线。
	openAIRouteDefaultBaselineMs = 10000.0
)

// openAIRouteABGroupConfig 是单个分组的实验开关。
type openAIRouteABGroupConfig struct {
	Mode    string `json:"mode"`
	Percent int    `json:"percent"`
}

// openAIRouteABConfig 是 settings.openai_route_ab 的 JSON 结构；缺省字段取默认值。
type openAIRouteABConfig struct {
	Groups map[string]openAIRouteABGroupConfig `json:"groups"`
	Salt   string                              `json:"salt"`
	// HalfLifeSec 观测的半衰期，默认 180 秒（「这一分钟」量级）。
	HalfLifeSec float64 `json:"half_life_sec"`
	// CostMsPerRate 把账号成本倍率折成毫秒：默认 60000，即 0.05x ≈ 3s。
	CostMsPerRate float64 `json:"cost_ms_per_rate"`
	// TemperatureMs 软分配温度：分数差这么多毫秒，权重差 e 倍。
	TemperatureMs float64 `json:"temperature_ms"`
	// Explore 探索流量比例：均匀分给所有候选，让慢号持续有新样本。
	Explore float64 `json:"explore"`
	// MaxPriority 参与评分的最大 priority；更低档（数字更大）的号只做兜底，按老顺序排在后面。
	MaxPriority int `json:"max_priority"`
	// StallFactor/StallMinMs：在途请求等首字超过 max(预计×StallFactor, StallMinMs) 算挂住。
	StallFactor float64 `json:"stall_factor"`
	StallMinMs  float64 `json:"stall_min_ms"`
	// ErrorPenaltyMs 报错率每 100% 加的毫秒数（一次失败约等于一次重试的代价）。
	ErrorPenaltyMs float64 `json:"error_penalty_ms"`
	// PriorSamples 慢系数的收缩强度：样本权重低于它时向「和大家一样」收缩。
	PriorSamples float64 `json:"prior_samples"`
}

func defaultOpenAIRouteABConfig() openAIRouteABConfig {
	return openAIRouteABConfig{
		Groups:         map[string]openAIRouteABGroupConfig{},
		Salt:           "route-ab",
		HalfLifeSec:    180,
		CostMsPerRate:  60000,
		TemperatureMs:  5000,
		Explore:        0.03,
		MaxPriority:    2,
		StallFactor:    2,
		StallMinMs:     15000,
		ErrorPenaltyMs: 30000,
		PriorSamples:   5,
	}
}

// parseOpenAIRouteABConfig 解析配置；非法 JSON 返回 ok=false（调用方按关闭处理）。
func parseOpenAIRouteABConfig(raw string) (openAIRouteABConfig, bool) {
	cfg := defaultOpenAIRouteABConfig()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cfg, false
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return defaultOpenAIRouteABConfig(), false
	}
	def := defaultOpenAIRouteABConfig()
	positive := func(v *float64, fallback float64) {
		if *v <= 0 || math.IsNaN(*v) || math.IsInf(*v, 0) {
			*v = fallback
		}
	}
	positive(&cfg.HalfLifeSec, def.HalfLifeSec)
	positive(&cfg.TemperatureMs, def.TemperatureMs)
	positive(&cfg.StallFactor, def.StallFactor)
	positive(&cfg.StallMinMs, def.StallMinMs)
	positive(&cfg.PriorSamples, def.PriorSamples)
	if cfg.CostMsPerRate < 0 || math.IsNaN(cfg.CostMsPerRate) || math.IsInf(cfg.CostMsPerRate, 0) {
		cfg.CostMsPerRate = def.CostMsPerRate
	}
	if cfg.ErrorPenaltyMs < 0 || math.IsNaN(cfg.ErrorPenaltyMs) || math.IsInf(cfg.ErrorPenaltyMs, 0) {
		cfg.ErrorPenaltyMs = def.ErrorPenaltyMs
	}
	if cfg.Explore < 0 || cfg.Explore > 0.5 || math.IsNaN(cfg.Explore) {
		cfg.Explore = def.Explore
	}
	if cfg.MaxPriority <= 0 {
		cfg.MaxPriority = def.MaxPriority
	}
	if strings.TrimSpace(cfg.Salt) == "" {
		cfg.Salt = def.Salt
	}
	if cfg.Groups == nil {
		cfg.Groups = map[string]openAIRouteABGroupConfig{}
	}
	return cfg, true
}

func (c openAIRouteABConfig) groupConfig(groupID *int64) (openAIRouteABGroupConfig, bool) {
	if groupID == nil || len(c.Groups) == 0 {
		return openAIRouteABGroupConfig{}, false
	}
	g, ok := c.Groups[strconv.FormatInt(*groupID, 10)]
	if !ok {
		return openAIRouteABGroupConfig{}, false
	}
	g.Mode = strings.ToLower(strings.TrimSpace(g.Mode))
	if g.Mode != openAIRouteModeShadow && g.Mode != openAIRouteModeAB {
		return openAIRouteABGroupConfig{}, false
	}
	if g.Percent < 0 {
		g.Percent = 0
	}
	if g.Percent > 100 {
		g.Percent = 100
	}
	return g, true
}

type cachedOpenAIRouteABConfig struct {
	cfg       openAIRouteABConfig
	enabled   bool
	expiresAt int64
}

var openAIRouteABConfigCache atomic.Value // *cachedOpenAIRouteABConfig
var openAIRouteABConfigMu sync.Mutex

// openAIRouteRandFloat 是软分配用的随机源，测试里可替换成确定值。
var openAIRouteRandFloat = rand.Float64

// openAIRouteABRuntimeConfig 读取（带 5 秒缓存）实验配置。读失败按关闭处理。
func (s *OpenAIGatewayService) openAIRouteABRuntimeConfig(ctx context.Context) (openAIRouteABConfig, bool) {
	if cached, ok := openAIRouteABConfigCache.Load().(*cachedOpenAIRouteABConfig); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.cfg, cached.enabled
		}
	}
	openAIRouteABConfigMu.Lock()
	defer openAIRouteABConfigMu.Unlock()
	if cached, ok := openAIRouteABConfigCache.Load().(*cachedOpenAIRouteABConfig); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return cached.cfg, cached.enabled
		}
	}
	cfg, enabled := defaultOpenAIRouteABConfig(), false
	if repo := s.openAIAdvancedSchedulerSettingRepo(); repo != nil {
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIRouteABSettingDBTimeout)
		raw, err := repo.GetValue(dbCtx, openAIRouteABSettingKey)
		cancel()
		if err == nil {
			cfg, enabled = parseOpenAIRouteABConfig(raw)
		}
	}
	openAIRouteABConfigCache.Store(&cachedOpenAIRouteABConfig{
		cfg:       cfg,
		enabled:   enabled,
		expiresAt: time.Now().Add(openAIRouteABSettingCacheTTL).UnixNano(),
	})
	return cfg, enabled
}

// ---------------------------------------------------------------------------
// 观测
// ---------------------------------------------------------------------------

// openAIRouteDecayed 是按时间指数衰减的均值（半衰期由调用方给）。
type openAIRouteDecayed struct {
	sum    float64
	weight float64
	at     time.Time
}

func openAIRouteDecayFactor(from, now time.Time, halfLifeSec float64) float64 {
	if from.IsZero() || !now.After(from) {
		return 1
	}
	return math.Exp2(-now.Sub(from).Seconds() / halfLifeSec)
}

func (d *openAIRouteDecayed) add(x float64, now time.Time, halfLifeSec float64) {
	f := openAIRouteDecayFactor(d.at, now, halfLifeSec)
	d.sum = d.sum*f + x
	d.weight = d.weight*f + 1
	d.at = now
}

// value 返回衰减到 now 的均值与有效样本数；没有样本时 weight=0。
func (d *openAIRouteDecayed) value(now time.Time, halfLifeSec float64) (mean float64, weight float64) {
	if d == nil || d.weight <= 0 {
		return 0, 0
	}
	f := openAIRouteDecayFactor(d.at, now, halfLifeSec)
	w := d.weight * f
	if w <= 0 {
		return 0, 0
	}
	return d.sum / d.weight, w
}

type openAIRouteTicket struct {
	id        uint64
	accountID int64
	model     string
	shape     string
	start     time.Time
	firstAt   time.Time
	done      bool
	stats     *openAIRouteStats
	decision  *OpenAIRouteDecision
	halfLife  float64
}

// openAIRouteCellKey 是「账号 × 请求档位」格子。
type openAIRouteCellKey struct {
	accountID int64
	shape     string
}

type openAIRouteStats struct {
	mu sync.Mutex
	// cells: model → (账号, 档位) → log(首字ms) 的衰减均值。
	// 预测时用双向固定效应拟合：log 首字 = 档位基线 + 账号偏移。账号偏移只在
	// 同档位的格子之间比较、按账号等权，所以「哪个号接了更重的请求」不会被当成
	// 快慢，某个号占大头流量时也不会把基线拖着一起变慢。
	cells map[string]map[openAIRouteCellKey]*openAIRouteDecayed
	// errs: accountID → 失败(1)/成功(0)
	errs     map[int64]*openAIRouteDecayed
	inflight map[int64]map[uint64]*openAIRouteTicket
	nextID   uint64
}

func newOpenAIRouteStats() *openAIRouteStats {
	return &openAIRouteStats{
		cells:    map[string]map[openAIRouteCellKey]*openAIRouteDecayed{},
		errs:     map[int64]*openAIRouteDecayed{},
		inflight: map[int64]map[uint64]*openAIRouteTicket{},
	}
}

func (s *OpenAIGatewayService) openAIRouteStats() *openAIRouteStats {
	if s == nil {
		return nil
	}
	s.openaiRouteStatsOnce.Do(func() {
		s.openaiRouteStatsState = newOpenAIRouteStats()
	})
	return s.openaiRouteStatsState
}

// openAIRouteBodyBand 把请求体大小分档（约 4 字节/token）：s<32k、m<128k、l<512k token，x 更大。
func openAIRouteBodyBand(bodyBytes int64) string {
	switch {
	case bodyBytes <= 0:
		return "?"
	case bodyBytes < 128<<10:
		return "s"
	case bodyBytes < 512<<10:
		return "m"
	case bodyBytes < 2<<20:
		return "l"
	default:
		return "x"
	}
}

func openAIRouteShape(model, effort, band string) string {
	if effort == "" {
		effort = "-"
	}
	return model + "|" + effort + "|" + band
}

func (st *openAIRouteStats) startTicket(accountID int64, model, shape string, decision *OpenAIRouteDecision, halfLife float64, now time.Time) *openAIRouteTicket {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.nextID++
	t := &openAIRouteTicket{
		id:        st.nextID,
		accountID: accountID,
		model:     model,
		shape:     shape,
		start:     now,
		stats:     st,
		decision:  decision,
		halfLife:  halfLife,
	}
	m := st.inflight[accountID]
	if m == nil {
		m = map[uint64]*openAIRouteTicket{}
		st.inflight[accountID] = m
	}
	m[t.id] = t
	return t
}

// markFirstToken 记一个首字样本（从本次尝试开始转发算起）。
func (t *openAIRouteTicket) markFirstToken(now time.Time) {
	if t == nil || t.stats == nil {
		return
	}
	st := t.stats
	st.mu.Lock()
	defer st.mu.Unlock()
	if t.done || !t.firstAt.IsZero() {
		return
	}
	t.firstAt = now
	ms := float64(now.Sub(t.start).Milliseconds())
	if ms < 1 {
		ms = 1
	}
	byCell := st.cells[t.model]
	if byCell == nil {
		byCell = map[openAIRouteCellKey]*openAIRouteDecayed{}
		st.cells[t.model] = byCell
	}
	key := openAIRouteCellKey{accountID: t.accountID, shape: t.shape}
	d := byCell[key]
	if d == nil {
		d = &openAIRouteDecayed{}
		byCell[key] = d
	}
	d.add(math.Log(ms), now, t.halfLife)
}

// finish 结束一次尝试：移出在途，记成功/失败。
func (t *openAIRouteTicket) finish(success bool, now time.Time) {
	if t == nil || t.stats == nil {
		return
	}
	st := t.stats
	st.mu.Lock()
	defer st.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	if m := st.inflight[t.accountID]; m != nil {
		delete(m, t.id)
		if len(m) == 0 {
			delete(st.inflight, t.accountID)
		}
	}
	d := st.errs[t.accountID]
	if d == nil {
		d = &openAIRouteDecayed{}
		st.errs[t.accountID] = d
	}
	x := 0.0
	if !success {
		x = 1
	}
	d.add(x, now, t.halfLife)
}

// openAIRouteFit 是某个模型的拟合结果（对数毫秒）。
type openAIRouteFit struct {
	shapeBase   map[string]float64
	accountOff  map[int64]float64
	accountW    map[int64]float64
	overallBase float64
	hasData     bool
}

// fitLocked 做几轮交替最小二乘：base[s] = 加权均值(m − off[a])，
// off[a] = 加权和(m − base[s]) / (权重 + 先验)，再把账号偏移按账号等权居中。
// 先验让样本少的账号向「和大家一样」收缩；格子和账号都很少（个位数号 × 几十档），
// 每次选号算一遍的开销可以忽略。
func (st *openAIRouteStats) fitLocked(model string, now time.Time, cfg openAIRouteABConfig) openAIRouteFit {
	fit := openAIRouteFit{
		shapeBase:  map[string]float64{},
		accountOff: map[int64]float64{},
		accountW:   map[int64]float64{},
	}
	type cell struct {
		key  openAIRouteCellKey
		mean float64
		w    float64
	}
	cells := make([]cell, 0, len(st.cells[model]))
	for key, d := range st.cells[model] {
		mean, w := d.value(now, cfg.HalfLifeSec)
		if w < 0.01 {
			if w <= 0 || now.Sub(d.at) > 24*time.Hour {
				delete(st.cells[model], key)
			}
			continue
		}
		cells = append(cells, cell{key: key, mean: mean, w: w})
		fit.accountW[key.accountID] += w
	}
	if len(cells) == 0 {
		return fit
	}
	fit.hasData = true
	for iter := 0; iter < 8; iter++ {
		num, den := map[string]float64{}, map[string]float64{}
		for _, c := range cells {
			num[c.key.shape] += c.w * (c.mean - fit.accountOff[c.key.accountID])
			den[c.key.shape] += c.w
		}
		for shape := range den {
			fit.shapeBase[shape] = num[shape] / den[shape]
		}
		anum := map[int64]float64{}
		for _, c := range cells {
			anum[c.key.accountID] += c.w * (c.mean - fit.shapeBase[c.key.shape])
		}
		center := 0.0
		for id, w := range fit.accountW {
			fit.accountOff[id] = anum[id] / (w + cfg.PriorSamples)
			center += fit.accountOff[id]
		}
		center /= float64(len(fit.accountW))
		for id := range fit.accountOff {
			fit.accountOff[id] -= center
		}
	}
	totalW, totalNum := 0.0, 0.0
	for _, c := range cells {
		totalNum += c.w * (c.mean - fit.accountOff[c.key.accountID])
		totalW += c.w
	}
	fit.overallBase = totalNum / totalW
	return fit
}

// openAIRouteEstimate 是单个候选号的评分明细（毫秒）。
type openAIRouteEstimate struct {
	AccountID  int64
	PredMs     float64 // 预计首字（档位基线 × 账号慢系数）
	StallMs    float64 // 挂住罚分
	ErrorMs    float64 // 报错罚分
	CostMs     float64 // 成本折算
	Score      float64
	Stalled    int
	Inflight   int
	SlowFactor float64
	Samples    float64
}

// estimateAll 给一组候选号打分（同一次拟合）。
func (st *openAIRouteStats) estimateAll(accounts []*Account, model, shape string, cfg openAIRouteABConfig, now time.Time) []openAIRouteEstimate {
	st.mu.Lock()
	defer st.mu.Unlock()
	fit := st.fitLocked(model, now, cfg)
	out := make([]openAIRouteEstimate, len(accounts))
	for i, account := range accounts {
		out[i] = st.estimateLocked(account, shape, fit, cfg, now)
	}
	return out
}

func (st *openAIRouteStats) estimate(account *Account, model, shape string, cfg openAIRouteABConfig, now time.Time) openAIRouteEstimate {
	return st.estimateAll([]*Account{account}, model, shape, cfg, now)[0]
}

func (st *openAIRouteStats) estimateLocked(account *Account, shape string, fit openAIRouteFit, cfg openAIRouteABConfig, now time.Time) openAIRouteEstimate {
	est := openAIRouteEstimate{AccountID: account.ID, SlowFactor: 1}
	baseLog := math.Log(openAIRouteDefaultBaselineMs)
	if fit.hasData {
		baseLog = fit.overallBase
		if b, ok := fit.shapeBase[shape]; ok {
			baseLog = b
		}
	}
	off := fit.accountOff[account.ID]
	est.Samples = fit.accountW[account.ID]
	est.SlowFactor = math.Exp(off)
	est.PredMs = math.Exp(baseLog + off)

	stallThreshold := math.Max(est.PredMs*cfg.StallFactor, cfg.StallMinMs)
	maxStallAge := 0.0
	for id, t := range st.inflight[account.ID] {
		age := float64(now.Sub(t.start).Milliseconds())
		if now.Sub(t.start) > openAIRouteTicketMaxAge {
			delete(st.inflight[account.ID], id)
			continue
		}
		est.Inflight++
		if t.firstAt.IsZero() && age > stallThreshold {
			est.Stalled++
			if age > maxStallAge {
				maxStallAge = age
			}
		}
	}
	if est.Inflight > 0 && est.Stalled > 0 {
		est.StallMs = float64(est.Stalled) / float64(est.Inflight) * maxStallAge
	}
	if d := st.errs[account.ID]; d != nil {
		mean, w := d.value(now, cfg.HalfLifeSec)
		est.ErrorMs = mean * w / (w + cfg.PriorSamples) * cfg.ErrorPenaltyMs
	}
	est.CostMs = account.BillingRateMultiplier() * cfg.CostMsPerRate
	est.Score = est.PredMs + est.StallMs + est.ErrorMs + est.CostMs
	return est
}

// ---------------------------------------------------------------------------
// 决策
// ---------------------------------------------------------------------------

// OpenAIRouteDecision 写进 usage_logs.route_decision（JSON），供 A/B 与影子分析。
type OpenAIRouteDecision struct {
	Arm     string `json:"arm"`
	Layer   string `json:"layer"`
	Session string `json:"sh,omitempty"`
	Shape   string `json:"shape"`
	// Picked 实际选中的号；Best 新逻辑的首选（分数最低）；Applied=true 表示本次真的按新逻辑排序。
	Picked  int64 `json:"picked"`
	Best    int64 `json:"best,omitempty"`
	Applied bool  `json:"applied,omitempty"`
	// Est 各候选号的评分明细（毫秒，取整）：p 预计首字、s 挂住、e 报错、c 成本、t 总分、n 在途、k 挂住条数。
	Est map[string]openAIRouteEstimateJSON `json:"est,omitempty"`

	model string
	shape string
}

type openAIRouteEstimateJSON struct {
	P int64 `json:"p"`
	S int64 `json:"s,omitempty"`
	E int64 `json:"e,omitempty"`
	C int64 `json:"c,omitempty"`
	T int64 `json:"t"`
	N int   `json:"n,omitempty"`
	K int   `json:"k,omitempty"`
}

// MarshalRouteDecision 序列化为 usage_logs.route_decision；nil 返回 nil。
func (d *OpenAIRouteDecision) MarshalRouteDecision() []byte {
	if d == nil {
		return nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	return b
}

// openAIRouteSelection 是单次选号的实验上下文（不在实验中则为 nil）。
type openAIRouteSelection struct {
	stats   *openAIRouteStats
	cfg     openAIRouteABConfig
	arm     string
	session string
	model   string
	shape   string
	now     time.Time
	rng     func() float64
}

// newOpenAIRouteSelection 判断本次请求是否在实验里、在哪一组。
func (s *OpenAIGatewayService) newOpenAIRouteSelection(ctx context.Context, groupID *int64, sessionHash, requestedModel string) *openAIRouteSelection {
	if s == nil || groupID == nil {
		return nil
	}
	cfg, enabled := s.openAIRouteABRuntimeConfig(ctx)
	if !enabled {
		return nil
	}
	group, ok := cfg.groupConfig(groupID)
	if !ok {
		return nil
	}
	effort := ""
	if e := RequestedReasoningEffortFromContext(ctx); e != nil {
		effort = strings.ToLower(strings.TrimSpace(*e))
	}
	model := strings.TrimSpace(requestedModel)
	sel := &openAIRouteSelection{
		stats: s.openAIRouteStats(),
		cfg:   cfg,
		model: model,
		shape: openAIRouteShape(model, effort, openAIRouteBodyBand(openAIInboundRequestBodyBytesFromContext(ctx))),
		now:   time.Now(),
		rng:   openAIRouteRandFloat,
	}
	bucket, session := openAIRouteSessionBucket(cfg.Salt, sessionHash)
	sel.session = session
	switch group.Mode {
	case openAIRouteModeShadow:
		sel.arm = OpenAIRouteArmShadow
	case openAIRouteModeAB:
		if bucket < group.Percent {
			sel.arm = OpenAIRouteArmB
		} else {
			sel.arm = OpenAIRouteArmA
		}
	default:
		return nil
	}
	return sel
}

// openAIRouteSessionBucket 按会话哈希分桶（0–99）；没有会话键时按请求随机。
// 返回的 session 是加盐后的短哈希，只用于分析时按会话聚合，不能还原会话键。
func openAIRouteSessionBucket(salt, sessionHash string) (int, string) {
	sessionHash = strings.TrimSpace(sessionHash)
	if sessionHash == "" {
		return rand.IntN(100), ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(sessionHash))
	sum := h.Sum64()
	return int(sum % 100), strconv.FormatUint(sum>>16, 36)
}

func (r *openAIRouteSelection) newDecision(layer string, picked int64) *OpenAIRouteDecision {
	if r == nil {
		return nil
	}
	return &OpenAIRouteDecision{
		Arm:     r.arm,
		Layer:   layer,
		Session: r.session,
		Shape:   r.shape,
		Picked:  picked,
		model:   r.model,
		shape:   r.shape,
	}
}

// order 对 Layer 2 的候选排序：评分范围内的号按新逻辑打分；B 组按软分配重排，
// 其他组保持老顺序。返回（可能重排后的）顺序和决策模板（Picked 由调用方填）。
func (r *openAIRouteSelection) order(available []accountWithLoad) ([]accountWithLoad, *OpenAIRouteDecision) {
	decision := r.newDecision(openAIRouteLayerLoad, 0)
	if r == nil || len(available) == 0 {
		return available, decision
	}
	scored := make([]accountWithLoad, 0, len(available))
	rest := make([]accountWithLoad, 0)
	for _, item := range available {
		if item.account != nil && item.account.Priority <= r.cfg.MaxPriority {
			scored = append(scored, item)
		} else {
			rest = append(rest, item)
		}
	}
	if len(scored) == 0 {
		return available, decision
	}
	scoredAccounts := make([]*Account, len(scored))
	for i, item := range scored {
		scoredAccounts[i] = item.account
	}
	estimates := r.stats.estimateAll(scoredAccounts, r.model, r.shape, r.cfg, r.now)
	decision.Est = make(map[string]openAIRouteEstimateJSON, len(scored))
	best := 0
	for i, item := range scored {
		est := estimates[i]
		decision.Est[strconv.FormatInt(item.account.ID, 10)] = openAIRouteEstimateJSON{
			P: int64(math.Round(est.PredMs)),
			S: int64(math.Round(est.StallMs)),
			E: int64(math.Round(est.ErrorMs)),
			C: int64(math.Round(est.CostMs)),
			T: int64(math.Round(est.Score)),
			N: est.Inflight,
			K: est.Stalled,
		}
		if est.Score < estimates[best].Score {
			best = i
		}
	}
	decision.Best = scored[best].account.ID
	if r.arm != OpenAIRouteArmB {
		return available, decision
	}
	decision.Applied = true
	ordered := openAIRouteSoftOrder(scored, estimates, r.cfg, r.rng)
	return append(ordered, rest...), decision
}

// openAIRouteSoftOrder 按 softmax(−分数/温度) 加探索做无放回抽样，得到尝试顺序。
// 抢槽失败时会顺着往下试，所以排在后面的号仍然是兜底。
func openAIRouteSoftOrder(items []accountWithLoad, estimates []openAIRouteEstimate, cfg openAIRouteABConfig, rng func() float64) []accountWithLoad {
	n := len(items)
	if n <= 1 {
		return append([]accountWithLoad(nil), items...)
	}
	minScore := estimates[0].Score
	for _, e := range estimates[1:] {
		if e.Score < minScore {
			minScore = e.Score
		}
	}
	weights := make([]float64, n)
	total := 0.0
	for i, e := range estimates {
		weights[i] = math.Exp(-(e.Score - minScore) / cfg.TemperatureMs)
		total += weights[i]
	}
	for i := range weights {
		weights[i] = (1-cfg.Explore)*weights[i]/total + cfg.Explore/float64(n)
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	out := make([]accountWithLoad, 0, n)
	for len(idx) > 0 {
		sum := 0.0
		for _, i := range idx {
			sum += weights[i]
		}
		x := rng() * sum
		pick := len(idx) - 1
		for k, i := range idx {
			x -= weights[i]
			if x < 0 {
				pick = k
				break
			}
		}
		out = append(out, items[idx[pick]])
		idx = append(idx[:pick], idx[pick+1:]...)
	}
	return out
}

// ---------------------------------------------------------------------------
// handler 侧接口
// ---------------------------------------------------------------------------

// StartOpenAIRouteAttempt 在转发前调用：把本次选号的实验决策挂到 gin 上下文并登记在途。
// 不在实验里时什么都不做。重复调用会先结束上一张未结束的票据（按失败计）。
func (s *OpenAIGatewayService) StartOpenAIRouteAttempt(c *gin.Context, selection *AccountSelectionResult) {
	if s == nil || c == nil {
		return
	}
	if prev := openAIRouteTicketFromGin(c); prev != nil {
		prev.finish(false, time.Now())
		c.Set(openAIRouteTicketGinKey, (*openAIRouteTicket)(nil))
	}
	if selection == nil || selection.Account == nil || selection.RouteDecision == nil {
		return
	}
	d := selection.RouteDecision
	d.Picked = selection.Account.ID
	st := s.openAIRouteStats()
	cfg, _ := s.openAIRouteABRuntimeConfig(c.Request.Context())
	t := st.startTicket(selection.Account.ID, d.model, d.shape, d, cfg.HalfLifeSec, time.Now())
	c.Set(openAIRouteTicketGinKey, t)
}

// FinishOpenAIRouteAttempt 在转发返回后调用，success 与调度结果口径一致。
func (s *OpenAIGatewayService) FinishOpenAIRouteAttempt(c *gin.Context, success bool) {
	if t := openAIRouteTicketFromGin(c); t != nil {
		t.finish(success, time.Now())
	}
}

// OpenAIRouteDecisionJSON 返回最近一次尝试的决策 JSON（写 usage_logs 用）；没有时为 nil。
func OpenAIRouteDecisionJSON(c *gin.Context) []byte {
	t := openAIRouteTicketFromGin(c)
	if t == nil {
		return nil
	}
	return t.decision.MarshalRouteDecision()
}

// markOpenAIRouteFirstToken 在流式首个语义输出处调用。
func markOpenAIRouteFirstToken(c *gin.Context) {
	if t := openAIRouteTicketFromGin(c); t != nil {
		t.markFirstToken(time.Now())
	}
}

func openAIRouteTicketFromGin(c *gin.Context) *openAIRouteTicket {
	if c == nil {
		return nil
	}
	v, ok := c.Get(openAIRouteTicketGinKey)
	if !ok {
		return nil
	}
	t, _ := v.(*openAIRouteTicket)
	return t
}
