package service

import (
	"bytes"
	"encoding/binary"

	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

// 缓存命中分析的中间表示（IR）。
//
// Anthropic Messages、OpenAI Chat Completions、OpenAI Responses、Gemini generateContent
// 四种请求体先各自转换成同一个 CacheRequestIR，之后的命中归因只看 IR：
//
//   - Chain[0] 是“请求头部”（system / instructions / tools 等，每轮都会原样重发、最先被缓存的部分）的哈希；
//   - Chain[i] 是头部加上前 i 条消息的累计前缀哈希。上一轮请求的 Chain 末端出现在本轮 Chain 里，
//     就说明本轮是在上一轮基础上追加的，理论上可以命中上一轮写入的上游缓存。
//
// 消息按“规范化 JSON”求哈希：去掉 cache_control 这类只影响计费标记、会在轮次之间移动的字段，
// 否则 Anthropic 把断点从上一条消息挪到最新一条时，同一段历史会被误判成内容变化。
//
// 有状态的 OpenAI Responses（previous_response_id）请求体里没有完整上下文，
// 由 ops_cache_analyzer.go 里的状态机按 response_id 接上上一轮的前缀。

type CacheProtocol string

const (
	CacheProtocolAnthropic       CacheProtocol = "anthropic"
	CacheProtocolOpenAIChat      CacheProtocol = "openai_chat"
	CacheProtocolOpenAIResponses CacheProtocol = "openai_responses"
	CacheProtocolGemini          CacheProtocol = "gemini"
)

// CacheRequestIR 是一次请求与缓存相关的全部信息；不保留任何原文，只有哈希和计数。
type CacheRequestIR struct {
	Protocol CacheProtocol
	// Chain[0] 头部哈希；Chain[i] 头部 + 前 i 条消息的累计哈希。长度 = 消息数 + 1。
	Chain []uint64
	// AssistantTurns 是请求里已有的模型输出条数（assistant / model / function_call 等）。
	// 为 0 且没有 previous_response_id 时，这是单轮请求，没有可复用的历史。
	AssistantTurns int
	// ExplicitBreakpoint：请求体里声明了 cache_control（Anthropic 需要显式断点才写缓存）。
	ExplicitBreakpoint bool
	// PreviousResponseID：OpenAI Responses 的有状态续写。
	PreviousResponseID string
	// ExplicitCachedContent：Gemini 引用了显式缓存（cachedContent）。
	ExplicitCachedContent bool
	// SessionKey 标识“同一段对话”：优先 prompt_cache_key / metadata.user_id 等显式会话键，
	// 否则用头部 + 第一条用户消息。同一会话前缀突然对不上 => 前缀漂移。
	SessionKey uint64
}

// MessageCount 返回 IR 中的消息条数（不含头部）。
func (ir *CacheRequestIR) MessageCount() int {
	if ir == nil || len(ir.Chain) == 0 {
		return 0
	}
	return len(ir.Chain) - 1
}

// Tip 返回整条请求的前缀哈希。
func (ir *CacheRequestIR) Tip() uint64 {
	if ir == nil || len(ir.Chain) == 0 {
		return 0
	}
	return ir.Chain[len(ir.Chain)-1]
}

var cacheIRIgnoredKeys = map[string]struct{}{
	"cache_control": {},
}

// BuildCacheRequestIR 按协议把请求体转成 IR；不是合法 JSON 或缺少消息时返回 false。
func BuildCacheRequestIR(protocol CacheProtocol, body []byte) (*CacheRequestIR, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, false
	}
	root := gjson.ParseBytes(body)
	ir := &CacheRequestIR{Protocol: protocol}
	head := newCacheHasher()
	var messages []gjson.Result
	explicitSession := ""

	switch protocol {
	case CacheProtocolAnthropic:
		head.field("system", root.Get("system"))
		head.field("tools", root.Get("tools"))
		head.field("tool_choice", root.Get("tool_choice"))
		messages = root.Get("messages").Array()
		explicitSession = root.Get("metadata.user_id").String()
		ir.ExplicitBreakpoint = bytes.Contains(body, []byte(`"cache_control"`))
	case CacheProtocolOpenAIChat:
		head.field("tools", root.Get("tools"))
		head.field("functions", root.Get("functions"))
		messages = root.Get("messages").Array()
		explicitSession = root.Get("prompt_cache_key").String()
	case CacheProtocolOpenAIResponses:
		head.field("instructions", root.Get("instructions"))
		head.field("tools", root.Get("tools"))
		input := root.Get("input")
		if input.Type == gjson.String {
			// 字符串 input 等价于一条 user 消息。
			messages = []gjson.Result{input}
		} else {
			messages = input.Array()
		}
		ir.PreviousResponseID = root.Get("previous_response_id").String()
		explicitSession = root.Get("prompt_cache_key").String()
		if explicitSession == "" {
			explicitSession = root.Get("conversation").String()
		}
	case CacheProtocolGemini:
		head.field("systemInstruction", cacheIRFirstExisting(root, "systemInstruction", "system_instruction"))
		head.field("tools", root.Get("tools"))
		head.field("toolConfig", cacheIRFirstExisting(root, "toolConfig", "tool_config"))
		messages = root.Get("contents").Array()
		cached := cacheIRFirstExisting(root, "cachedContent", "cached_content").String()
		ir.ExplicitCachedContent = cached != ""
		explicitSession = cached
	default:
		return nil, false
	}
	if len(messages) == 0 {
		return nil, false
	}

	ir.Chain = make([]uint64, 0, len(messages)+1)
	prev := head.sum()
	ir.Chain = append(ir.Chain, prev)
	var firstUser uint64
	for _, msg := range messages {
		msgHash := hashCanonicalJSON(msg)
		if isCacheIRModelOutput(protocol, msg) {
			ir.AssistantTurns++
		} else if firstUser == 0 {
			firstUser = msgHash
		}
		prev = mixCacheHash(prev, msgHash)
		ir.Chain = append(ir.Chain, prev)
	}
	if explicitSession != "" {
		ir.SessionKey = mixCacheHash(ir.Chain[0], xxhash.Sum64String("explicit:"+explicitSession))
	} else {
		ir.SessionKey = mixCacheHash(ir.Chain[0], firstUser)
	}
	return ir, true
}

func cacheIRFirstExisting(root gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if v := root.Get(key); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}

// isCacheIRModelOutput 判断一条消息/输入项是否是上一轮的模型输出。
func isCacheIRModelOutput(protocol CacheProtocol, msg gjson.Result) bool {
	switch protocol {
	case CacheProtocolGemini:
		return msg.Get("role").String() == "model"
	case CacheProtocolOpenAIResponses:
		switch msg.Get("type").String() {
		case "function_call", "reasoning", "custom_tool_call", "web_search_call", "file_search_call", "computer_call", "local_shell_call", "image_generation_call", "code_interpreter_call", "mcp_call":
			return true
		}
		return msg.Get("role").String() == "assistant"
	default:
		return msg.Get("role").String() == "assistant"
	}
}

// mixCacheHash 把两个 64 位哈希按顺序合成一个（链式前缀哈希用）。
func mixCacheHash(a, b uint64) uint64 {
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], a)
	binary.LittleEndian.PutUint64(buf[8:], b)
	return xxhash.Sum64(buf[:])
}

type cacheHasher struct{ d *xxhash.Digest }

func newCacheHasher() cacheHasher { return cacheHasher{d: xxhash.New()} }

func (h cacheHasher) field(name string, value gjson.Result) {
	if !value.Exists() {
		return
	}
	_, _ = h.d.WriteString(name)
	_, _ = h.d.Write([]byte{0})
	writeCanonicalJSON(h.d, value)
	_, _ = h.d.Write([]byte{0xff})
}

func (h cacheHasher) sum() uint64 { return h.d.Sum64() }

func hashCanonicalJSON(value gjson.Result) uint64 {
	d := xxhash.New()
	writeCanonicalJSON(d, value)
	return d.Sum64()
}

// writeCanonicalJSON 递归写入去掉忽略字段后的 JSON。对象保留原始键顺序（同一客户端每轮一致），
// 标量直接用原始文本，不做反转义，避免为大段文本/图片 base64 额外分配内存。
func writeCanonicalJSON(d *xxhash.Digest, value gjson.Result) {
	switch {
	case value.IsObject():
		_, _ = d.Write([]byte{'{'})
		value.ForEach(func(key, val gjson.Result) bool {
			if _, skip := cacheIRIgnoredKeys[key.Str]; skip {
				return true
			}
			_, _ = d.WriteString(key.Str)
			_, _ = d.Write([]byte{':'})
			writeCanonicalJSON(d, val)
			_, _ = d.Write([]byte{','})
			return true
		})
		_, _ = d.Write([]byte{'}'})
	case value.IsArray():
		_, _ = d.Write([]byte{'['})
		value.ForEach(func(_, val gjson.Result) bool {
			writeCanonicalJSON(d, val)
			_, _ = d.Write([]byte{','})
			return true
		})
		_, _ = d.Write([]byte{']'})
	default:
		_, _ = d.WriteString(value.Raw)
	}
}
