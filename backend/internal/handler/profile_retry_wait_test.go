package handler

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientProfileRepeated429ThenFallbackIncludesRetryWaits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/101" {
			w.WriteHeader(http.StatusTooManyRequests)
		} else if r.URL.Path == "/33" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer upstream.Close()
	ctx := requestprofile.Attach(context.Background(), time.Now())
	for i, account := range []int64{22, 22, 22, 33, 33, 34} {
		path := "/101"
		if account == 33 {
			path = "/33"
		} else if account == 34 {
			path = "/fallback"
		}
		req, err := http.NewRequestWithContext(ctx, "GET", upstream.URL+path, nil)
		require.NoError(t, err)
		observed, finish := requestprofile.ObserveHTTP(req, account)
		resp, err := upstream.Client().Do(observed)
		finish(resp, err)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if i < 2 || i == 3 {
			require.True(t, waitForSameAccountRetry(ctx, time.Millisecond))
		}
	}
	profile := requestprofile.Finish(ctx, time.Now())
	require.Equal(t, 6, profile.Attempts)
	retries, fallbacks, waits := 0, 0, 0
	for _, e := range profile.Events {
		if e.Kind == "retry" {
			retries++
		}
		if e.Kind == "fallback" {
			fallbacks++
			require.Contains(t, []int64{33, 34}, e.AccountID)
		}
	}
	for _, span := range profile.Spans {
		if span.Name == "retry_backoff" {
			waits++
			require.Greater(t, span.EndUS, span.StartUS)
			require.False(t, span.Incomplete)
		}
	}
	require.Equal(t, 3, retries)
	require.Equal(t, 2, fallbacks)
	require.Equal(t, 3, waits)
	var total int64
	for _, segment := range profile.Segments {
		total += segment.DurationUS
	}
	require.Equal(t, profile.TotalUS, total)
}
func TestClientCancellationClosesRetryWaitWithoutAnotherAttempt(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx := requestprofile.Attach(parent, time.Now())
	cancel()
	require.False(t, waitForSameAccountRetry(ctx, time.Hour))
	profile := requestprofile.Finish(ctx, time.Now())
	require.Zero(t, profile.Attempts)
	require.Len(t, profile.Spans, 1)
	require.Equal(t, "retry_backoff", profile.Spans[0].Name)
	require.False(t, profile.Spans[0].Incomplete)
}
