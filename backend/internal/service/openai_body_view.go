package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// gjsonBodyGet 是不复制结果的 gjson.GetBytes：返回的 Raw/Str 直接指向 body 的内存。
// gjson.GetBytes 会把结果整段复制成新字符串，对 input 这类占请求体主体的字段，每调用一次就多分配
// 一份请求体（10–15MB 的请求体一个请求要查好几次）。
//
// 只用于只读判断：调用方必须保证 body 在结果存活期间不被改写，结果里的字符串也不能存进比本次调用
// 活得更久的地方（否则会把整个 body 钉在内存里）。见 openAIWSPayloadStringView。
func gjsonBodyGet(body []byte, path string) gjson.Result {
	if len(body) == 0 {
		return gjson.Result{}
	}
	return gjson.Get(openAIWSPayloadStringView(body), path)
}

// forEachOpenAIInputItem 不复制地遍历顶层 input 数组的每一项；input 不是数组时什么也不做。
// fn 返回 false 时停止。约束同 gjsonBodyGet。
func forEachOpenAIInputItem(body []byte, fn func(item gjson.Result) bool) {
	input := gjsonBodyGet(body, "input")
	if !input.IsArray() {
		return
	}
	input.ForEach(func(_, item gjson.Result) bool { return fn(item) })
}

// openAIInputItemFields 是一个 input 项里用来判断要不要改写的几个字段，按完整解码（encoding/json）的
// 口径取值：字符串去掉首尾空白，非字符串当空串，重复的键以最后一个为准。
type openAIInputItemFields struct {
	typ       string
	id        string
	hasCallID bool
}

// readOpenAIInputItemFields 一次遍历 item 的键取出这些字段；对带大段 base64 的项，比分别 Get 少扫几遍。
func readOpenAIInputItemFields(item gjson.Result) openAIInputItemFields {
	var fields openAIInputItemFields
	item.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "type":
			fields.typ = openAIJSONTrimmedString(value)
		case "id":
			fields.id = openAIJSONTrimmedString(value)
		case "call_id":
			fields.hasCallID = true
		}
		return true
	})
	return fields
}

func openAIJSONTrimmedString(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.Str)
}
