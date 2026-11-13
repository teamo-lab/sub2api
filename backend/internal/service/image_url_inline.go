package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// 图片 URL 默认直接交给上游去拉，省掉我们这边的下载和出网流量（11-13 实测：Gemini 各中转/Vertex
// 都认 fileData 里的 http 链接，Claude 中转认 url 图片源，OpenAI 认 image_url）。只有下面几种情况
// 才由我们把 Claude 格式里的 {"type":"image","source":{"type":"url",...}} 下载成 base64：
//   - Bedrock、Vertex 上的 Claude：不支持 url 图片源；
//   - Antigravity：走 Cloud Code 内部接口，未验证能拉外链；
//   - Gemini 上游回 400 "Cannot fetch content from the provided URL"（Google 抓取被 robots 或站点拦截）
//     时，下载后重试一次。
//
// 下载失败时返回 *ImageURLInlineError，调用方回 400，不再悄悄丢图。
const (
	imageURLInlineMaxBytes      = 20 << 20
	imageURLInlineMaxTotalBytes = 50 << 20
	imageURLInlineMaxCount      = 32
	imageURLInlineTimeout       = 30 * time.Second
	imageURLInlineConcurrency   = 4
	imageURLInlineMaxRedirects  = 5
	imageURLInlineCacheKey      = "sub2api_image_url_inline_cache"
	imageURLInlineUserAgent     = "Mozilla/5.0 (compatible; Sub2API-ImageFetcher/1.0)"
)

// ImageURLInlineError 表示请求里的图片 URL 无法下载或不是图片，属于客户端请求错误。
type ImageURLInlineError struct {
	URL    string
	Reason string
}

func (e *ImageURLInlineError) Error() string {
	return fmt.Sprintf("failed to fetch image url %s: %s", redactImageURL(e.URL), e.Reason)
}

// IsImageURLInlineError 报告 err 是否为图片 URL 下载失败。
func IsImageURLInlineError(err error) bool {
	var target *ImageURLInlineError
	return errors.As(err, &target)
}

type inlinedImage struct {
	mediaType string
	data      string
	size      int
}

type imageURLInlineCache struct {
	mu     sync.Mutex
	images map[string]*inlinedImage
}

// imageURLInlineIPAllowed 决定下载图片时允许连接的 IP；测试里替换以访问 httptest 的回环地址。
var imageURLInlineIPAllowed = isPublicImageFetchIP

var cgnatPrefix = mustParseCIDR("100.64.0.0/10")

func mustParseCIDR(raw string) *net.IPNet {
	_, n, err := net.ParseCIDR(raw)
	if err != nil {
		panic(err)
	}
	return n
}

func isPublicImageFetchIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || cgnatPrefix.Contains(v4) {
			return false
		}
	}
	return true
}

var (
	imageURLInlineClientOnce sync.Once
	imageURLInlineClient     *http.Client
)

// getImageURLInlineClient 返回下载图片用的 client：不走代理，在拨号时按实际连接的 IP 拦截
// 内网/回环/链路本地地址（覆盖重定向和 DNS 重绑定），重定向只允许 http/https 且最多 5 跳。
func getImageURLInlineClient() *http.Client {
	imageURLInlineClientOnce.Do(func() {
		dialer := &net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if !imageURLInlineIPAllowed(net.ParseIP(host)) {
					return fmt.Errorf("destination %s is not allowed", host)
				}
				return nil
			},
		}
		transport := &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
		}
		imageURLInlineClient = &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= imageURLInlineMaxRedirects {
					return fmt.Errorf("too many redirects")
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
				}
				return nil
			},
		}
	})
	return imageURLInlineClient
}

type claudeImageURLRef struct {
	sourcePath string
	url        string
}

// collectClaudeImageURLRefs 找出 messages[].content[] 以及 tool_result.content[] 里
// source.type == "url" 的图片块。
func collectClaudeImageURLRefs(body []byte) []claudeImageURLRef {
	var refs []claudeImageURLRef
	check := func(path string, block gjson.Result) {
		if block.Get("type").String() != "image" {
			return
		}
		source := block.Get("source")
		if source.Get("type").String() != "url" {
			return
		}
		refs = append(refs, claudeImageURLRef{sourcePath: path + ".source", url: strings.TrimSpace(source.Get("url").String())})
	}
	gjson.GetBytes(body, "messages").ForEach(func(mi, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(bi, block gjson.Result) bool {
			blockPath := "messages." + mi.String() + ".content." + bi.String()
			check(blockPath, block)
			if block.Get("type").String() == "tool_result" {
				inner := block.Get("content")
				if inner.IsArray() {
					inner.ForEach(func(ii, innerBlock gjson.Result) bool {
						check(blockPath+".content."+ii.String(), innerBlock)
						return true
					})
				}
			}
			return true
		})
		return true
	})
	return refs
}

// inlineClaudeImageURLs 把 Claude 格式请求体里的 url 图片下载并替换成 base64 图片。
// 没有 url 图片时原样返回 body；下载结果按 URL 缓存在 gin.Context 上，换号重试不会重复下载。
func inlineClaudeImageURLs(ctx context.Context, c *gin.Context, body []byte) ([]byte, error) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"url"`)) || !gjson.ValidBytes(body) {
		return body, nil
	}
	refs := collectClaudeImageURLRefs(body)
	if len(refs) == 0 {
		return body, nil
	}

	unique := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, ok := seen[ref.url]; ok {
			continue
		}
		seen[ref.url] = struct{}{}
		unique = append(unique, ref.url)
	}
	if len(unique) > imageURLInlineMaxCount {
		return nil, &ImageURLInlineError{URL: unique[imageURLInlineMaxCount], Reason: fmt.Sprintf("too many image urls (max %d)", imageURLInlineMaxCount)}
	}

	cache := imageURLInlineCacheFromContext(c)
	start := time.Now()
	images, fetched, err := fetchImageURLs(ctx, cache, unique)
	if err != nil {
		logger.L().Warn("image_url_inline.fetch_failed", zap.Int("urls", len(unique)), zap.Error(err))
		return nil, err
	}

	total := 0
	for _, img := range images {
		total += img.size
	}
	if total > imageURLInlineMaxTotalBytes {
		return nil, &ImageURLInlineError{URL: unique[0], Reason: fmt.Sprintf("images total %d bytes exceed %d", total, imageURLInlineMaxTotalBytes)}
	}

	out := body
	for _, ref := range refs {
		img := images[ref.url]
		source, _ := json.Marshal(map[string]string{"type": "base64", "media_type": img.mediaType, "data": img.data})
		next, setErr := sjson.SetRawBytes(out, ref.sourcePath, source)
		if setErr != nil {
			return nil, fmt.Errorf("inline image url: %w", setErr)
		}
		out = next
	}
	logger.L().Info("image_url_inline.inlined",
		zap.Int("blocks", len(refs)),
		zap.Int("urls", len(unique)),
		zap.Int("fetched", fetched),
		zap.Int("bytes", total),
		zap.Duration("elapsed", time.Since(start)),
	)
	return out, nil
}

func imageURLInlineCacheFromContext(c *gin.Context) *imageURLInlineCache {
	if c == nil {
		return &imageURLInlineCache{images: map[string]*inlinedImage{}}
	}
	if v, ok := c.Get(imageURLInlineCacheKey); ok {
		if cache, ok := v.(*imageURLInlineCache); ok {
			return cache
		}
	}
	cache := &imageURLInlineCache{images: map[string]*inlinedImage{}}
	c.Set(imageURLInlineCacheKey, cache)
	return cache
}

func fetchImageURLs(ctx context.Context, cache *imageURLInlineCache, urls []string) (map[string]*inlinedImage, int, error) {
	result := make(map[string]*inlinedImage, len(urls))
	var pending []string
	cache.mu.Lock()
	for _, u := range urls {
		if img, ok := cache.images[u]; ok {
			result[u] = img
		} else {
			pending = append(pending, u)
		}
	}
	cache.mu.Unlock()
	if len(pending) == 0 {
		return result, 0, nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, imageURLInlineTimeout)
	defer cancel()

	type fetchResult struct {
		url string
		img *inlinedImage
		err error
	}
	sem := make(chan struct{}, imageURLInlineConcurrency)
	results := make(chan fetchResult, len(pending))
	for _, u := range pending {
		go func(u string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			img, err := fetchImageURL(fetchCtx, u)
			results <- fetchResult{url: u, img: img, err: err}
		}(u)
	}
	var firstErr error
	for range pending {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				cancel()
			}
			continue
		}
		result[r.url] = r.img
	}
	if firstErr != nil {
		return nil, 0, firstErr
	}
	cache.mu.Lock()
	for _, u := range pending {
		cache.images[u] = result[u]
	}
	cache.mu.Unlock()
	return result, len(pending), nil
}

func fetchImageURL(ctx context.Context, raw string) (*inlinedImage, error) {
	fail := func(reason string) (*inlinedImage, error) {
		return nil, &ImageURLInlineError{URL: raw, Reason: reason}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fail("only http(s) image urls are supported")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return fail("invalid url")
	}
	req.Header.Set("User-Agent", imageURLInlineUserAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.8,*/*;q=0.5")

	resp, err := getImageURLInlineClient().Do(req) //nolint:gosec // G107: 拨号时按实际 IP 拦截内网地址，见 getImageURLInlineClient
	if err != nil {
		reason := "download failed"
		if ctx.Err() != nil {
			reason = "download timed out or canceled"
		} else if strings.Contains(err.Error(), "is not allowed") {
			reason = "destination address is not allowed"
		} else if strings.Contains(err.Error(), "redirect") {
			reason = "invalid redirect"
		}
		return fail(reason)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fail("upstream returned status " + strconv.Itoa(resp.StatusCode))
	}
	if resp.ContentLength > imageURLInlineMaxBytes {
		return fail(fmt.Sprintf("image larger than %d bytes", imageURLInlineMaxBytes))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imageURLInlineMaxBytes+1))
	if err != nil {
		return fail("read body failed")
	}
	if len(data) > imageURLInlineMaxBytes {
		return fail(fmt.Sprintf("image larger than %d bytes", imageURLInlineMaxBytes))
	}
	if len(data) == 0 {
		return fail("empty body")
	}

	mediaType := ""
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mt, _, parseErr := mime.ParseMediaType(ct); parseErr == nil {
			mediaType = strings.ToLower(mt)
		}
	}
	if !strings.HasPrefix(mediaType, "image/") {
		sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(data))
		if !strings.HasPrefix(sniffed, "image/") {
			if mediaType == "" {
				mediaType = sniffed
			}
			return fail("content is not an image (" + mediaType + ")")
		}
		mediaType = sniffed
	}
	return &inlinedImage{mediaType: mediaType, data: base64.StdEncoding.EncodeToString(data), size: len(data)}, nil
}

// redactImageURL 去掉 query 和 fragment（可能带签名），避免把凭据写进错误信息和日志。
func redactImageURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		if len(raw) > 64 {
			return raw[:64] + "..."
		}
		return raw
	}
	out := parsed.Scheme + "://" + parsed.Host + parsed.Path
	if len(out) > 200 {
		out = out[:200] + "..."
	}
	return out
}

// accountRequiresInlineImageURLs 报告该账号的上游是否不支持 url 图片源，需要先下载成 base64。
func accountRequiresInlineImageURLs(account *Account) bool {
	if account == nil {
		return false
	}
	return account.IsBedrock() || (account.Platform == PlatformAnthropic && account.Type == AccountTypeServiceAccount)
}

// claudeBodyHasImageURL 报告 Claude 格式请求体里是否有 url 图片源。
func claudeBodyHasImageURL(body []byte) bool {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"url"`)) || !gjson.ValidBytes(body) {
		return false
	}
	return len(collectClaudeImageURLRefs(body)) > 0
}

// isGeminiImageURLFetchError 识别 Gemini 上游拉不到 fileData 链接时的 400，例如
// "Cannot fetch content from the provided URL ... Status: URL_ERROR-ERROR_OTHER" 或
// "URL_UNREACHABLE-UNREACHABLE_ROBOTS_ERROR"。
func isGeminiImageURLFetchError(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "cannot fetch content") ||
		strings.Contains(lower, "url_unreachable") ||
		strings.Contains(lower, "url_error")
}

// guessImageMIMEFromURL 按扩展名猜图片类型，猜不到用 image/jpeg。Gemini 的 fileData 必须带
// mimeType（Vertex 不填会 400），但类型填错不影响识别（实测 webp 填 image/jpeg、image/png 都正常）。
func guessImageMIMEFromURL(raw string) string {
	path := raw
	if parsed, err := url.Parse(raw); err == nil {
		path = parsed.Path
	}
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".heic"):
		return "image/heic"
	case strings.HasSuffix(lower, ".heif"):
		return "image/heif"
	default:
		return "image/jpeg"
	}
}
