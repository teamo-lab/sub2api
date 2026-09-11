package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProfileReasoningBoundary(t *testing.T) {
	for _, tc := range []struct {
		event, item string
		reason, end bool
	}{
		{"response.reasoning_summary_text.delta", "", true, false},
		{"response.reasoning_summary_text.done", "", true, true},
		{"response.output_item.added", "reasoning", true, false},
		{"response.output_item.done", "reasoning", true, true},
		{"response.created", "", false, false}, {"response.in_progress", "", false, false}, {"", "", false, false},
		{"response.output_text.delta", "", false, true}, {"response.output_item.added", "message", false, true},
		{"response.function_call_arguments.delta", "", false, true}, {"response.completed", "", false, true}, {"response.failed", "", false, true},
	} {
		r, e := profileReasoningBoundary(tc.event, tc.item)
		if r != tc.reason || e != tc.end {
			t.Fatalf("%s/%s: %v %v", tc.event, tc.item, r, e)
		}
	}
}

func TestProfileReasoningNativeStreamBeforeAndAfterOutput(t *testing.T) {
	start := time.Now()
	ctx := requestprofile.Attach(context.Background(), start)
	requestprofile.Metadata(ctx, 3, "gpt-6-astra", "sse")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	observed, complete := requestprofile.ObserveHTTP(req, 22)
	reader, writer := io.Pipe()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}
	complete(resp, nil)
	c, w := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	_ = w
	go func() {
		defer writer.Close()
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_qa"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_qa","type":"reasoning","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_qa","delta":"PRIVATE_THOUGHT_FIXTURE"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_qa","type":"reasoning","summary":[]}}`,
			`{"type":"response.output_text.delta","delta":"Hello"}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"id":"rs_qa2","type":"reasoning","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_qa2","delta":"PRIVATE_THOUGHT_FIXTURE"}`,
			`{"type":"response.output_item.done","output_index":2,"item":{"id":"rs_qa2","type":"reasoning","summary":[]}}`,
			`{"type":"response.output_text.delta","delta":" world"}`,
			`{"type":"response.completed","response":{"id":"resp_qa","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20}}}`,
		} {
			fmt.Fprintf(writer, "data: %s\n\n", event)
			time.Sleep(15 * time.Millisecond)
		}
	}()
	svc := &OpenAIGatewayService{}
	_, err := svc.handleStreamingResponse(observed.Context(), resp, c, &Account{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, start, "gpt-6-astra", "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	profile := requestprofile.Finish(ctx, time.Now())
	totals := map[string]int64{}
	for _, s := range profile.Segments {
		totals[s.Name] += s.DurationUS
	}
	if totals["reasoning_observed_before_output"] <= 0 || totals["reasoning_observed_after_output"] <= 0 {
		t.Fatalf("missing reasoning phases: %+v", totals)
	}
	encoded, _ := json.Marshal(profile)
	if strings.Contains(string(encoded), "PRIVATE_THOUGHT_FIXTURE") {
		t.Fatal("reasoning content captured")
	}
}
