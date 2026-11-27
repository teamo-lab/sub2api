package service

import "time"

// OpsCacheAnalysisCauseStat 是某个归因在窗口内的请求数与 prompt token 数。
type OpsCacheAnalysisCauseStat struct {
	Cause    CacheCause `json:"cause"`
	Requests int64      `json:"requests"`
	Tokens   int64      `json:"tokens"`
}

// OpsCacheAnalysisAccountStat 是单个上游账号的归因拆分（按可处理的未命中 token 排序）。
type OpsCacheAnalysisAccountStat struct {
	AccountID    int64                       `json:"account_id"`
	AccountName  string                      `json:"account_name"`
	PromptTokens int64                       `json:"prompt_tokens"`
	HitTokens    int64                       `json:"hit_tokens"`
	Causes       []OpsCacheAnalysisCauseStat `json:"causes"`
}

// OpsCacheAnalysisResponse 是 /admin/ops/cache-analysis 的返回。
type OpsCacheAnalysisResponse struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`

	Requests       int64   `json:"requests"`
	PromptTokens   int64   `json:"prompt_tokens"`
	HitTokens      int64   `json:"hit_tokens"`
	UncachedTokens int64   `json:"uncached_tokens"`
	SavedUSD       float64 `json:"saved_usd"`
	// HitRate = 命中 token / 全部 prompt token，与分账号 SLA 里的缓存率同口径。
	HitRate *float64 `json:"hit_rate"`
	// ReusableHitRate = 命中 / (命中 + TTL 过期 + 换号 + 前缀漂移 + 上游未命中)，
	// 即“前缀之前出现过、缓存理论上应该在”的那部分里实际命中的比例。
	ReusableHitRate *float64 `json:"reusable_hit_rate"`

	Causes   []OpsCacheAnalysisCauseStat   `json:"causes"`
	Accounts []OpsCacheAnalysisAccountStat `json:"accounts"`
}

// FinalizeCacheAnalysis 按展示顺序补齐所有归因并计算比率。
func FinalizeCacheAnalysis(resp *OpsCacheAnalysisResponse) {
	if resp == nil {
		return
	}
	byCause := make(map[CacheCause]OpsCacheAnalysisCauseStat, len(resp.Causes))
	for _, stat := range resp.Causes {
		cur := byCause[stat.Cause]
		cur.Cause = stat.Cause
		cur.Requests += stat.Requests
		cur.Tokens += stat.Tokens
		byCause[stat.Cause] = cur
	}
	ordered := make([]OpsCacheAnalysisCauseStat, 0, len(AllCacheCauses))
	var prompt, hit, reusable, requests int64
	for _, cause := range AllCacheCauses {
		stat := byCause[cause]
		stat.Cause = cause
		ordered = append(ordered, stat)
		prompt += stat.Tokens
		requests += stat.Requests
		if cause == CacheCauseHit {
			hit = stat.Tokens
		}
		if cacheReusableCauses[cause] {
			reusable += stat.Tokens
		}
	}
	resp.Causes = ordered
	resp.Requests = requests
	resp.PromptTokens = prompt
	resp.HitTokens = hit
	resp.UncachedTokens = prompt - hit
	if prompt > 0 {
		v := float64(hit) / float64(prompt)
		resp.HitRate = &v
	}
	if reusable > 0 {
		v := float64(hit) / float64(reusable)
		resp.ReusableHitRate = &v
	}
}

// IsActionableCacheMiss 报告这个归因是否是“该命中却没命中”、运维可以处理的未命中。
func IsActionableCacheMiss(cause CacheCause) bool {
	return cause != CacheCauseHit && cacheReusableCauses[cause]
}
