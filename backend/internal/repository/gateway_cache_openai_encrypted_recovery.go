package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// invalid_encrypted_content 恢复的共享状态（蓝绿两槽、worker 共用）。
//
//	rej:{group}:{session}:{account}   SET  被该账号拒绝过的密文摘要（容量上限）
//	prov:{group}:{session}            HASH 密文摘要 → 生成方 "<domain>#<accountID>"
//	dead:{group}:{session}:{account}  STR  剥离重试后仍被拒：熔断
//	bucket:{account}                  HASH 账号级重试令牌桶
//	metrics:{date}                    HASH 计数器
const openAIEncryptedRecoveryPrefix = "openai_encrypted_recovery:"

var _ service.OpenAIEncryptedRecoveryStore = (*gatewayCache)(nil)

func openAIEncryptedRecoveryRejectedKey(key service.OpenAIEncryptedRecoveryKey) string {
	return fmt.Sprintf("%srej:%d:%s:%d", openAIEncryptedRecoveryPrefix, key.GroupID, key.SessionHash, key.AccountID)
}

func openAIEncryptedRecoveryDeadKey(key service.OpenAIEncryptedRecoveryKey) string {
	return fmt.Sprintf("%sdead:%d:%s:%d", openAIEncryptedRecoveryPrefix, key.GroupID, key.SessionHash, key.AccountID)
}

func openAIEncryptedRecoveryProvenanceKey(groupID int64, sessionHash string) string {
	return fmt.Sprintf("%sprov:%d:%s", openAIEncryptedRecoveryPrefix, groupID, sessionHash)
}

func openAIEncryptedRecoveryBucketKey(accountID int64) string {
	return fmt.Sprintf("%sbucket:%d", openAIEncryptedRecoveryPrefix, accountID)
}

func openAIEncryptedRecoveryMetricKey(now time.Time) string {
	cst := time.FixedZone("CST", 8*60*60)
	return openAIEncryptedRecoveryPrefix + "metrics:" + now.In(cst).Format("2006-01-02")
}

var loadOpenAIEncryptedRecoveryStateScript = redis.NewScript(`
local out = {redis.call('EXISTS', KEYS[1])}
for i = 1, #ARGV do
  out[#out + 1] = redis.call('SISMEMBER', KEYS[2], ARGV[i])
  local origin = redis.call('HGET', KEYS[3], ARGV[i])
  if origin == false then origin = '' end
  out[#out + 1] = origin
end
return out
`)

var addOpenAIEncryptedRejectedScript = redis.NewScript(`
local cap = tonumber(ARGV[1])
local n = redis.call('SCARD', KEYS[1])
local added = 0
for i = 3, #ARGV do
  if redis.call('SISMEMBER', KEYS[1], ARGV[i]) == 0 and n < cap then
    redis.call('SADD', KEYS[1], ARGV[i])
    n = n + 1
    added = added + 1
  end
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return added
`)

var recordOpenAIEncryptedProvenanceScript = redis.NewScript(`
local cap = tonumber(ARGV[1])
local n = redis.call('HLEN', KEYS[1])
for i = 3, #ARGV, 2 do
  if redis.call('HEXISTS', KEYS[1], ARGV[i]) == 1 then
    redis.call('HSET', KEYS[1], ARGV[i], ARGV[i + 1])
  elseif n < cap then
    redis.call('HSET', KEYS[1], ARGV[i], ARGV[i + 1])
    n = n + 1
  end
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return n
`)

var allowOpenAIEncryptedRetryScript = redis.NewScript(`
local cap = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local tokens = tonumber(redis.call('HGET', KEYS[1], 't'))
local ts = tonumber(redis.call('HGET', KEYS[1], 'ts'))
if tokens == nil or ts == nil then
  tokens = cap
  ts = now
end
if now > ts then
  tokens = math.min(cap, tokens + (now - ts) * cap / 60000)
  ts = now
end
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', KEYS[1], 't', tostring(tokens))
redis.call('HSET', KEYS[1], 'ts', tostring(ts))
redis.call('PEXPIRE', KEYS[1], 120000)
return allowed
`)

func openAIEncryptedRecoveryTTLMillis(ttl time.Duration) int64 {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return ttl.Milliseconds()
}

func (c *gatewayCache) LoadOpenAIEncryptedRecoveryState(ctx context.Context, key service.OpenAIEncryptedRecoveryKey, digests []string) (service.OpenAIEncryptedRecoveryState, error) {
	state := service.OpenAIEncryptedRecoveryState{}
	if c == nil || c.rdb == nil || strings.TrimSpace(key.SessionHash) == "" || key.AccountID <= 0 {
		return state, nil
	}
	args := make([]any, 0, len(digests))
	for _, digest := range digests {
		args = append(args, digest)
	}
	raw, err := loadOpenAIEncryptedRecoveryStateScript.Run(ctx, c.rdb, []string{
		openAIEncryptedRecoveryDeadKey(key),
		openAIEncryptedRecoveryRejectedKey(key),
		openAIEncryptedRecoveryProvenanceKey(key.GroupID, key.SessionHash),
	}, args...).Slice()
	if err != nil {
		return state, fmt.Errorf("load openai encrypted recovery state: %w", err)
	}
	if len(raw) == 0 {
		return state, nil
	}
	if dead, _ := raw[0].(int64); dead > 0 {
		state.Unrecoverable = true
	}
	for i, digest := range digests {
		pos := 1 + 2*i
		if pos+1 >= len(raw) {
			break
		}
		if member, _ := raw[pos].(int64); member == 1 {
			if state.Rejected == nil {
				state.Rejected = make(map[string]struct{})
			}
			state.Rejected[digest] = struct{}{}
		}
		if origin, _ := raw[pos+1].(string); origin != "" {
			if state.Provenance == nil {
				state.Provenance = make(map[string]string)
			}
			state.Provenance[digest] = origin
		}
	}
	return state, nil
}

func (c *gatewayCache) AddOpenAIEncryptedRejected(ctx context.Context, key service.OpenAIEncryptedRecoveryKey, digests []string, maxEntries int, ttl time.Duration) error {
	if c == nil || c.rdb == nil || strings.TrimSpace(key.SessionHash) == "" || key.AccountID <= 0 || len(digests) == 0 {
		return nil
	}
	args := make([]any, 0, len(digests)+2)
	args = append(args, maxEntries, openAIEncryptedRecoveryTTLMillis(ttl))
	for _, digest := range digests {
		args = append(args, digest)
	}
	return addOpenAIEncryptedRejectedScript.Run(ctx, c.rdb, []string{openAIEncryptedRecoveryRejectedKey(key)}, args...).Err()
}

func (c *gatewayCache) RecordOpenAIEncryptedProvenance(ctx context.Context, groupID int64, sessionHash string, entries map[string]string, maxEntries int, ttl time.Duration) error {
	if c == nil || c.rdb == nil || strings.TrimSpace(sessionHash) == "" || len(entries) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(entries)+2)
	args = append(args, maxEntries, openAIEncryptedRecoveryTTLMillis(ttl))
	for digest, origin := range entries {
		args = append(args, digest, origin)
	}
	return recordOpenAIEncryptedProvenanceScript.Run(ctx, c.rdb, []string{openAIEncryptedRecoveryProvenanceKey(groupID, sessionHash)}, args...).Err()
}

func (c *gatewayCache) MarkOpenAIEncryptedUnrecoverable(ctx context.Context, key service.OpenAIEncryptedRecoveryKey, ttl time.Duration) error {
	if c == nil || c.rdb == nil || strings.TrimSpace(key.SessionHash) == "" || key.AccountID <= 0 {
		return nil
	}
	return c.rdb.Set(ctx, openAIEncryptedRecoveryDeadKey(key), "1", time.Duration(openAIEncryptedRecoveryTTLMillis(ttl))*time.Millisecond).Err()
}

func (c *gatewayCache) AllowOpenAIEncryptedRetry(ctx context.Context, accountID int64, perMinute int, now time.Time) (bool, error) {
	if perMinute <= 0 {
		return false, nil
	}
	if c == nil || c.rdb == nil {
		return true, nil
	}
	allowed, err := allowOpenAIEncryptedRetryScript.Run(ctx, c.rdb, []string{openAIEncryptedRecoveryBucketKey(accountID)},
		strconv.Itoa(perMinute), strconv.FormatInt(now.UnixMilli(), 10)).Int64()
	if err != nil {
		return false, fmt.Errorf("openai encrypted recovery token bucket: %w", err)
	}
	return allowed == 1, nil
}

func (c *gatewayCache) IncrementOpenAIEncryptedRecoveryMetric(ctx context.Context, field string, ttl time.Duration) error {
	if c == nil || c.rdb == nil || strings.TrimSpace(field) == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 8 * 24 * time.Hour
	}
	key := openAIEncryptedRecoveryMetricKey(time.Now())
	pipe := c.rdb.TxPipeline()
	pipe.HIncrBy(ctx, key, field, 1)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}
