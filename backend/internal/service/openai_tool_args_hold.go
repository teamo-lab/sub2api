package service

import (
	"strings"
	"time"
)

// 工具参数暂存（c8ed743f5）在首个客户端输出之前把 function_call / custom tool
// 的参数增量扣住，直到携带完整参数的 output_item.done 才放行，好让参数中途
// 失败时还能换号。
//
// 但暂存时长等于参数生成时长：整文件写入这类上千 token 的参数要扣几十秒，
// 下游在此期间一个语义字节都收不到，带 60s 流式超时的客户端会断开并重发
// （每次重发都被完整计费）。线上 2 天 109 万次 OpenAI 请求里，流内失败后换号
// 救回的只有 333 次（含推理阶段失败），参数中途失败只是其中一部分；而大参数
// 工具调用几乎全部被扣到 60s 以上。
//
// 所以暂存只保护「短参数」：从第一帧参数被扣开始，超过时长或字节上限就放弃
// 保护、按原顺序放行并改为边生成边下发。短参数（绝大多数工具调用 1~2s 写完）
// 仍然完整受保护；超长参数放行后中途失败不再换号，与暂存之前的行为一致。
const (
	openAIToolArgsHoldMaxDuration = 10 * time.Second
	openAIToolArgsHoldMaxBytes    = 64 << 10
)

// openAIStreamEventIsToolArgs reports whether a Responses SSE event carries
// tool-call argument bytes that the pre-output stage holds back.
func openAIStreamEventIsToolArgs(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done":
		return true
	}
	return false
}

// openAIToolArgsHold bounds how long held tool-argument frames may delay the
// first client output. It is only consulted before client output started.
type openAIToolArgsHold struct {
	now         func() time.Time
	maxDuration time.Duration
	maxBytes    int
	startedAt   time.Time
	bytes       int
}

func newOpenAIToolArgsHold() *openAIToolArgsHold {
	return &openAIToolArgsHold{
		now:         time.Now,
		maxDuration: openAIToolArgsHoldMaxDuration,
		maxBytes:    openAIToolArgsHoldMaxBytes,
	}
}

// Exceeded records a held tool-argument frame of frameBytes and reports
// whether the hold is over its budget, in which case the caller must release
// the buffered frames (and this one) and stream the rest. Frames that are not
// tool arguments neither start nor extend the hold.
func (h *openAIToolArgsHold) Exceeded(eventType string, frameBytes int) bool {
	if h == nil || !openAIStreamEventIsToolArgs(eventType) {
		return false
	}
	now := h.now()
	if h.startedAt.IsZero() {
		h.startedAt = now
	}
	h.bytes += frameBytes
	return now.Sub(h.startedAt) >= h.maxDuration || h.bytes > h.maxBytes
}
