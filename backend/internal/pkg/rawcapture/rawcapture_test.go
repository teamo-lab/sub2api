package rawcapture

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

type fakePutter struct {
	mu      sync.Mutex
	objects map[string][]byte
	block   chan struct{}
	err     error
}

func (f *fakePutter) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	data, _ := io.ReadAll(in.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[*in.Key] = data
	return &s3.PutObjectOutput{}, nil
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	raw, err := io.ReadAll(gz)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestUploadEmbedsJSONBodyAndMetadata(t *testing.T) {
	p := &fakePutter{}
	c := newWithClient(Options{Bucket: "b", Prefix: "/raw/"}, p, nil)
	started := time.Date(2026, 10, 22, 9, 5, 0, 0, time.UTC)
	ok := c.Enqueue(&Record{ClientRequestID: "abc-123", StartedAt: started, Status: 200, Model: "gpt-6-astra",
		Body: []byte(`{"model":"gpt-6-astra","input":"hi"}`), BodyBytes: 36})
	require.True(t, ok)
	c.Close(context.Background())

	data, found := p.objects["raw/abc-123.json.gz"]
	require.True(t, found, "keys: %v", p.objects)
	got := decode(t, data)
	require.Equal(t, "json", got["body_encoding"])
	require.Equal(t, "abc-123", got["client_request_id"])
	require.Equal(t, "hi", got["body"].(map[string]any)["input"])
	require.Equal(t, uint64(1), c.Stats().Uploaded)
	require.Equal(t, int64(0), c.Stats().QueuedBytes)
}

func TestNonJSONBodyIsBase64(t *testing.T) {
	p := &fakePutter{}
	c := newWithClient(Options{Bucket: "b"}, p, nil)
	require.True(t, c.Enqueue(&Record{ClientRequestID: "x", StartedAt: time.Now(), Body: []byte("not json")}))
	c.Close(context.Background())
	for _, data := range p.objects {
		got := decode(t, data)
		require.Equal(t, "base64", got["body_encoding"])
		require.NotEmpty(t, got["body_base64"])
	}
	require.Len(t, p.objects, 1)
}

func TestEnqueueDropsWhenQueueBytesExceeded(t *testing.T) {
	p := &fakePutter{block: make(chan struct{})}
	c := newWithClient(Options{Bucket: "b", Workers: 1, QueueSize: 10, MaxQueueBytes: 10}, p, nil)
	require.True(t, c.Enqueue(&Record{ClientRequestID: "a", StartedAt: time.Now(), Body: []byte("12345678")}))
	require.False(t, c.Enqueue(&Record{ClientRequestID: "b", StartedAt: time.Now(), Body: []byte("12345678")}))
	require.Equal(t, uint64(1), c.Stats().DroppedFull)
	close(p.block)
	c.Close(context.Background())
	require.Equal(t, uint64(1), c.Stats().Uploaded)
}

func TestEnqueueDropsWhenQueueFullAndNeverBlocks(t *testing.T) {
	p := &fakePutter{block: make(chan struct{})}
	c := newWithClient(Options{Bucket: "b", Workers: 1, QueueSize: 1}, p, nil)
	// 第一条被 worker 取走并卡在上传里，第二条占满队列，第三条必须立即丢弃。
	require.True(t, c.Enqueue(&Record{ClientRequestID: "1", StartedAt: time.Now()}))
	require.Eventually(t, func() bool { return len(c.queue) == 0 }, time.Second, time.Millisecond)
	require.True(t, c.Enqueue(&Record{ClientRequestID: "2", StartedAt: time.Now()}))
	done := make(chan bool)
	go func() { done <- c.Enqueue(&Record{ClientRequestID: "3", StartedAt: time.Now()}) }()
	select {
	case ok := <-done:
		require.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full queue")
	}
	close(p.block)
	c.Close(context.Background())
}

func TestUploadFailureIsCountedAndReported(t *testing.T) {
	p := &fakePutter{err: errors.New("boom")}
	var reported []string
	var mu sync.Mutex
	c := newWithClient(Options{Bucket: "b"}, p, func(rec *Record, err error) {
		mu.Lock()
		reported = append(reported, rec.ClientRequestID+":"+err.Error())
		mu.Unlock()
	})
	require.True(t, c.Enqueue(&Record{ClientRequestID: "f", StartedAt: time.Now(), Body: []byte("{}")}))
	c.Close(context.Background())
	require.Equal(t, uint64(1), c.Stats().UploadFailed)
	require.Equal(t, []string{"f:boom"}, reported)
	require.Equal(t, int64(0), c.Stats().QueuedBytes)
}

func TestEnqueueAfterCloseIsRejected(t *testing.T) {
	c := newWithClient(Options{Bucket: "b"}, &fakePutter{}, nil)
	c.Close(context.Background())
	require.False(t, c.Enqueue(&Record{ClientRequestID: "late", StartedAt: time.Now()}))
	c.Close(context.Background()) // 重复 Close 不 panic
}

func TestObjectKeySanitizesID(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("CST", 8*3600))
	key := ObjectKey("raw", &Record{ClientRequestID: "../a b/c", StartedAt: at})
	require.Equal(t, "raw/..abc.json.gz", key)
	require.False(t, strings.Contains(key, "/../"))
	require.True(t, strings.HasPrefix(ObjectKey("", &Record{StartedAt: at}), "unknown/2026-01-01/"))
}

func TestResponseEncodings(t *testing.T) {
	cases := []struct {
		name      string
		resp      []byte
		truncated bool
		encoding  string
		field     string
	}{
		{"json", []byte(`{"id":"resp_1","status":"completed"}`), false, "json", "response"},
		{"sse text", []byte("event: response.completed\ndata: {\"x\":1}\n\n"), false, "text", "response_text"},
		{"truncated json is text", []byte(`{"id":"resp_1"`), true, "text", "response_text"},
		{"binary", []byte{0x1f, 0x8b, 0xff, 0x00}, false, "base64", "response_base64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePutter{}
			c := newWithClient(Options{Bucket: "b"}, p, nil)
			require.True(t, c.Enqueue(&Record{ClientRequestID: "r", StartedAt: time.Now(), Body: []byte("{}"),
				Response: tc.resp, ResponseBytes: int64(len(tc.resp)), ResponseTruncated: tc.truncated}))
			c.Close(context.Background())
			got := decode(t, p.objects["r.json.gz"])
			require.Equal(t, tc.encoding, got["response_encoding"])
			require.NotNil(t, got[tc.field])
		})
	}
}

func TestQueueBytesCountResponse(t *testing.T) {
	p := &fakePutter{block: make(chan struct{})}
	c := newWithClient(Options{Bucket: "b", Workers: 1, MaxQueueBytes: 10}, p, nil)
	require.False(t, c.Enqueue(&Record{ClientRequestID: "big", StartedAt: time.Now(), Body: []byte("123456"), Response: []byte("123456")}))
	close(p.block)
	c.Close(context.Background())
}
