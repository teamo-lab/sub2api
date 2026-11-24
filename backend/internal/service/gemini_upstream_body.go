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
// 请求体字节由一个共享源持有，Body 和 GetBody 生成的 reader 都从它取。调用方在 httpUpstream.Do 返回之后
// （收到响应头或者出错，Transport 已经不会再重放这个请求）调 releaseGeminiUpstreamRequestBody 放掉共享源，
// 之后流式响应期间 Transport 手里的 Request 只剩空壳。发完请求体之后、收到响应头之前的重放
// （HTTP/2 REFUSED_STREAM / GOAWAY、307/308 重定向）照常可用。

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

// Read 读完最后一个字节就丢掉本 reader 的引用；共享源要等 releaseGeminiUpstreamRequestBody。
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
	}
	return n, nil
}

// Close 只丢掉本 reader 的引用，不动共享源：Transport 重放前会先关掉旧请求体再调 GetBody。
func (r *geminiReleasingBody) Close() error {
	r.mu.Lock()
	r.b = nil
	r.mu.Unlock()
	return nil
}

// newGeminiUpstreamRequest 和 http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body)) 等价
// （ContentLength、GetBody 都有）。Do 返回后调 releaseGeminiUpstreamRequestBody，流式响应期间就不再引用 body。
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

// releaseGeminiUpstreamRequestBody 在 httpUpstream.Do 返回之后调用：放掉请求体字节，之后 GetBody 返回错误。
// 不是 newGeminiUpstreamRequest 建的请求什么也不做。
func releaseGeminiUpstreamRequestBody(req *http.Request) {
	if req == nil {
		return
	}
	if rb, ok := req.Body.(*geminiReleasingBody); ok {
		if rb.src != nil {
			rb.src.release()
		}
		_ = rb.Close()
	}
}
