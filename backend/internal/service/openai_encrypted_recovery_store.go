package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// OpenAIEncryptedRecoveryKey 标识一段 lineage：同一分组、同一会话、发往同一
// 账号。被拒密文只对"拒绝过它"的账号生效，绝不跨账号剥离。
type OpenAIEncryptedRecoveryKey struct {
	GroupID     int64
	SessionHash string
	AccountID   int64
}

func (k OpenAIEncryptedRecoveryKey) valid() bool {
	return strings.TrimSpace(k.SessionHash) != "" && k.AccountID > 0
}

// OpenAIEncryptedRecoveryState 是一次请求进场时需要的全部 lineage 视图。
type OpenAIEncryptedRecoveryState struct {
	// Rejected 是请求中已被该账号拒绝过的密文摘要（请求摘要的子集）。
	Rejected map[string]struct{}
	// Provenance 是请求中来历明确的密文摘要 → 生成方（"<domain>#<accountID>"）。
	Provenance map[string]string
	// Unrecoverable 表示该会话 × 账号已熔断：剥离重试后仍被拒。
	Unrecoverable bool
}

// OpenAIEncryptedRecoveryStore 是 invalid_encrypted_content 恢复的共享状态。
// 生产实现由 Redis GatewayCache 提供（蓝绿两槽、worker 共享）；缓存未实现
// 时退化为进程内存储，仅单实例有效。
type OpenAIEncryptedRecoveryStore interface {
	LoadOpenAIEncryptedRecoveryState(ctx context.Context, key OpenAIEncryptedRecoveryKey, digests []string) (OpenAIEncryptedRecoveryState, error)
	AddOpenAIEncryptedRejected(ctx context.Context, key OpenAIEncryptedRecoveryKey, digests []string, maxEntries int, ttl time.Duration) error
	RecordOpenAIEncryptedProvenance(ctx context.Context, groupID int64, sessionHash string, entries map[string]string, maxEntries int, ttl time.Duration) error
	MarkOpenAIEncryptedUnrecoverable(ctx context.Context, key OpenAIEncryptedRecoveryKey, ttl time.Duration) error
	// AllowOpenAIEncryptedRetry 是账号级令牌桶：容量与每分钟补充量均为 perMinute。
	AllowOpenAIEncryptedRetry(ctx context.Context, accountID int64, perMinute int, now time.Time) (bool, error)
	IncrementOpenAIEncryptedRecoveryMetric(ctx context.Context, field string, ttl time.Duration) error
}

type memoryOpenAIEncryptedRecoveryEntry struct {
	members   map[string]string
	expiresAt time.Time
}

type memoryOpenAIEncryptedBucket struct {
	tokens float64
	last   time.Time
}

// memoryOpenAIEncryptedRecoveryStore 是无 Redis 时的进程内退化实现。
type memoryOpenAIEncryptedRecoveryStore struct {
	mu            sync.Mutex
	rejected      map[string]*memoryOpenAIEncryptedRecoveryEntry
	provenance    map[string]*memoryOpenAIEncryptedRecoveryEntry
	unrecoverable map[string]time.Time
	buckets       map[int64]*memoryOpenAIEncryptedBucket
	metrics       map[string]int64
	now           func() time.Time
}

func newMemoryOpenAIEncryptedRecoveryStore() *memoryOpenAIEncryptedRecoveryStore {
	return &memoryOpenAIEncryptedRecoveryStore{
		rejected:      make(map[string]*memoryOpenAIEncryptedRecoveryEntry),
		provenance:    make(map[string]*memoryOpenAIEncryptedRecoveryEntry),
		unrecoverable: make(map[string]time.Time),
		buckets:       make(map[int64]*memoryOpenAIEncryptedBucket),
		metrics:       make(map[string]int64),
		now:           time.Now,
	}
}

// 进程内退化存储的总键数上限，超出时先清理过期项，仍超出则丢弃新键。
const memoryOpenAIEncryptedRecoveryMaxKeys = 65536

func memoryOpenAIEncryptedRecoveryKey(key OpenAIEncryptedRecoveryKey) string {
	return fmt.Sprintf("%d:%s:%d", key.GroupID, key.SessionHash, key.AccountID)
}

func memoryOpenAIEncryptedSessionKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%d:%s", groupID, sessionHash)
}

func (s *memoryOpenAIEncryptedRecoveryStore) liveEntry(m map[string]*memoryOpenAIEncryptedRecoveryEntry, key string, now time.Time) *memoryOpenAIEncryptedRecoveryEntry {
	entry, ok := m[key]
	if !ok {
		return nil
	}
	if now.After(entry.expiresAt) {
		delete(m, key)
		return nil
	}
	return entry
}

func (s *memoryOpenAIEncryptedRecoveryStore) sweepLocked(now time.Time) {
	for _, m := range []map[string]*memoryOpenAIEncryptedRecoveryEntry{s.rejected, s.provenance} {
		if len(m) < memoryOpenAIEncryptedRecoveryMaxKeys {
			continue
		}
		for k, entry := range m {
			if now.After(entry.expiresAt) {
				delete(m, k)
			}
		}
	}
	if len(s.unrecoverable) >= memoryOpenAIEncryptedRecoveryMaxKeys {
		for k, until := range s.unrecoverable {
			if now.After(until) {
				delete(s.unrecoverable, k)
			}
		}
	}
}

func (s *memoryOpenAIEncryptedRecoveryStore) LoadOpenAIEncryptedRecoveryState(_ context.Context, key OpenAIEncryptedRecoveryKey, digests []string) (OpenAIEncryptedRecoveryState, error) {
	state := OpenAIEncryptedRecoveryState{}
	if !key.valid() {
		return state, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if until, ok := s.unrecoverable[memoryOpenAIEncryptedRecoveryKey(key)]; ok && now.Before(until) {
		state.Unrecoverable = true
	}
	rejected := s.liveEntry(s.rejected, memoryOpenAIEncryptedRecoveryKey(key), now)
	provenance := s.liveEntry(s.provenance, memoryOpenAIEncryptedSessionKey(key.GroupID, key.SessionHash), now)
	for _, digest := range digests {
		if rejected != nil {
			if _, hit := rejected.members[digest]; hit {
				if state.Rejected == nil {
					state.Rejected = make(map[string]struct{})
				}
				state.Rejected[digest] = struct{}{}
			}
		}
		if provenance != nil {
			if origin, hit := provenance.members[digest]; hit {
				if state.Provenance == nil {
					state.Provenance = make(map[string]string)
				}
				state.Provenance[digest] = origin
			}
		}
	}
	return state, nil
}

func (s *memoryOpenAIEncryptedRecoveryStore) addMembers(m map[string]*memoryOpenAIEncryptedRecoveryEntry, key string, entries map[string]string, maxEntries int, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry := s.liveEntry(m, key, now)
	if entry == nil {
		s.sweepLocked(now)
		if len(m) >= memoryOpenAIEncryptedRecoveryMaxKeys {
			return
		}
		entry = &memoryOpenAIEncryptedRecoveryEntry{members: make(map[string]string, len(entries))}
		m[key] = entry
	}
	for member, value := range entries {
		if _, exists := entry.members[member]; !exists && len(entry.members) >= maxEntries {
			continue
		}
		entry.members[member] = value
	}
	entry.expiresAt = now.Add(ttl)
}

func (s *memoryOpenAIEncryptedRecoveryStore) AddOpenAIEncryptedRejected(_ context.Context, key OpenAIEncryptedRecoveryKey, digests []string, maxEntries int, ttl time.Duration) error {
	if !key.valid() || len(digests) == 0 {
		return nil
	}
	entries := make(map[string]string, len(digests))
	for _, digest := range digests {
		entries[digest] = ""
	}
	s.addMembers(s.rejected, memoryOpenAIEncryptedRecoveryKey(key), entries, maxEntries, ttl)
	return nil
}

func (s *memoryOpenAIEncryptedRecoveryStore) RecordOpenAIEncryptedProvenance(_ context.Context, groupID int64, sessionHash string, entries map[string]string, maxEntries int, ttl time.Duration) error {
	if strings.TrimSpace(sessionHash) == "" || len(entries) == 0 {
		return nil
	}
	s.addMembers(s.provenance, memoryOpenAIEncryptedSessionKey(groupID, sessionHash), entries, maxEntries, ttl)
	return nil
}

func (s *memoryOpenAIEncryptedRecoveryStore) MarkOpenAIEncryptedUnrecoverable(_ context.Context, key OpenAIEncryptedRecoveryKey, ttl time.Duration) error {
	if !key.valid() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	s.unrecoverable[memoryOpenAIEncryptedRecoveryKey(key)] = now.Add(ttl)
	return nil
}

func (s *memoryOpenAIEncryptedRecoveryStore) AllowOpenAIEncryptedRetry(_ context.Context, accountID int64, perMinute int, now time.Time) (bool, error) {
	if perMinute <= 0 {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	capacity := float64(perMinute)
	bucket, ok := s.buckets[accountID]
	if !ok {
		bucket = &memoryOpenAIEncryptedBucket{tokens: capacity, last: now}
		s.buckets[accountID] = bucket
	}
	if elapsed := now.Sub(bucket.last); elapsed > 0 {
		bucket.tokens += elapsed.Minutes() * capacity
		if bucket.tokens > capacity {
			bucket.tokens = capacity
		}
		bucket.last = now
	}
	if bucket.tokens < 1 {
		return false, nil
	}
	bucket.tokens--
	return true, nil
}

func (s *memoryOpenAIEncryptedRecoveryStore) IncrementOpenAIEncryptedRecoveryMetric(_ context.Context, field string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics[field]++
	return nil
}

func (s *memoryOpenAIEncryptedRecoveryStore) metric(field string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metrics[field]
}
