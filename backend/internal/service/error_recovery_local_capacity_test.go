package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLocalCapacityRecoveryReservationKeepsOriginalRequestBudget(t *testing.T) {
	c, _, account, failure := recoveryFixture(t, "apikey", 0, 2)
	require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, account, "gpt", failure))
	state := recoveryState(c)
	ctx, deadline, code := c.Request.Context(), state.deadline, state.code
	require.True(t, ReserveErrorRecoveryAccountSwitch(c))
	require.Same(t, state, recoveryState(c))
	require.Equal(t, ctx, c.Request.Context())
	require.Equal(t, deadline, state.deadline)
	require.Equal(t, code, state.code)
	require.Same(t, failure, state.failure)
	require.Equal(t, 2, state.switches)
	require.Empty(t, state.retries)
	require.False(t, ReserveErrorRecoveryAccountSwitch(c), "local admission shares the same switch ceiling")
}

func TestLocalCapacityRecoveryReservationDoesNotInventRecoveryState(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ctx := c.Request.Context()
	require.True(t, ReserveErrorRecoveryAccountSwitch(c))
	require.Nil(t, recoveryState(c))
	require.Equal(t, ctx, c.Request.Context())
}

func TestLocalCapacityRecoveryReservationStopsAtDeadlineOrOutput(t *testing.T) {
	for _, mode := range []string{"deadline", "output", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			c, _, account, failure := recoveryFixture(t, "apikey", 0, 3)
			require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, account, "gpt", failure))
			state := recoveryState(c)
			switch mode {
			case "deadline":
				state.timer.Stop()
				state.deadline = time.Now().Add(-time.Millisecond)
			case "output":
				CompleteErrorRecovery(c)
			case "cancel":
				ctx, cancel := context.WithCancel(c.Request.Context())
				c.Request = c.Request.WithContext(ctx)
				cancel()
			}
			require.False(t, ReserveErrorRecoveryAccountSwitch(c))
			require.Equal(t, 1, state.switches)
		})
	}
}
