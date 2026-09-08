package requestprofile

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
)

// ObserveHTTP composes transport callbacks; it neither buffers nor reads bodies.
// URL, request headers, credentials and error text are deliberately not captured.
func ObserveHTTP(req *http.Request, account int64) (*http.Request, func(*http.Response, error)) {
	if req == nil || From(req.Context()) == nil {
		return req, func(*http.Response, error) {}
	}
	doneHTTP := activeHTTP(req.Context())
	endPreparation(req.Context())
	ctx := NewAttempt(req.Context(), account)
	endHeaders := Start(ctx, "upstream_headers")
	var mu sync.Mutex
	ends := map[string]func(){}
	begin := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		if old := ends[name]; old != nil {
			old()
		}
		ends[name] = Start(ctx, name)
	}
	end := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		if f := ends[name]; f != nil {
			f()
			delete(ends, name)
		}
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { begin("connection_wait") }, GotConn: func(httptrace.GotConnInfo) { end("connection_wait"); begin("request_write") },
		DNSStart: func(httptrace.DNSStartInfo) { begin("dns") }, DNSDone: func(httptrace.DNSDoneInfo) { end("dns") },
		ConnectStart: func(string, string) { begin("connect") }, ConnectDone: func(string, string, error) { end("connect") },
		TLSHandshakeStart: func() { begin("tls") }, TLSHandshakeDone: func(tls.ConnectionState, error) { end("tls") },
		WroteRequest:         func(httptrace.WroteRequestInfo) { end("request_write"); begin("upstream_wait") },
		GotFirstResponseByte: func() { end("upstream_wait"); Mark(ctx, "upstream_first_byte", 0) },
	}
	cloned := req.WithContext(httptrace.WithClientTrace(ctx, trace))
	return cloned, func(resp *http.Response, err error) {
		endHeaders()
		mu.Lock()
		for k, f := range ends {
			f()
			delete(ends, k)
		}
		mu.Unlock()
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		kind := "upstream_response"
		if err != nil {
			kind = failureKind(err)
		}
		Mark(ctx, kind, status)
		if resp != nil && resp.Body != nil {
			endBody := Start(ctx, "response_body")
			resp.Body = &profileBody{ctx: ctx, ReadCloser: resp.Body, done: func() { endBody(); doneHTTP() }}
		} else {
			doneHTTP()
		}
	}
}

type profileBody struct {
	ctx context.Context
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *profileBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if e != nil {
		b.once.Do(func() {
			if !errors.Is(e, io.EOF) {
				Mark(b.ctx, failureKind(e), 0)
			}
			b.done()
		})
	}
	return n, e
}
func (b *profileBody) Close() error { e := b.ReadCloser.Close(); b.once.Do(b.done); return e }

func failureKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var timed net.Error
	if errors.As(err, &timed) && timed.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "upstream_cancelled"
	}
	return "network_error"
}
