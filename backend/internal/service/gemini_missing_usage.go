package service

import (
	"bytes"
	"io"
	"log/slog"

	"github.com/tidwall/gjson"
)

// 有些 Gemini 中转（例如 AnyRoute）的流式回包里完全没有 usageMetadata：每个 chunk 只有
// candidates/model，最后一个 chunk 带 finishReason 也不带用量。这种请求 usage_logs 里 token
// 全是 0，标准价、实际扣费和账号成本都记成 0，而上游（router）按自己的估算照常计费，
// 看板和对账两边对不上。
//
// geminiUsageSniffer 包住上游响应体，在不改变任何转发字节的前提下旁路观察 SSE：
// 记录是否出现过有效的 usageMetadata，并累计生成内容（文本按累计/增量两种写法去重，
// functionCall 按参数 JSON 计）。流结束后，如果整条流都没有用量、但确实生成了内容，
// 调用方用 estimate 按请求体和生成内容估算一份用量。上游给了用量时完全不介入。

const geminiUsageSnifferMaxLine = 8 << 20

type geminiUsageSniffer struct {
	rc           io.ReadCloser
	isOAuth      bool
	pending      []byte
	overflow     bool
	sawUsage     bool
	sawCandidate bool
	text         string
	toolChars    int
}

func newGeminiUsageSniffer(rc io.ReadCloser, isOAuth bool) *geminiUsageSniffer {
	return &geminiUsageSniffer{rc: rc, isOAuth: isOAuth}
}

func (s *geminiUsageSniffer) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.feed(p[:n])
	}
	if err == io.EOF && len(s.pending) > 0 {
		s.observeLine(s.pending)
		s.pending = nil
	}
	return n, err
}

func (s *geminiUsageSniffer) Close() error { return s.rc.Close() }

func (s *geminiUsageSniffer) feed(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if s.overflow || len(s.pending)+len(b) > geminiUsageSnifferMaxLine {
				s.overflow = true
				s.pending = nil
				return
			}
			s.pending = append(s.pending, b...)
			return
		}
		if s.overflow {
			// 超长行整行丢弃，从下一行重新开始观察。
			s.overflow = false
			s.pending = nil
		} else if len(s.pending) > 0 {
			line := append(s.pending, b[:i]...)
			s.pending = nil
			s.observeLine(line)
		} else {
			s.observeLine(b[:i])
		}
		b = b[i+1:]
	}
}

func (s *geminiUsageSniffer) observeLine(line []byte) {
	trimmed := bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return
	}
	raw := payload
	if s.isOAuth {
		if inner, err := unwrapGeminiResponse(payload); err == nil {
			raw = inner
		}
	}
	if !gjson.ValidBytes(raw) {
		return
	}
	if u := gjson.GetBytes(raw, "usageMetadata"); u.Exists() {
		if u.Get("promptTokenCount").Int() > 0 || u.Get("candidatesTokenCount").Int() > 0 || u.Get("totalTokenCount").Int() > 0 {
			s.sawUsage = true
		}
	}
	candidates := gjson.GetBytes(raw, "candidates")
	if !candidates.Exists() {
		return
	}
	s.sawCandidate = true
	candidates.Get("0.content.parts").ForEach(func(_, part gjson.Result) bool {
		if t := part.Get("text"); t.Exists() && t.String() != "" {
			_, s.text = computeGeminiTextDelta(s.text, t.String())
		}
		if fc := part.Get("functionCall"); fc.Exists() {
			s.toolChars += len(fc.Get("name").String()) + len(fc.Get("args").Raw)
		}
		return true
	})
}

// estimate 在整条流没有任何有效 usageMetadata、但生成了内容时返回估算用量；否则返回 nil。
func (s *geminiUsageSniffer) estimate(reqBody []byte) *ClaudeUsage {
	if s == nil || s.sawUsage || !s.sawCandidate {
		return nil
	}
	out := estimateTokensForText(s.text) + (s.toolChars+3)/4
	if out == 0 {
		return nil
	}
	return &ClaudeUsage{InputTokens: estimateGeminiRequestTokens(reqBody), OutputTokens: out}
}

// estimateGeminiRequestTokens 估算 generateContent 请求体的输入 token：在 estimateGeminiCountTokens
// （system + contents 文本）的基础上，再算上 functionCall 参数、functionResponse 结果和工具声明，
// agent 类请求的大头往往在这几处。
func estimateGeminiRequestTokens(reqBody []byte) int {
	total := estimateGeminiCountTokens(reqBody)
	structured := 0
	gjson.GetBytes(reqBody, "contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if fc := part.Get("functionCall"); fc.Exists() {
				structured += len(fc.Raw)
			}
			if fr := part.Get("functionResponse"); fr.Exists() {
				structured += len(fr.Raw)
			}
			return true
		})
		return true
	})
	if tools := gjson.GetBytes(reqBody, "tools"); tools.Exists() {
		structured += len(tools.Raw)
	}
	return total + (structured+3)/4
}

func geminiUsageIsEmpty(u *ClaudeUsage) bool {
	return u == nil || (u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0)
}

// applyGeminiMissingUsageEstimate 只在解析出的用量为空、且旁路观察确认上游没给用量时替换成估算值。
func applyGeminiMissingUsageEstimate(account *Account, usage *ClaudeUsage, sniffer *geminiUsageSniffer, reqBody []byte, path string) *ClaudeUsage {
	if !geminiUsageIsEmpty(usage) {
		return usage
	}
	est := sniffer.estimate(reqBody)
	if est == nil {
		return usage
	}
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	slog.Info("gemini_usage_estimated",
		"account_id", accountID,
		"path", path,
		"input_tokens", est.InputTokens,
		"output_tokens", est.OutputTokens)
	return est
}
