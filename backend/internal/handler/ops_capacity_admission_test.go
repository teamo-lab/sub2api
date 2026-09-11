package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type userAdmissionFailureCache struct{ helperConcurrencyCacheStub }

func (*userAdmissionFailureCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	return false, errors.New("synthetic datastore unavailable")
}

func TestOpsCapacityUserAdmissionDependencyFailureIsNotExcluded(t *testing.T) {
	helper := NewConcurrencyHelper(service.NewConcurrencyService(&userAdmissionFailureCache{}), SSEPingFormatNone, time.Second)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
	started := false
	_, err := helper.AcquireUserSlotWithWait(c, 1, 1, false, &started)
	require.Error(t, err)
	require.False(t, service.HasOpsClientBusinessLimited(c))
}

func TestOpsCapacityUserWaitQueueGetsExplicitContractMarker(t *testing.T) {
	helper := NewConcurrencyHelper(service.NewConcurrencyService(&helperConcurrencyCacheStub{userSeq: []bool{false}, waitAllowed: false}), SSEPingFormatNone, time.Second)
	c, _ := newHelperTestContext(http.MethodPost, "/v1/responses")
	started := false
	_, err := helper.AcquireUserSlotWithWait(c, 1, 1, false, &started)
	var full *WaitQueueFullError
	require.ErrorAs(t, err, &full)
	require.True(t, service.HasOpsClientBusinessLimited(c))
	require.Equal(t, service.OpsClientBusinessLimitedReasonUserWaitQueue, service.OpsClientBusinessLimitedReason(c))
}
