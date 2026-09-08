package service

import (
	"context"
	"sync"
	"time"
)

type openAIUpstreamAttemptFactsKey struct{}

// These are dispatch facts only: no retry policy, budget, payload or credentials.
// The pointer follows derived/detached request contexts so internal parameter
// repairs and Responses/Chat adaptations cannot reset the observed count.
type openAIUpstreamAttemptTracker struct {
	mu           sync.Mutex
	count        int
	firstStarted time.Time
}

type OpenAIUpstreamAttemptFacts struct {
	Count   int
	Elapsed time.Duration
}

func WithOpenAIUpstreamAttemptTracking(ctx context.Context) context.Context {
	if _, ok := ctx.Value(openAIUpstreamAttemptFactsKey{}).(*openAIUpstreamAttemptTracker); ok {
		return ctx
	}
	return context.WithValue(ctx, openAIUpstreamAttemptFactsKey{}, &openAIUpstreamAttemptTracker{})
}

func noteOpenAIUpstreamAttempt(ctx context.Context) {
	tracker, _ := ctx.Value(openAIUpstreamAttemptFactsKey{}).(*openAIUpstreamAttemptTracker)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.count++
	if tracker.count == 1 {
		tracker.firstStarted = time.Now()
	}
}

func OpenAIUpstreamAttemptFactsFromContext(ctx context.Context) OpenAIUpstreamAttemptFacts {
	tracker, _ := ctx.Value(openAIUpstreamAttemptFactsKey{}).(*openAIUpstreamAttemptTracker)
	if tracker == nil {
		return OpenAIUpstreamAttemptFacts{}
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	facts := OpenAIUpstreamAttemptFacts{Count: tracker.count}
	if tracker.count > 0 {
		facts.Elapsed = time.Since(tracker.firstStarted)
	}
	return facts
}
