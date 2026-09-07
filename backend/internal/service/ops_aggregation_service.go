package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	opsAggHourlyJobName      = "ops_preaggregation_hourly"
	opsAggDailyJobName       = "ops_preaggregation_daily"
	opsAggModel5mJobName     = "ops_model_preaggregation_5m"
	opsAggModelHourlyJobName = "ops_model_preaggregation_hourly"

	opsAggHourlyInterval = 10 * time.Minute
	opsAggDailyInterval  = 1 * time.Hour

	// Keep in sync with ops retention target (vNext default 30d).
	opsAggBackfillWindow = 1 * time.Hour

	// Recompute overlap to absorb late-arriving rows near boundaries.
	opsAggHourlyOverlap = 2 * time.Hour
	opsAggDailyOverlap  = 48 * time.Hour

	opsAggHourlyChunk = 24 * time.Hour
	opsAggDailyChunk  = 7 * 24 * time.Hour

	// Delay around boundaries (e.g. 10:00..10:05) to avoid aggregating buckets
	// that may still receive late inserts.
	opsAggSafeDelay = 5 * time.Minute

	opsAggMaxQueryTimeout = 5 * time.Second
	opsAggHourlyTimeout   = 5 * time.Minute
	opsAggDailyTimeout    = 2 * time.Minute

	opsAggHourlyLeaderLockKey      = "ops:aggregation:hourly:leader"
	opsAggDailyLeaderLockKey       = "ops:aggregation:daily:leader"
	opsAggModel5mLeaderLockKey     = "ops:aggregation:model:5m:leader"
	opsAggModelHourlyLeaderLockKey = "ops:aggregation:model:hourly:leader"

	opsAggHourlyLeaderLockTTL = 15 * time.Minute
	opsAggDailyLeaderLockTTL  = 10 * time.Minute

	opsModel5mRetention     = 14 * 24 * time.Hour
	opsModelHourlyRetention = 90 * 24 * time.Hour
)

// OpsAggregationService periodically backfills ops_metrics_hourly / ops_metrics_daily
// for stable long-window dashboard queries.
//
// It is safe to run in multi-replica deployments when Redis is available (leader lock).
type OpsAggregationService struct {
	opsRepo     OpsRepository
	settingRepo SettingRepository
	cfg         *config.Config

	db          *sql.DB
	redisClient *redis.Client
	instanceID  string

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	hourlyMu      sync.Mutex
	dailyMu       sync.Mutex
	model5mMu     sync.Mutex
	modelHourlyMu sync.Mutex

	skipLogMu sync.Mutex
	skipLogAt time.Time
}

func NewOpsAggregationService(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsAggregationService {
	return &OpsAggregationService{
		opsRepo:     opsRepo,
		settingRepo: settingRepo,
		cfg:         cfg,
		db:          db,
		redisClient: redisClient,
		instanceID:  uuid.NewString(),
	}
}

func (s *OpsAggregationService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		if s.stopCh == nil {
			s.stopCh = make(chan struct{})
		}
		go s.hourlyLoop()
		go s.dailyLoop()
		go s.modelAggregationLoop(300, 5*time.Minute)
		go s.modelAggregationLoop(3600, 10*time.Minute)
	})
}

func (s *OpsAggregationService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
}

func (s *OpsAggregationService) hourlyLoop() {
	// First run immediately.
	s.aggregateHourly()

	ticker := time.NewTicker(opsAggHourlyInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.aggregateHourly()
		case <-s.stopCh:
			return
		}
	}
}

func (s *OpsAggregationService) dailyLoop() {
	// First run immediately.
	s.aggregateDaily()

	ticker := time.NewTicker(opsAggDailyInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.aggregateDaily()
		case <-s.stopCh:
			return
		}
	}
}

// modelAggregationLoop refreshes recent buckets on every run and walks one
// historical chunk backwards. This bounds each run while eventually filling
// the complete retention window after a fresh deployment.
func (s *OpsAggregationService) modelAggregationLoop(resolutionSeconds int, interval time.Duration) {
	s.aggregateModelMetrics(resolutionSeconds)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.aggregateModelMetrics(resolutionSeconds)
		case <-s.stopCh:
			return
		}
	}
}

func (s *OpsAggregationService) aggregateModelMetrics(resolutionSeconds int) {
	if s == nil || s.opsRepo == nil || (s.cfg != nil && (!s.cfg.Ops.Enabled || !s.cfg.Ops.Aggregation.Enabled)) {
		return
	}
	var mu *sync.Mutex
	var retention, step, chunk time.Duration
	var lockKey, jobName string
	var upsert func(context.Context, time.Time, time.Time) error
	switch resolutionSeconds {
	case 300:
		mu, retention, step, chunk = &s.model5mMu, opsModel5mRetention, 5*time.Minute, 6*time.Hour
		lockKey, jobName, upsert = opsAggModel5mLeaderLockKey, opsAggModel5mJobName, s.opsRepo.UpsertModelMetrics5m
	case 3600:
		mu, retention, step, chunk = &s.modelHourlyMu, opsModelHourlyRetention, time.Hour, 24*time.Hour
		lockKey, jobName, upsert = opsAggModelHourlyLeaderLockKey, opsAggModelHourlyJobName, s.opsRepo.UpsertModelMetricsHourly
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opsAggHourlyTimeout)
	defer cancel()
	if !s.isMonitoringEnabled(ctx) {
		return
	}
	release, ok := s.tryAcquireLeaderLock(ctx, lockKey, opsAggHourlyLeaderLockTTL, "[OpsAggregation][model]")
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}
	mu.Lock()
	defer mu.Unlock()

	startedAt, runAt := time.Now().UTC(), time.Now().UTC()
	end := floorToDuration(time.Now().UTC().Add(-opsAggSafeDelay), step)
	cutoff := floorToDuration(end.Add(-retention), step)
	recentStart := end.Add(-2 * step)
	var aggErr error
	if recentStart.Before(end) {
		aggErr = upsert(ctx, recentStart, end)
	}
	if aggErr == nil {
		missing, found, err := s.opsRepo.GetMissingModelMetricsBucketStart(ctx, resolutionSeconds, cutoff, recentStart)
		if err != nil {
			aggErr = err
		} else if found {
			// Fill the newest hole first. This repairs isolated gaps and also
			// walks a fresh installation backwards in bounded chunks.
			historyEnd := missing.Add(step)
			historyStart := historyEnd.Add(-chunk)
			if historyStart.Before(cutoff) {
				historyStart = cutoff
			}
			if historyStart.Before(historyEnd) {
				aggErr = upsert(ctx, historyStart, historyEnd)
			}
		}
	}

	finishedAt := time.Now().UTC()
	dur := finishedAt.Sub(startedAt).Milliseconds()
	hbCtx, hbCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer hbCancel()
	input := &OpsUpsertJobHeartbeatInput{JobName: jobName, LastRunAt: &runAt, LastDurationMs: &dur}
	if aggErr != nil {
		msg, at := truncateString(aggErr.Error(), 2048), finishedAt
		input.LastError, input.LastErrorAt = &msg, &at
	} else {
		result, at := fmt.Sprintf("resolution=%ds retention=%s", resolutionSeconds, retention), finishedAt
		input.LastResult, input.LastSuccessAt = &result, &at
	}
	_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, input)
}

func floorToDuration(t time.Time, step time.Duration) time.Time {
	return t.UTC().Truncate(step)
}

func (s *OpsAggregationService) aggregateHourly() {
	if s == nil || s.opsRepo == nil {
		return
	}
	if s.cfg != nil {
		if !s.cfg.Ops.Enabled {
			return
		}
		if !s.cfg.Ops.Aggregation.Enabled {
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), opsAggHourlyTimeout)
	defer cancel()

	if !s.isMonitoringEnabled(ctx) {
		return
	}

	release, ok := s.tryAcquireLeaderLock(ctx, opsAggHourlyLeaderLockKey, opsAggHourlyLeaderLockTTL, "[OpsAggregation][hourly]")
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}

	s.hourlyMu.Lock()
	defer s.hourlyMu.Unlock()

	startedAt := time.Now().UTC()
	runAt := startedAt

	// Aggregate stable full hours only.
	end := utcFloorToHour(time.Now().UTC().Add(-opsAggSafeDelay))
	start := end.Add(-opsAggBackfillWindow)

	// Resume from the latest bucket with overlap.
	{
		ctxMax, cancelMax := context.WithTimeout(context.Background(), opsAggMaxQueryTimeout)
		latest, ok, err := s.opsRepo.GetLatestHourlyBucketStart(ctxMax)
		cancelMax()
		if err != nil {
			logger.LegacyPrintf("service.ops_aggregation", "[OpsAggregation][hourly] failed to read latest bucket: %v", err)
		} else if ok {
			candidate := latest.Add(-opsAggHourlyOverlap)
			if candidate.After(start) {
				start = candidate
			}
		}
	}

	start = utcFloorToHour(start)
	if !start.Before(end) {
		return
	}

	var aggErr error
	for cursor := start; cursor.Before(end); cursor = cursor.Add(opsAggHourlyChunk) {
		chunkEnd := minTime(cursor.Add(opsAggHourlyChunk), end)
		if err := s.opsRepo.UpsertHourlyMetrics(ctx, cursor, chunkEnd); err != nil {
			aggErr = err
			logger.LegacyPrintf("service.ops_aggregation", "[OpsAggregation][hourly] upsert failed (%s..%s): %v", cursor.Format(time.RFC3339), chunkEnd.Format(time.RFC3339), err)
			break
		}
	}

	finishedAt := time.Now().UTC()
	durationMs := finishedAt.Sub(startedAt).Milliseconds()
	dur := durationMs

	if aggErr != nil {
		msg := truncateString(aggErr.Error(), 2048)
		errAt := finishedAt
		hbCtx, hbCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer hbCancel()
		_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, &OpsUpsertJobHeartbeatInput{
			JobName:        opsAggHourlyJobName,
			LastRunAt:      &runAt,
			LastErrorAt:    &errAt,
			LastError:      &msg,
			LastDurationMs: &dur,
		})
		return
	}

	successAt := finishedAt
	hbCtx, hbCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer hbCancel()
	result := truncateString(fmt.Sprintf("window=%s..%s", start.Format(time.RFC3339), end.Format(time.RFC3339)), 2048)
	_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, &OpsUpsertJobHeartbeatInput{
		JobName:        opsAggHourlyJobName,
		LastRunAt:      &runAt,
		LastSuccessAt:  &successAt,
		LastDurationMs: &dur,
		LastResult:     &result,
	})
}

func (s *OpsAggregationService) aggregateDaily() {
	if s == nil || s.opsRepo == nil {
		return
	}
	if s.cfg != nil {
		if !s.cfg.Ops.Enabled {
			return
		}
		if !s.cfg.Ops.Aggregation.Enabled {
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), opsAggDailyTimeout)
	defer cancel()

	if !s.isMonitoringEnabled(ctx) {
		return
	}

	release, ok := s.tryAcquireLeaderLock(ctx, opsAggDailyLeaderLockKey, opsAggDailyLeaderLockTTL, "[OpsAggregation][daily]")
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}

	s.dailyMu.Lock()
	defer s.dailyMu.Unlock()

	startedAt := time.Now().UTC()
	runAt := startedAt

	end := utcFloorToDay(time.Now().UTC())
	start := end.Add(-opsAggBackfillWindow)

	{
		ctxMax, cancelMax := context.WithTimeout(context.Background(), opsAggMaxQueryTimeout)
		latest, ok, err := s.opsRepo.GetLatestDailyBucketDate(ctxMax)
		cancelMax()
		if err != nil {
			logger.LegacyPrintf("service.ops_aggregation", "[OpsAggregation][daily] failed to read latest bucket: %v", err)
		} else if ok {
			candidate := latest.Add(-opsAggDailyOverlap)
			if candidate.After(start) {
				start = candidate
			}
		}
	}

	start = utcFloorToDay(start)
	if !start.Before(end) {
		return
	}

	var aggErr error
	for cursor := start; cursor.Before(end); cursor = cursor.Add(opsAggDailyChunk) {
		chunkEnd := minTime(cursor.Add(opsAggDailyChunk), end)
		if err := s.opsRepo.UpsertDailyMetrics(ctx, cursor, chunkEnd); err != nil {
			aggErr = err
			logger.LegacyPrintf("service.ops_aggregation", "[OpsAggregation][daily] upsert failed (%s..%s): %v", cursor.Format("2006-01-02"), chunkEnd.Format("2006-01-02"), err)
			break
		}
	}

	finishedAt := time.Now().UTC()
	durationMs := finishedAt.Sub(startedAt).Milliseconds()
	dur := durationMs

	if aggErr != nil {
		msg := truncateString(aggErr.Error(), 2048)
		errAt := finishedAt
		hbCtx, hbCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer hbCancel()
		_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, &OpsUpsertJobHeartbeatInput{
			JobName:        opsAggDailyJobName,
			LastRunAt:      &runAt,
			LastErrorAt:    &errAt,
			LastError:      &msg,
			LastDurationMs: &dur,
		})
		return
	}

	successAt := finishedAt
	hbCtx, hbCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer hbCancel()
	result := truncateString(fmt.Sprintf("window=%s..%s", start.Format(time.RFC3339), end.Format(time.RFC3339)), 2048)
	_ = s.opsRepo.UpsertJobHeartbeat(hbCtx, &OpsUpsertJobHeartbeatInput{
		JobName:        opsAggDailyJobName,
		LastRunAt:      &runAt,
		LastSuccessAt:  &successAt,
		LastDurationMs: &dur,
		LastResult:     &result,
	})
}

func (s *OpsAggregationService) isMonitoringEnabled(ctx context.Context) bool {
	if s == nil {
		return false
	}
	if s.cfg != nil && !s.cfg.Ops.Enabled {
		return false
	}
	if s.settingRepo == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}

	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpsMonitoringEnabled)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return true
		}
		return true
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "off", "disabled":
		return false
	default:
		return true
	}
}

var opsAggReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (s *OpsAggregationService) tryAcquireLeaderLock(ctx context.Context, key string, ttl time.Duration, logPrefix string) (func(), bool) {
	if s == nil {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Prefer Redis leader lock when available (multi-instance), but avoid stampeding
	// the DB when Redis is flaky by falling back to a DB advisory lock.
	if s.redisClient != nil {
		ok, err := s.redisClient.SetNX(ctx, key, s.instanceID, ttl).Result()
		if err == nil {
			if !ok {
				s.maybeLogSkip(logPrefix)
				return nil, false
			}
			release := func() {
				ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = opsAggReleaseScript.Run(ctx2, s.redisClient, []string{key}, s.instanceID).Result()
			}
			return release, true
		}
		// Redis error: fall through to DB advisory lock.
	}

	release, ok := tryAcquireDBAdvisoryLock(ctx, s.db, hashAdvisoryLockID(key))
	if !ok {
		s.maybeLogSkip(logPrefix)
		return nil, false
	}
	return release, true
}

func (s *OpsAggregationService) maybeLogSkip(prefix string) {
	s.skipLogMu.Lock()
	defer s.skipLogMu.Unlock()

	now := time.Now()
	if !s.skipLogAt.IsZero() && now.Sub(s.skipLogAt) < time.Minute {
		return
	}
	s.skipLogAt = now
	if prefix == "" {
		prefix = "[OpsAggregation]"
	}
	logger.LegacyPrintf("service.ops_aggregation", "%s leader lock held by another instance; skipping", prefix)
}

func utcFloorToHour(t time.Time) time.Time {
	return t.UTC().Truncate(time.Hour)
}

func utcFloorToDay(t time.Time) time.Time {
	u := t.UTC()
	y, m, d := u.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
