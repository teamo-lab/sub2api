package service

import (
	"sync"
	"time"
)

// Gemini 空回复冷却的前提：这个（账号, 模型）最近的 200 回复里，空回复 / 200 里带错误占多数。
//
// 只看「1 分钟 5 次空回复」不够：11-23 HK 上线 #197 后，codingrouter 1132 的
// gemini-3.8-flash 一分钟成功约 40 次、空回复约 5 次（成功率 89%），也被冷却了 1 分钟；
// 当时 1133 刚被熔断摘掉、同组能接 3.8 的号本来就少，冷掉一个大部分时间能用的号，
// 3.8 SLA 从约 95% 掉到 71–87%。冷却只该落在「基本只回空」的号上（RootFlow 1138/1119
// 那种 3/3 空 200），它们才是每个请求都白撞一跳的那一类。
//
// 计数只在本进程内：每个 Gemini 200 都会经过 guardGeminiEmptyResponse，在那里顺手记一笔，
// 不给热路径加 Redis 调用。多实例（蓝绿同时在线）各自判断，冷却写进共享的 model_rate_limits。
const (
	geminiReplyHealthBucket   = 10 * time.Second
	geminiReplyHealthBuckets  = 12 // 共 2 分钟
	geminiReplyMinUnusableShr = 0.5
)

type geminiReplyKey struct {
	accountID int64
	model     string
}

type geminiReplyCounts struct {
	slot   [geminiReplyHealthBuckets]int64 // 这个桶属于哪个 10 秒区间（unix 秒 / 10）
	usable [geminiReplyHealthBuckets]int64
	broken [geminiReplyHealthBuckets]int64
}

type geminiReplyHealthTracker struct {
	mu  sync.Mutex
	m   map[geminiReplyKey]*geminiReplyCounts
	now func() time.Time
}

func newGeminiReplyHealthTracker() *geminiReplyHealthTracker {
	return &geminiReplyHealthTracker{m: make(map[geminiReplyKey]*geminiReplyCounts), now: time.Now}
}

// geminiReplyHealth 是进程内唯一的计数器。
var geminiReplyHealth = newGeminiReplyHealthTracker()

// observe 记一次 200 回复：usable=true 是有回答，false 是空回复 / 200 里带上游错误。
func (t *geminiReplyHealthTracker) observe(accountID int64, model string, usable bool) {
	if t == nil || accountID <= 0 || model == "" {
		return
	}
	id := t.now().Unix() / int64(geminiReplyHealthBucket/time.Second)
	idx := int(id % geminiReplyHealthBuckets)
	key := geminiReplyKey{accountID: accountID, model: model}

	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.m[key]
	if c == nil {
		c = &geminiReplyCounts{}
		t.m[key] = c
	}
	if c.slot[idx] != id {
		c.slot[idx], c.usable[idx], c.broken[idx] = id, 0, 0
	}
	if usable {
		c.usable[idx]++
	} else {
		c.broken[idx]++
	}
}

// counts 返回窗口内的有回答 / 坏回复次数。
func (t *geminiReplyHealthTracker) counts(accountID int64, model string) (usable, broken int64) {
	if t == nil {
		return 0, 0
	}
	id := t.now().Unix() / int64(geminiReplyHealthBucket/time.Second)
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.m[geminiReplyKey{accountID: accountID, model: model}]
	if c == nil {
		return 0, 0
	}
	for i := range c.slot {
		if c.slot[i] > id-geminiReplyHealthBuckets && c.slot[i] <= id {
			usable += c.usable[i]
			broken += c.broken[i]
		}
	}
	return usable, broken
}

// mostlyUnusable 表示这个（账号, 模型）最近的 200 回复里坏回复占多数，可以冷却。
// 没有样本时返回 false：宁可不冷，单次失败已经由 failover 换号处理。
func (t *geminiReplyHealthTracker) mostlyUnusable(accountID int64, model string) bool {
	usable, broken := t.counts(accountID, model)
	total := usable + broken
	if total == 0 {
		return false
	}
	return float64(broken)/float64(total) >= geminiReplyMinUnusableShr
}
