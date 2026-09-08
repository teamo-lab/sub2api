package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIUpstreamAttemptFactsFollowInternalDerivedContexts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := WithOpenAIUpstreamAttemptTracking(context.Background())
		time.Sleep(35 * time.Second) // JSON preparation is not upstream waiting.
		require.Equal(t, OpenAIUpstreamAttemptFacts{}, OpenAIUpstreamAttemptFactsFromContext(ctx))
		noteOpenAIUpstreamAttempt(ctx)
		time.Sleep(35 * time.Second)
		require.Equal(t, OpenAIUpstreamAttemptFacts{Count: 1, Elapsed: 35 * time.Second}, OpenAIUpstreamAttemptFactsFromContext(ctx))
		derived, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		derived = WithOpenAIUpstreamAttemptTracking(derived)
		noteOpenAIUpstreamAttempt(derived)
		time.Sleep(35 * time.Second)
		require.Equal(t, OpenAIUpstreamAttemptFacts{Count: 2, Elapsed: 70 * time.Second}, OpenAIUpstreamAttemptFactsFromContext(ctx))
		require.Equal(t, OpenAIUpstreamAttemptFactsFromContext(ctx), OpenAIUpstreamAttemptFactsFromContext(derived))
		require.Equal(t, OpenAIUpstreamAttemptFacts{}, OpenAIUpstreamAttemptFactsFromContext(context.Background()))
	})
}
