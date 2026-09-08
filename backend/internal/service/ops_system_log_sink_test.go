package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

func TestOpsSystemLogSink_ShouldIndex(t *testing.T) {
	sink := &OpsSystemLogSink{}

	cases := []struct {
		name  string
		event *logger.LogEvent
		want  bool
	}{
		{
			name:  "warn level",
			event: &logger.LogEvent{Level: "warn", Component: "app"},
			want:  true,
		},
		{
			name:  "error level",
			event: &logger.LogEvent{Level: "error", Component: "app"},
			want:  true,
		},
		{
			name:  "access component",
			event: &logger.LogEvent{Level: "info", Component: "http.access"},
			want:  true,
		},
		{
			name: "rejected access excluded from database sink",
			event: &logger.LogEvent{
				Level:     "info",
				Component: "http.access",
				Fields:    map[string]any{logger.OpsSystemLogSkipField: true},
			},
			want: false,
		},
		{
			name: "access component from fields (real zap path)",
			event: &logger.LogEvent{
				Level:     "info",
				Component: "",
				Fields:    map[string]any{"component": "http.access"},
			},
			want: true,
		},
		{
			name:  "audit component",
			event: &logger.LogEvent{Level: "info", Component: "audit.log_config_change"},
			want:  true,
		},
		{
			name: "audit component from fields (real zap path)",
			event: &logger.LogEvent{
				Level:     "info",
				Component: "",
				Fields:    map[string]any{"component": "audit.log_config_change"},
			},
			want: true,
		},
		{
			name:  "plain info",
			event: &logger.LogEvent{Level: "info", Component: "app"},
			want:  false,
		},
	}

	for _, tc := range cases {
		if got := sink.shouldIndex(tc.event); got != tc.want {
			t.Fatalf("%s: shouldIndex()=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOpsSystemLogSink_LocalCapacityInfoAllowlist(t *testing.T) {
	sink := &OpsSystemLogSink{}
	for _, component := range []string{"handler.openai_gateway.responses", "handler.openai_gateway.chat_completions"} {
		for _, message := range []string{"openai.local_capacity_reselect_eligible", "openai.local_capacity_reselect", "openai.local_capacity_reselect_admitted"} {
			for _, fromField := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/field_%t", component, message, fromField), func(t *testing.T) {
					event := &logger.LogEvent{Level: "info", Component: component, Message: message, Fields: map[string]any{"origin": "local_account_admission"}}
					if fromField {
						event.Component = "unrelated.zap.logger.name"
						event.Fields["component"] = component
					}
					if !sink.shouldIndex(event) {
						t.Fatal("exact local admission info event must reach the database sink")
					}
					event.Fields[logger.OpsSystemLogSkipField] = true
					if sink.shouldIndex(event) {
						t.Fatal("explicit Ops skip must override the local admission allowlist")
					}
				})
			}
		}
	}
}

func TestOpsSystemLogSink_LocalCapacityInfoDoesNotBroadenOtherEvents(t *testing.T) {
	sink := &OpsSystemLogSink{}
	tests := []struct {
		name   string
		mutate func(*logger.LogEvent)
	}{
		{"wrong component", func(e *logger.LogEvent) { e.Component = "handler.other" }},
		{"component prefix", func(e *logger.LogEvent) { e.Component = "prefix.handler.openai_gateway.responses" }},
		{"component suffix", func(e *logger.LogEvent) { e.Component += ".other" }},
		{"field component overrides matching logger", func(e *logger.LogEvent) { e.Fields["component"] = "handler.other" }},
		{"other event", func(e *logger.LogEvent) { e.Message = "openai.account_selected" }},
		{"event prefix", func(e *logger.LogEvent) { e.Message = "prefix." + e.Message }},
		{"event suffix", func(e *logger.LogEvent) { e.Message += ".extra" }},
		{"event trailing whitespace", func(e *logger.LogEvent) { e.Message += " " }},
		{"ordinary info", func(e *logger.LogEvent) { e.Message = "ordinary informational event" }},
		{"debug event", func(e *logger.LogEvent) { e.Level = "debug" }},
		{"missing origin", func(e *logger.LogEvent) { delete(e.Fields, "origin") }},
		{"wrong origin", func(e *logger.LogEvent) { e.Fields["origin"] = "upstream" }},
		{"origin whitespace", func(e *logger.LogEvent) { e.Fields["origin"] = " local_account_admission " }},
		{"origin number", func(e *logger.LogEvent) { e.Fields["origin"] = 1 }},
		{"origin boolean", func(e *logger.LogEvent) { e.Fields["origin"] = true }},
		{"origin stringer", func(e *logger.LogEvent) { e.Fields["origin"] = stringerValue("local_account_admission") }},
		{"origin array", func(e *logger.LogEvent) { e.Fields["origin"] = []string{"local_account_admission"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := &logger.LogEvent{Level: "info", Component: "handler.openai_gateway.responses", Message: "openai.local_capacity_reselect", Fields: map[string]any{"origin": "local_account_admission"}}
			test.mutate(event)
			if sink.shouldIndex(event) {
				t.Fatal("unrelated event must not be admitted by the local-capacity info exception")
			}
		})
	}
	for _, level := range []string{"info", "warn", "error"} {
		event := &logger.LogEvent{Level: level, Component: "handler.openai_gateway.responses", Message: "openai.local_capacity_reselect", Fields: map[string]any{"origin": "local_account_admission", logger.OpsSystemLogSkipField: true}}
		if sink.shouldIndex(event) {
			t.Fatalf("explicit skip must remain first for level %s", level)
		}
	}
}

func TestOpsSystemLogSink_LocalCapacityInfoPersistsThroughQueueAndFlush(t *testing.T) {
	var captured []*OpsInsertSystemLogInput
	repo := &opsRepoMock{BatchInsertSystemLogsFn: func(ctx context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		captured = append(captured, inputs...)
		return int64(len(inputs)), nil
	}}
	sink := NewOpsSystemLogSink(repo)
	t.Cleanup(sink.Stop)
	messages := []string{"openai.local_capacity_reselect_eligible", "openai.local_capacity_reselect", "openai.local_capacity_reselect_admitted"}
	for _, component := range []string{"handler.openai_gateway.responses", "handler.openai_gateway.chat_completions"} {
		for _, fromField := range []bool{false, true} {
			for index, message := range messages {
				fields := map[string]any{"origin": "local_account_admission", "request_id": "synthetic-capacity-request", "client_request_id": "synthetic-capacity-client",
					"user_id": int64(9001), "api_key_id": int64(9002), "group_id": int64(9003), "account_id": int64(9004), "source_account_id": int64(9004),
					"platform": "openai", "model": "gpt-5.6-terra", "reason_code": "gateway_concurrency_limit", "original_wait_ms": int64(1600)}
				if index == 1 {
					fields["wait_ms"] = int64(1600)
					fields["local_reselect_count"] = 1
				}
				if index == 2 {
					fields["selected_account_id"] = int64(9005)
					fields["local_reselect_count"] = 1
				}
				event := &logger.LogEvent{Time: time.Now().UTC(), Level: "info", Component: component, Message: message, Fields: fields}
				if fromField {
					event.Component = "unrelated.zap.logger.name"
					fields["component"] = component
				}
				sink.WriteLogEvent(event)
			}
		}
	}
	if got := len(sink.queue); got != 12 {
		t.Fatalf("queue contains %d local admission events, want 12", got)
	}
	// Drain through the real worker and conversion to the repository input. Stop
	// joins that worker, so assertions below do not race its captured batch.
	sink.Start()
	sink.Stop()
	if len(captured) != 12 {
		t.Fatalf("persisted call contains %d events, want 12", len(captured))
	}
	for index, item := range captured {
		wantComponent := "handler.openai_gateway.responses"
		if index >= 6 {
			wantComponent = "handler.openai_gateway.chat_completions"
		}
		if item.Component != wantComponent || item.Level != "info" || item.Message != messages[index%3] {
			t.Fatalf("unexpected indexed event identity at %d: %+v", index, item)
		}
		if item.RequestID != "synthetic-capacity-request" || item.ClientRequestID != "synthetic-capacity-client" {
			t.Fatalf("request IDs lost at %d", index)
		}
		if item.UserID == nil || *item.UserID != 9001 || item.APIKeyID == nil || *item.APIKeyID != 9002 || item.AccountID == nil || *item.AccountID != 9004 {
			t.Fatalf("indexed identity IDs lost at %d", index)
		}
		var extra map[string]any
		if err := json.Unmarshal([]byte(item.ExtraJSON), &extra); err != nil {
			t.Fatal(err)
		}
		if extra["origin"] != "local_account_admission" || extra["group_id"] != float64(9003) || extra["source_account_id"] != float64(9004) || extra["reason_code"] != "gateway_concurrency_limit" {
			t.Fatalf("local origin fields lost at %d: %s", index, item.ExtraJSON)
		}
		if index%3 == 2 && extra["selected_account_id"] != float64(9005) {
			t.Fatalf("admitted backup ID lost: %s", item.ExtraJSON)
		}
	}
	if sink.Health().WrittenCount != 12 {
		t.Fatalf("written count = %d, want 12", sink.Health().WrittenCount)
	}
}

func TestOpsSystemLogSink_WriteLogEvent_ShouldDropWhenQueueFull(t *testing.T) {
	sink := &OpsSystemLogSink{
		queue: make(chan *logger.LogEvent, 1),
	}

	sink.WriteLogEvent(&logger.LogEvent{Level: "warn", Component: "app"})
	sink.WriteLogEvent(&logger.LogEvent{Level: "warn", Component: "app"})

	if got := len(sink.queue); got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
	if dropped := atomic.LoadUint64(&sink.droppedCount); dropped != 1 {
		t.Fatalf("droppedCount = %d, want 1", dropped)
	}
}

func TestOpsSystemLogSink_Health(t *testing.T) {
	sink := &OpsSystemLogSink{
		queue: make(chan *logger.LogEvent, 10),
	}
	sink.lastError.Store("db timeout")
	atomic.StoreUint64(&sink.droppedCount, 3)
	atomic.StoreUint64(&sink.writeFailed, 2)
	atomic.StoreUint64(&sink.writtenCount, 5)
	atomic.StoreUint64(&sink.totalDelayNs, uint64(5000000)) // 5ms total -> avg 1ms
	sink.queue <- &logger.LogEvent{Level: "warn", Component: "app"}
	sink.queue <- &logger.LogEvent{Level: "warn", Component: "app"}

	health := sink.Health()
	if health.QueueDepth != 2 {
		t.Fatalf("queue depth = %d, want 2", health.QueueDepth)
	}
	if health.QueueCapacity != 10 {
		t.Fatalf("queue capacity = %d, want 10", health.QueueCapacity)
	}
	if health.DroppedCount != 3 {
		t.Fatalf("dropped = %d, want 3", health.DroppedCount)
	}
	if health.WriteFailed != 2 {
		t.Fatalf("write failed = %d, want 2", health.WriteFailed)
	}
	if health.WrittenCount != 5 {
		t.Fatalf("written = %d, want 5", health.WrittenCount)
	}
	if health.AvgWriteDelayMs != 1 {
		t.Fatalf("avg delay ms = %d, want 1", health.AvgWriteDelayMs)
	}
	if health.LastError != "db timeout" {
		t.Fatalf("last error = %q, want db timeout", health.LastError)
	}
}

func TestOpsSystemLogSink_StartStopAndFlushSuccess(t *testing.T) {
	done := make(chan struct{}, 1)
	var captured []*OpsInsertSystemLogInput
	repo := &opsRepoMock{
		BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
			captured = append(captured, inputs...)
			select {
			case done <- struct{}{}:
			default:
			}
			return int64(len(inputs)), nil
		},
	}

	sink := NewOpsSystemLogSink(repo)
	sink.host = "api-node-1"
	sink.batchSize = 1
	sink.flushInterval = 10 * time.Millisecond
	sink.Start()
	defer sink.Stop()

	sink.WriteLogEvent(&logger.LogEvent{
		Time:      time.Now().UTC(),
		Level:     "warn",
		Component: "http.access",
		Message:   `authorization="Bearer sk-test-123"`,
		Fields: map[string]any{
			"component":         "http.access",
			"request_id":        "req-1",
			"client_request_id": "creq-1",
			"user_id":           "12",
			"api_key_id":        int64(56),
			"account_id":        json.Number("34"),
			"platform":          "openai",
			"model":             "gpt-5",
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for sink flush")
	}

	if len(captured) != 1 {
		t.Fatalf("captured len = %d, want 1", len(captured))
	}
	item := captured[0]
	if item.Host != "api-node-1" {
		t.Fatalf("host = %q, want api-node-1", item.Host)
	}
	if item.RequestID != "req-1" || item.ClientRequestID != "creq-1" {
		t.Fatalf("unexpected request ids: %+v", item)
	}
	if item.UserID == nil || *item.UserID != 12 {
		t.Fatalf("unexpected user_id: %+v", item.UserID)
	}
	if item.APIKeyID == nil || *item.APIKeyID != 56 {
		t.Fatalf("unexpected api_key_id: %+v", item.APIKeyID)
	}
	if item.AccountID == nil || *item.AccountID != 34 {
		t.Fatalf("unexpected account_id: %+v", item.AccountID)
	}
	if strings.TrimSpace(item.Message) == "" {
		t.Fatalf("message should not be empty")
	}
	// writtenCount is incremented after BatchInsertSystemLogsFn returns,
	// so poll briefly to avoid a race between the done signal and the atomic add.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sink.Health().WrittenCount > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	health := sink.Health()
	if health.WrittenCount == 0 {
		t.Fatalf("written_count should be >0")
	}
}

func TestOpsSystemLogSink_FlushFailureUpdatesHealth(t *testing.T) {
	repo := &opsRepoMock{
		BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
			return 0, errors.New("db unavailable")
		},
	}
	sink := NewOpsSystemLogSink(repo)
	sink.batchSize = 1
	sink.flushInterval = 10 * time.Millisecond
	sink.Start()
	defer sink.Stop()

	sink.WriteLogEvent(&logger.LogEvent{
		Time:      time.Now().UTC(),
		Level:     "warn",
		Component: "app",
		Message:   "boom",
		Fields:    map[string]any{},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		health := sink.Health()
		if health.WriteFailed > 0 {
			if !strings.Contains(health.LastError, "db unavailable") {
				t.Fatalf("unexpected last error: %s", health.LastError)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("write_failed_count not updated")
}

func TestOpsSystemLogSink_StopFlushUsesActiveContextAndDrainsQueue(t *testing.T) {
	var inserted int64
	var canceledCtxCalls int64
	repo := &opsRepoMock{
		BatchInsertSystemLogsFn: func(ctx context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
			if err := ctx.Err(); err != nil {
				atomic.AddInt64(&canceledCtxCalls, 1)
				return 0, err
			}
			atomic.AddInt64(&inserted, int64(len(inputs)))
			return int64(len(inputs)), nil
		},
	}

	sink := NewOpsSystemLogSink(repo)
	sink.batchSize = 200
	sink.flushInterval = time.Hour
	sink.Start()

	sink.WriteLogEvent(&logger.LogEvent{
		Time:      time.Now().UTC(),
		Level:     "warn",
		Component: "app",
		Message:   "pending-on-shutdown",
		Fields:    map[string]any{"component": "http.access"},
	})

	sink.Stop()

	if got := atomic.LoadInt64(&inserted); got != 1 {
		t.Fatalf("inserted = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&canceledCtxCalls); got != 0 {
		t.Fatalf("canceled ctx calls = %d, want 0", got)
	}
	health := sink.Health()
	if health.WrittenCount != 1 {
		t.Fatalf("written_count = %d, want 1", health.WrittenCount)
	}
}

type stringerValue string

func (s stringerValue) String() string { return string(s) }

func TestOpsSystemLogSink_HelperFunctions(t *testing.T) {
	src := map[string]any{"a": 1}
	cloned := copyMap(src)
	src["a"] = 2
	v, ok := cloned["a"].(int)
	if !ok || v != 1 {
		t.Fatalf("copyMap should create copy")
	}
	if got := asString(stringerValue(" hello ")); got != "hello" {
		t.Fatalf("asString stringer = %q", got)
	}
	if got := asString(fmt.Errorf("x")); got != "" {
		t.Fatalf("asString error should be empty, got %q", got)
	}
	if got := asString(123); got != "" {
		t.Fatalf("asString non-string should be empty, got %q", got)
	}

	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{in: 5, want: 5, ok: true},
		{in: int64(6), want: 6, ok: true},
		{in: float64(7), want: 7, ok: true},
		{in: json.Number("8"), want: 8, ok: true},
		{in: "9", want: 9, ok: true},
		{in: "0", ok: false},
		{in: -1, ok: false},
		{in: "abc", ok: false},
	}
	for _, tc := range cases {
		got := asInt64Ptr(tc.in)
		if tc.ok {
			if got == nil || *got != tc.want {
				t.Fatalf("asInt64Ptr(%v) = %+v, want %d", tc.in, got, tc.want)
			}
		} else if got != nil {
			t.Fatalf("asInt64Ptr(%v) should be nil, got %d", tc.in, *got)
		}
	}
}

func TestNormalizeSystemLogHost(t *testing.T) {
	if got := normalizeSystemLogHost(" api-node-1 ", nil); got != "api-node-1" {
		t.Fatalf("trimmed host = %q, want api-node-1", got)
	}
	if got := normalizeSystemLogHost("", nil); got != "unknown" {
		t.Fatalf("empty host = %q, want unknown", got)
	}
	if got := normalizeSystemLogHost("api-node-1", errors.New("hostname unavailable")); got != "unknown" {
		t.Fatalf("errored host = %q, want unknown", got)
	}
	longHost := strings.Repeat("节", maxSystemLogHostLength+1)
	got := normalizeSystemLogHost(longHost, nil)
	if runeCount := len([]rune(got)); runeCount != maxSystemLogHostLength {
		t.Fatalf("truncated host rune count = %d, want %d", runeCount, maxSystemLogHostLength)
	}
}
