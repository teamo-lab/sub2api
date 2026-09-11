package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/relay"
	"github.com/gin-gonic/gin"
)

type teamoLineScanner interface {
	Scan() bool
	Text() string
	Err() error
}

type teamoRelayScanner struct {
	reader    *relay.LineReader
	ctx       context.Context
	committed func() bool
	deadline  time.Time
	interval  time.Duration
	line      string
	err       error
}

func (s *OpenAIGatewayService) newTeamoRelayScanner(c *gin.Context, resp *http.Response, committed func() bool, started time.Time, reasoningEffort string) *teamoRelayScanner {
	r := &teamoRelayScanner{reader: relay.NewLineReader(resp.Body, openAIFirstOutputStageMaxBytes+openAIFirstOutputScannerFramingAllowance), ctx: c.Request.Context(), committed: committed}
	if s.cfg != nil {
		r.interval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	if timeout := s.openAIFirstOutputTimeout(reasoningEffort); timeout > 0 {
		r.deadline = started.Add(timeout)
	}
	return r
}

func (r *teamoRelayScanner) Scan() bool {
	if r.err != nil {
		return false
	}
	ctx, timeout := r.ctx, r.interval
	if r.committed() {
		// Preserve the existing post-disconnect usage drain. The configured read
		// idle bound still applies; cancellation before commit stops immediately.
		ctx = context.WithoutCancel(ctx)
	} else if !r.deadline.IsZero() {
		remaining := time.Until(r.deadline)
		if remaining <= 0 {
			r.err = relay.ErrReadTimeout
			return false
		}
		if timeout <= 0 || remaining < timeout {
			timeout = remaining
		}
	}
	r.line, r.err = r.reader.Next(ctx, timeout)
	return r.err == nil
}

func (r *teamoRelayScanner) Text() string { return r.line }
func (r *teamoRelayScanner) Err() error {
	if errors.Is(r.err, io.EOF) {
		return nil
	}
	return r.err
}
func (r *teamoRelayScanner) Close() error { return r.reader.Close() }
