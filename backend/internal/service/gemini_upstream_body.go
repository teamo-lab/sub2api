package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// Gemini 上游请求体发完就放手。
//
// http.NewRequest 用 bytes.Reader 当请求体时，Request.Body 和 GetBody 都引用整份请求体；Transport 在读完
// 响应之前一直持有这个 Request（resp.Request 也指向它），所以一条可能持续好几分钟的流式响应期间，发给
// 上游的那份请求体（通常是改写过的副本，编程 Agent 动辄 2–4MB）一直不能回收。HK 生产上每条在途 Gemini
// 流约 10–15MB，满载 500–700 条流时单个槽位 10GB 以上，蓝绿切换时两个槽位叠加就被内核 OOM 杀掉。
//
// geminiReleasingBody 读到 EOF 或被关闭后就丢掉对字节的引用。GetBody 在请求体第一次完整发出之前仍然
// 可用（Transport 在没写出任何东西时的重试照常工作）；发完之后再要重放就返回错误，由上层按普通上游失败换号。

var errGeminiUpstreamBodyReleased = errors.New("gemini upstream request body already sent and released")

type geminiReleasableBytes struct {
	mu sync.Mutex
	b  []byte
}

func (s *geminiReleasableBytes) get() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b
}

func (s *geminiReleasableBytes) release() {
	s.mu.Lock()
	s.b = nil
	s.mu.Unlock()
}

type geminiReleasingBody struct {
	mu  sync.Mutex
	src *geminiReleasableBytes
	b   []byte
}

// Read 读完最后一个字节时（请求体已经完整发出）同时丢掉共享源，之后 GetBody 不能再重放。
func (r *geminiReleasingBody) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.b) == 0 {
		r.b = nil
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	if len(r.b) == 0 {
		r.b = nil
		if r.src != nil {
			r.src.release()
		}
	}
	return n, nil
}

// Close 只丢掉本 reader 的引用，不动共享源：Transport 重试前会先关掉旧请求体再调 GetBody，
// 这时请求体还没发完，必须还能重放。
func (r *geminiReleasingBody) Close() error {
	r.mu.Lock()
	r.b = nil
	r.mu.Unlock()
	return nil
}

// newGeminiUpstreamRequest 和 http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body)) 等价
// （ContentLength、GetBody 都有），区别是请求体发完之后不再引用 body。
func newGeminiUpstreamRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		req.Body = http.NoBody
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		req.ContentLength = 0
		return req, nil
	}
	src := &geminiReleasableBytes{b: body}
	req.ContentLength = int64(len(body))
	req.Body = &geminiReleasingBody{src: src, b: body}
	req.GetBody = func() (io.ReadCloser, error) {
		b := src.get()
		if b == nil {
			return nil, errGeminiUpstreamBodyReleased
		}
		return &geminiReleasingBody{src: src, b: b}, nil
	}
	return req, nil
}
