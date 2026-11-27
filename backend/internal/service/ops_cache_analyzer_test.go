package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuildCacheRequestIR_AnthropicIgnoresMovingBreakpoint(t *testing.T) {
	turn1 := []byte(`{"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	turn2 := []byte(`{"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":"hello"},{"role":"user","content":[{"type":"text","text":"more","cache_control":{"type":"ephemeral"}}]}]}`)
	ir1, ok := BuildCacheRequestIR(CacheProtocolAnthropic, turn1)
	require.True(t, ok)
	ir2, ok := BuildCacheRequestIR(CacheProtocolAnthropic, turn2)
	require.True(t, ok)
	require.Equal(t, ir1.Tip(), ir2.Chain[1], "turn 2 must contain turn 1's prefix even though cache_control moved")
	require.Equal(t, 0, ir1.AssistantTurns)
	require.Equal(t, 1, ir2.AssistantTurns)
	require.True(t, ir2.ExplicitBreakpoint)
	require.Equal(t, ir1.SessionKey, ir2.SessionKey)
}

func TestBuildCacheRequestIR_ProtocolsNormalizeToSameShape(t *testing.T) {
	chat, ok := BuildCacheRequestIR(CacheProtocolOpenAIChat, []byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"q2"}],"prompt_cache_key":"k1"}`))
	require.True(t, ok)
	require.Len(t, chat.Chain, 5) // 头部 + 4 条消息（chat 的 system 也是一条消息）
	require.Equal(t, 4, chat.MessageCount())
	require.Equal(t, 1, chat.AssistantTurns)

	resp, ok := BuildCacheRequestIR(CacheProtocolOpenAIResponses, []byte(`{"instructions":"i","input":"hello","previous_response_id":"resp_1"}`))
	require.True(t, ok)
	require.Equal(t, 1, resp.MessageCount())
	require.Equal(t, "resp_1", resp.PreviousResponseID)

	gem, ok := BuildCacheRequestIR(CacheProtocolGemini, []byte(`{"systemInstruction":{"parts":[{"text":"s"}]},"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[{"text":"b"}]},{"role":"user","parts":[{"text":"c"}]}],"cachedContent":"cachedContents/x"}`))
	require.True(t, ok)
	require.Equal(t, 3, gem.MessageCount())
	require.Equal(t, 1, gem.AssistantTurns)
	require.True(t, gem.ExplicitCachedContent)

	_, ok = BuildCacheRequestIR(CacheProtocolGemini, []byte(`not json`))
	require.False(t, ok)
	_, ok = BuildCacheRequestIR(CacheProtocolOpenAIChat, []byte(`{"messages":[]}`))
	require.False(t, ok)
}

type cacheTestRequest struct {
	protocol CacheProtocol
	platform string
	body     string
	account  int64
	at       time.Time
	input    int64
	read     int64
	respID   string
}

func newTestCacheAnalyzer() *OpsCacheAnalyzer {
	return NewOpsCacheAnalyzer(config.OpsCacheAnalysisConfig{Enabled: true}, nil, nil)
}

func (a *OpsCacheAnalyzer) testClassify(t *testing.T, req cacheTestRequest) []cacheCausePart {
	t.Helper()
	probe := a.buildProbe(req.protocol, []byte(req.body))
	require.NotNil(t, probe.IR, "probe must build an IR: %s", probe.Skipped)
	probe.AccountPlatform = req.platform
	probe.ResponseID = req.respID
	return a.classify(cacheObservation{
		probe: probe, at: req.at, accountID: req.account, model: "m",
		inputTokens: req.input, cacheReadTokens: req.read,
	})
}

func causeOf(parts []cacheCausePart) CacheCause { return parts[0].cause }

const (
	cacheTurn1 = `{"contents":[{"role":"user","parts":[{"text":"q1"}]}]}`
	cacheTurn2 = `{"contents":[{"role":"user","parts":[{"text":"q1"}]},{"role":"model","parts":[{"text":"a1"}]},{"role":"user","parts":[{"text":"q2"}]}]}`
)

func TestOpsCacheAnalyzer_ClassifiesMissCauses(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)
	base := cacheTestRequest{protocol: CacheProtocolGemini, platform: PlatformGemini, account: 1150, input: 5000}

	t.Run("single turn then same-account miss is an upstream miss", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r1 := base
		r1.body, r1.at = cacheTurn1, t0
		require.Equal(t, CacheCauseSingleTurn, causeOf(a.testClassify(t, r1)))
		r2 := base
		r2.body, r2.at = cacheTurn2, t0.Add(time.Minute)
		require.Equal(t, CacheCauseUpstreamMiss, causeOf(a.testClassify(t, r2)))
	})

	t.Run("account switch", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r1 := base
		r1.body, r1.at = cacheTurn1, t0
		a.testClassify(t, r1)
		r2 := base
		r2.body, r2.at, r2.account = cacheTurn2, t0.Add(time.Minute), 1149
		require.Equal(t, CacheCauseAccountSwitch, causeOf(a.testClassify(t, r2)))
	})

	t.Run("ttl expired", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r1 := base
		r1.body, r1.at = cacheTurn1, t0
		a.testClassify(t, r1)
		r2 := base
		r2.body, r2.at = cacheTurn2, t0.Add(20*time.Minute)
		require.Equal(t, CacheCauseTTLExpired, causeOf(a.testClassify(t, r2)))
	})

	t.Run("hit splits cache read and incremental tokens", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r := base
		r.body, r.at, r.read = cacheTurn2, t0, 4000
		parts := a.testClassify(t, r)
		require.Equal(t, CacheCauseHit, parts[0].cause)
		require.EqualValues(t, 4000, parts[0].tokens)
		require.Equal(t, CacheCauseIncremental, parts[1].cause)
		require.EqualValues(t, 5000, parts[1].tokens)
	})

	t.Run("multi-turn never seen is a new session", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r := base
		r.body, r.at = cacheTurn2, t0
		require.Equal(t, CacheCauseNewSession, causeOf(a.testClassify(t, r)))
	})

	t.Run("rewritten history in the same session is prefix drift", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r1 := base
		r1.body, r1.at = cacheTurn2, t0
		a.testClassify(t, r1)
		r2 := base
		r2.body = `{"contents":[{"role":"user","parts":[{"text":"q1"}]},{"role":"model","parts":[{"text":"a1 edited"}]},{"role":"user","parts":[{"text":"q2"}]},{"role":"model","parts":[{"text":"a2"}]},{"role":"user","parts":[{"text":"q3"}]}]}`
		r2.at = t0.Add(time.Minute)
		require.Equal(t, CacheCausePrefixDrift, causeOf(a.testClassify(t, r2)))
	})

	t.Run("short prompt is below the cacheable minimum", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r := base
		r.body, r.at, r.input = cacheTurn2, t0, 300
		require.Equal(t, CacheCauseBelowMin, causeOf(a.testClassify(t, r)))
	})

	t.Run("anthropic without cache_control only counts on anthropic accounts", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		body := `{"messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`
		r := cacheTestRequest{protocol: CacheProtocolAnthropic, platform: PlatformAnthropic, body: body, account: 36, at: t0, input: 5000}
		require.Equal(t, CacheCauseNoBreakpoint, causeOf(a.testClassify(t, r)))
		a2 := newTestCacheAnalyzer()
		r.platform = PlatformGemini
		require.Equal(t, CacheCauseNewSession, causeOf(a2.testClassify(t, r)))
	})

	t.Run("responses state machine follows previous_response_id", func(t *testing.T) {
		a := newTestCacheAnalyzer()
		r1 := cacheTestRequest{protocol: CacheProtocolOpenAIResponses, platform: PlatformOpenAI, account: 1116, at: t0, input: 5000,
			body: `{"instructions":"i","input":[{"role":"user","content":"q1"}]}`, respID: "resp_1"}
		require.Equal(t, CacheCauseSingleTurn, causeOf(a.testClassify(t, r1)))
		r2 := r1
		r2.body, r2.at, r2.respID = `{"instructions":"i","input":[{"role":"user","content":"q2"}],"previous_response_id":"resp_1"}`, t0.Add(time.Minute), "resp_2"
		require.Equal(t, CacheCauseUpstreamMiss, causeOf(a.testClassify(t, r2)))
		r3 := r2
		r3.body, r3.at, r3.account = `{"input":[{"role":"user","content":"q3"}],"previous_response_id":"resp_2"}`, t0.Add(2*time.Minute), 1094
		require.Equal(t, CacheCauseAccountSwitch, causeOf(a.testClassify(t, r3)))
		r4 := r1
		r4.body, r4.respID = `{"input":"x","previous_response_id":"resp_unknown"}`, ""
		require.Equal(t, CacheCauseStateLost, causeOf(a.testClassify(t, r4)))
	})
}

type recordingCacheStore struct{ rows []OpsCacheAnalysisBucket }

func (s *recordingCacheStore) UpsertCacheAnalysisBuckets(_ context.Context, rows []OpsCacheAnalysisBucket) error {
	s.rows = append(s.rows, rows...)
	return nil
}

func (s *recordingCacheStore) QueryCacheAnalysis(context.Context, *OpsDashboardFilter) (*OpsCacheAnalysisResponse, error) {
	return nil, nil
}

func TestOpsCacheAnalyzer_AggregatesUnanalyzedAndFlushes(t *testing.T) {
	store := &recordingCacheStore{}
	a := NewOpsCacheAnalyzer(config.OpsCacheAnalysisConfig{Enabled: true}, store, nil)
	groupID := int64(85)
	at := time.Date(2026, 11, 27, 3, 2, 30, 0, time.UTC)
	a.process(cacheObservation{at: at, userID: 1, groupID: groupID, accountID: 7, model: "gemini-3.8-flash", inputTokens: 900, cacheReadTokens: 100, inputCost: 0.009})
	a.process(cacheObservation{at: at, userID: 1, groupID: groupID, accountID: 7, model: "gemini-3.8-flash", inputTokens: 50})
	a.flush(context.Background())

	got := map[CacheCause]OpsCacheAnalysisBucket{}
	for _, row := range store.rows {
		require.Equal(t, time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC), row.BucketStart)
		got[row.Cause] = row
	}
	require.EqualValues(t, 100, got[CacheCauseHit].Tokens)
	require.EqualValues(t, 1, got[CacheCauseHit].RequestCount)
	require.InDelta(t, 0.001, got[CacheCauseHit].SavedUSD, 1e-9)
	require.EqualValues(t, 950, got[CacheCauseUnanalyzed].Tokens)
	require.EqualValues(t, 1, got[CacheCauseUnanalyzed].RequestCount)
}

func TestFinalizeCacheAnalysis_Rates(t *testing.T) {
	resp := &OpsCacheAnalysisResponse{Causes: []OpsCacheAnalysisCauseStat{
		{Cause: CacheCauseHit, Requests: 2, Tokens: 300},
		{Cause: CacheCauseSingleTurn, Requests: 5, Tokens: 600},
		{Cause: CacheCauseUpstreamMiss, Requests: 1, Tokens: 100},
	}}
	FinalizeCacheAnalysis(resp)
	require.Len(t, resp.Causes, len(AllCacheCauses))
	require.EqualValues(t, 1000, resp.PromptTokens)
	require.EqualValues(t, 700, resp.UncachedTokens)
	require.InDelta(t, 0.3, *resp.HitRate, 1e-9)
	require.InDelta(t, 0.75, *resp.ReusableHitRate, 1e-9)
}

func TestCacheEntryCodecRoundTrip(t *testing.T) {
	in := cacheEntry{AtMs: 1795748000000, AccountID: 1150, Depth: 12, Tip: 0xdeadbeefcafe}
	out, ok := decodeCacheEntry(encodeCacheEntry(in))
	require.True(t, ok)
	require.Equal(t, in, out)
	_, ok = decodeCacheEntry("garbage")
	require.False(t, ok)
}

func TestCacheLRUEvictsOldest(t *testing.T) {
	l := newCacheLRU(16) // one entry per shard
	l.put(1, cacheEntry{Depth: 1})
	l.put(17, cacheEntry{Depth: 17}) // same shard as 1
	_, ok := l.get(1)
	require.False(t, ok)
	got, ok := l.get(17)
	require.True(t, ok)
	require.EqualValues(t, 17, got.Depth)
}
