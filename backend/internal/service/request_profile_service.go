package service

import (
	"context"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"time"
)

type RequestProfileFilter struct {
	Models, GroupIDs, AccountIDs                    []string
	IncludeOptions                                  bool
	From, To                                        time.Time
	Model, Protocol, ErrorType, RequestID, Evidence string
	GroupID, AccountID, ID                          int64
	Page, Limit                                     int
}
type RequestProfileRow struct {
	ID              int64                   `json:"id"`
	CreatedAt       time.Time               `json:"created_at"`
	RequestID       string                  `json:"request_id"`
	ClientRequestID string                  `json:"client_request_id"`
	AccountID       int64                   `json:"account_id"`
	AccountName     string                  `json:"account_name"`
	GroupName       string                  `json:"group_name"`
	Status          int                     `json:"status"`
	Profile         requestprofile.Snapshot `json:"profile"`
}
type RequestProfileStage struct {
	Name   string  `json:"name"`
	MeanUS float64 `json:"mean_us"`
}
type RequestProfileSummary struct {
	LocalReselect int64                 `json:"local_reselect"`
	Count         int64                 `json:"count"`
	MeanUS        float64               `json:"mean_us"`
	P90US         float64               `json:"p90_us"`
	Truncated     int64                 `json:"truncated"`
	Retries       int64                 `json:"retries"`
	Fallbacks     int64                 `json:"fallbacks"`
	Stages        []RequestProfileStage `json:"stages"`
}
type RequestProfileOption struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Label string `json:"label"`
}
type RequestProfileResult struct {
	Options           []RequestProfileOption  `json:"options,omitempty"`
	RetentionHours    int                     `json:"retention_hours"`
	RecordingGroupIDs []int64                 `json:"recording_group_ids"`
	RecordingEnabled  bool                    `json:"recording_enabled"`
	Rows              []RequestProfileRow     `json:"rows"`
	Summary           RequestProfileSummary   `json:"summary"`
	Page              int                     `json:"page"`
	Limit             int                     `json:"limit"`
	Health            *OpsSystemLogSinkHealth `json:"health,omitempty"`
}
type requestProfileRepository interface {
	QueryRequestProfiles(context.Context, RequestProfileFilter) (*RequestProfileResult, error)
}

func (s *OpsService) QueryRequestProfiles(ctx context.Context, f RequestProfileFilter) (*RequestProfileResult, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	r, ok := s.opsRepo.(requestProfileRepository)
	if !ok {
		return nil, infraerrors.ServiceUnavailable("PROFILE_REPOSITORY_UNAVAILABLE", "Request profiling unavailable")
	}
	if f.From.IsZero() || f.To.IsZero() || !f.From.Before(f.To) || f.To.Sub(f.From) > 24*time.Hour {
		return nil, infraerrors.BadRequest("INVALID_PROFILE_WINDOW", "Time range must be positive and at most 24 hours")
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Limit < 1 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	result, err := r.QueryRequestProfiles(ctx, f)
	if err != nil {
		return nil, err
	}
	if s.cfg != nil {
		result.RetentionHours = s.cfg.Gateway.RequestProfilingRetentionHours
		result.RecordingGroupIDs = append([]int64{}, s.cfg.Gateway.RequestProfilingGroupIDs...)
	}
	result.RecordingEnabled = s.cfg == nil || s.cfg.Gateway.RequestProfilingEnabled
	if s.systemLogSink != nil {
		h := s.systemLogSink.Health()
		h.LastError = "" // Do not expose database diagnostic strings through profiling.
		result.Health = &h
	}
	return result, nil
}
