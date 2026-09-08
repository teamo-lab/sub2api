package middleware

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/gin-gonic/gin"
	"net/http"
)

// Preserve gin's interfaces and delegate writes/flushes exactly once. Payloads
// are never parsed or retained here; semantic delivery is marked by the gateway.
type requestProfileWriter struct {
	gin.ResponseWriter
	ctx context.Context
}

func (w *requestProfileWriter) Write(b []byte) (int, error) {
	n, e := w.ResponseWriter.Write(b)
	requestprofile.DownstreamWrite(w.ctx, n, len(b), e)
	return n, e
}
func (w *requestProfileWriter) WriteString(s string) (int, error) {
	n, e := w.ResponseWriter.WriteString(s)
	requestprofile.DownstreamWrite(w.ctx, n, len(s), e)
	return n, e
}
func (w *requestProfileWriter) Flush() {
	w.ResponseWriter.Flush()
	requestprofile.DownstreamFlush(w.ctx)
}
func (w *requestProfileWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
