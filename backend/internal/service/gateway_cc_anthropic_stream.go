package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// These diagnostics contain protocol metadata only, never request/response text.
type ccAnthropicStreamState struct {
	Started       bool   `json:"message_start"`
	Stopped       bool   `json:"message_stop"`
	StopReason    string `json:"stop_reason,omitempty"`
	LastEvent     string `json:"last_event,omitempty"`
	FinalUsage    bool   `json:"final_usage"`
	UpstreamError string `json:"upstream_error_type,omitempty"`
}

type ccAnthropicStreamError struct {
	code  string
	cause error
}

func (e *ccAnthropicStreamError) Error() string { return e.code }
func (e *ccAnthropicStreamError) Unwrap() error { return e.cause }

type ccStreamWriteError struct{ error }

func (e *ccStreamWriteError) Unwrap() error { return e.error }

// readCCAnthropicStream validates the terminal BEFORE passing message_stop to a
// converter: that event causes the compatibility state machines to emit success.
// Stop reading at the protocol terminal, rather than waiting for HTTP EOF (some
// compatible upstreams keep sending heartbeats after message_stop).
// The callback's bool distinguishes explicit output usage (including zero)
// from input/cache-only deltas sent by compatible providers.
func readCCAnthropicStream(ctx context.Context, body io.Reader, maxLineSize int, consume func(*apicompat.AnthropicStreamEvent, bool) error) (ccAnthropicStreamState, error) {
	var state ccAnthropicStreamState
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, min(64*1024, maxLineSize)), maxLineSize)
	var eventName string
	var data strings.Builder

	dispatch := func() (bool, error) {
		name, payload := eventName, data.String()
		eventName = ""
		data.Reset()
		if payload == "" && name != "error" {
			return false, nil
		}
		if !strings.HasPrefix(strings.TrimSpace(payload), "{") {
			return false, &ccAnthropicStreamError{code: "upstream_stream_invalid_event"}
		}
		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return false, &ccAnthropicStreamError{code: "upstream_stream_invalid_event", cause: err}
		}
		if event.Type == "" {
			event.Type = name
		}
		state.LastEvent = event.Type
		if len(state.LastEvent) > 128 {
			state.LastEvent = state.LastEvent[:128]
		}
		if event.Type == "error" || name == "error" {
			var envelope struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(payload), &envelope)
			state.UpstreamError = envelope.Error.Type
			if len(state.UpstreamError) > 128 {
				state.UpstreamError = state.UpstreamError[:128]
			}
			return false, &ccAnthropicStreamError{code: "upstream_stream_error"}
		}
		hasOutputUsage := event.Type == "message_delta" && event.Usage != nil && gjson.Get(payload, "usage.output_tokens").Exists()
		switch event.Type {
		case "message_start":
			if state.Started || event.Message == nil {
				return false, &ccAnthropicStreamError{code: "upstream_stream_invalid_start"}
			}
			state.Started = true
		case "message_delta":
			if !state.Started {
				return false, &ccAnthropicStreamError{code: "upstream_stream_missing_start"}
			}
			if event.Delta != nil && strings.TrimSpace(event.Delta.StopReason) != "" {
				state.StopReason = event.Delta.StopReason
			}
			if hasOutputUsage {
				state.FinalUsage = true
			}
		case "message_stop":
			if !state.Started || state.StopReason == "" {
				return false, &ccAnthropicStreamError{code: "upstream_stream_invalid_terminal"}
			}
			state.Stopped = true
		case "content_block_start", "content_block_delta", "content_block_stop":
			if !state.Started {
				return false, &ccAnthropicStreamError{code: "upstream_stream_missing_start"}
			}
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := consume(&event, hasOutputUsage); err != nil {
			return false, err
		}
		return state.Stopped, nil
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		line := scanner.Text()
		if line == "" {
			done, err := dispatch()
			if err != nil || done {
				return state, err
			}
			continue
		}
		if name, ok := extractOpenAISSEEventLine(line); ok {
			eventName = name
		} else if payload, ok := extractOpenAISSEDataLine(line); ok {
			if data.Len()+len(payload)+1 > maxLineSize {
				return state, &ccAnthropicStreamError{code: "upstream_stream_event_too_large"}
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(payload)
		}
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if err := scanner.Err(); err != nil {
		return state, &ccAnthropicStreamError{code: "upstream_stream_read_error", cause: err}
	}
	// Accept an unterminated final SSE frame, but never synthesize a terminal.
	if done, err := dispatch(); err != nil || done {
		return state, err
	}
	return state, &ccAnthropicStreamError{code: "upstream_stream_incomplete", cause: io.ErrUnexpectedEOF}
}

func ccStreamContext(c *gin.Context) context.Context {
	if c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

// Do not write a JSON error here. Leaving the writer uncommitted lets the
// handler's existing bounded account-failover loop retry this attempt. Once any
// SSE bytes have been sent, that same handler must send an error without retry.
func ccAnthropicStreamFailure(c *gin.Context, resp *http.Response, state ccAnthropicStreamState, err error) error {
	var writeErr *ccStreamWriteError
	if errors.As(err, &writeErr) || ccStreamContext(c).Err() != nil {
		return err
	}
	code := "upstream_stream_error"
	var protocolErr *ccAnthropicStreamError
	if errors.As(err, &protocolErr) {
		code = protocolErr.code
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"type": "upstream_error", "code": code, "message": "Upstream response stream failed"},
	})
	detail, _ := json.Marshal(state)
	kind := "failover"
	if c.Writer.Written() {
		kind = "stream_error"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: PlatformAnthropic, UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID: resp.Header.Get("x-request-id"), Kind: kind,
		Message: code, Reason: code, Detail: string(detail),
	})
	return &UpstreamFailoverError{
		StatusCode: http.StatusBadGateway, ResponseBody: body, ResponseHeaders: resp.Header.Clone(),
	}
}

func ccStreamWriteFailure(err error) error {
	return &ccStreamWriteError{fmt.Errorf("write chat completions stream: %w", err)}
}
