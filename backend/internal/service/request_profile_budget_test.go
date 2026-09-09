package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"strings"
	"testing"
)

func TestProfileQueueHasEncodedByteBudget(t *testing.T) {
	s := NewOpsSystemLogSink(nil)
	defer s.Stop()
	event := &logger.LogEvent{Level: "info", Component: "http.access", Message: "http request completed", Fields: map[string]any{"request_profile": strings.Repeat("x", 80<<10)}}
	for i := 0; i < 600; i++ {
		s.WriteLogEvent(event)
	}
	h := s.Health()
	if h.ProfileQueueBytes > profileQueueByteLimit || h.ProfileRejectedCount == 0 || h.DroppedCount == 0 {
		t.Fatal(h)
	}
	if event.Fields == nil || event.EncodedFields != nil {
		t.Fatal("caller event mutated")
	}
	for len(s.queue) > 0 {
		e := <-s.queue
		if e.Fields != nil || len(e.EncodedFields) == 0 {
			t.Fatal("unbounded object retained")
		}
		releaseProfileBytes(s, []*logger.LogEvent{e})
	}
	if s.Health().ProfileQueueBytes != 0 {
		t.Fatal("budget leaked")
	}
	s.WriteLogEvent(&logger.LogEvent{Level: "error", Message: "ordinary", Fields: map[string]any{"message": "kept"}})
	if len(s.queue) != 1 {
		t.Fatal("ordinary errors blocked by profile quota")
	}
}
func TestProfileEncodedBytesReleaseAfterFlush(t *testing.T) {
	repo := &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, in []*OpsInsertSystemLogInput) (int64, error) { return int64(len(in)), nil }}
	s := NewOpsSystemLogSink(repo)
	s.Start()
	for i := 0; i < 20; i++ {
		s.WriteLogEvent(&logger.LogEvent{Level: "info", Component: "http.access", Message: "http request completed", Fields: map[string]any{"request_profile": map[string]any{"version": 1}}})
	}
	s.Stop()
	h := s.Health()
	if h.ProfileQueueBytes != 0 || h.WrittenCount != 20 {
		t.Fatal(h)
	}
}
