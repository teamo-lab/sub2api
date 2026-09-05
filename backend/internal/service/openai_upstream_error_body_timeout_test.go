package service

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type closeUnblocksErrorBody struct {
	payload []byte
	closed  chan struct{}
	once    sync.Once
}

func (b *closeUnblocksErrorBody) Read(dst []byte) (int, error) {
	if len(b.payload) > 0 {
		n := copy(dst, b.payload)
		b.payload = b.payload[n:]
		return n, nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *closeUnblocksErrorBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestReadOpenAIUpstreamErrorBodyWithTimeoutPreservesPartialBodyAndUnblocks(t *testing.T) {
	body := &closeUnblocksErrorBody{
		payload: []byte("{\"error\":{\"code\":\"invalid_json_schema\"}}"),
		closed:  make(chan struct{}),
	}
	startedAt := time.Now()
	payload, timedOut, err := readOpenAIUpstreamErrorBodyWithTimeout(body, 4096, 20*time.Millisecond)

	require.NoError(t, err)
	require.True(t, timedOut)
	require.Less(t, time.Since(startedAt), time.Second)
	require.JSONEq(t, "{\"error\":{\"code\":\"invalid_json_schema\"}}", string(payload))
	select {
	case <-body.closed:
	default:
		t.Fatal("timed-out response body was not closed")
	}
}

func TestReadOpenAIUpstreamErrorBodyWithTimeoutKeepsNormalResponse(t *testing.T) {
	responseBody := []byte("{\"error\":{\"message\":\"bad request\"}}")
	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(responseBody))}

	payload, timedOut, err := readOpenAIUpstreamErrorBodyWithTimeout(resp.Body, 4096, time.Second)

	require.NoError(t, err)
	require.False(t, timedOut)
	require.Equal(t, responseBody, payload)
}
