package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openAIResponsesInputStatusBackfillExtraKey 按账号开启 Responses input 回放 item
// 的 status 补全。默认关闭：OpenAI 官方与 Azure 接受缺 status 的回放 item，只有
// 按规范严格校验的第三方上游（如 Zero 的 rustponsesapi、switchbase）会以
// 400 "Missing required parameter: 'input[N].status'" 拒绝，且同一会话之后每轮都会失败。
const openAIResponsesInputStatusBackfillExtraKey = "openai_responses_input_status_backfill_enabled"

// openAIResponsesInputBackfilledStatus 是回放历史 item 补上的状态。回放的都是上一轮
// 已经结束的输出，下列每种类型的 status 枚举都包含 completed。
const openAIResponsesInputBackfilledStatus = "completed"

// IsOpenAIResponsesInputStatusBackfillEnabled 只对开启开关的 OpenAI API Key 账号生效。
func (a *Account) IsOpenAIResponsesInputStatusBackfillEnabled() bool {
	if a == nil || !a.IsOpenAIApiKey() {
		return false
	}
	return a.getExtraBool(openAIResponsesInputStatusBackfillExtraKey)
}

// openAIResponsesInputItemRequiresStatus 列出 Responses 规范里 status 为必填的
// input item 类型。Codex 客户端回放 web_search_call / image_generation_call 时不带
// status，这是线上 400 的来源。
//
// 刻意不包含：
//   - message / function_call / function_call_output / reasoning 等：status 可选，
//     严格上游也接受缺省，补了反而改变请求语义。
//   - custom_tool_call(_output) / compaction / item_reference / mcp_* 等：规范里没有
//     status 字段，补上会被严格上游以 Unknown parameter 拒绝。
//   - apply_patch_call_output 等输出类：status 可能是 failed，无法安全推断。
func openAIResponsesInputItemRequiresStatus(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "web_search_call",
		"image_generation_call",
		"file_search_call",
		"computer_call",
		"code_interpreter_call",
		"local_shell_call":
		return true
	default:
		return false
	}
}

// backfillOpenAIResponsesInputItemStatus 给缺 status（或 status 为 null）的必填类型
// item 补 "completed"。已有 status 的 item、其它类型和字段原样保留。
func backfillOpenAIResponsesInputItemStatus(body []byte) ([]byte, bool, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	items := make([][]byte, 0)
	needsBackfill := make([]bool, 0)
	hasBackfill := false
	input.ForEach(func(_, item gjson.Result) bool {
		missing := false
		if item.IsObject() && openAIResponsesInputItemRequiresStatus(item.Get("type").String()) {
			status := item.Get("status")
			missing = !status.Exists() || status.Type == gjson.Null
		}
		items = append(items, []byte(item.Raw))
		needsBackfill = append(needsBackfill, missing)
		hasBackfill = hasBackfill || missing
		return true
	})
	if !hasBackfill {
		return body, false, nil
	}

	rebuiltInput := make([]byte, 0, len(input.Raw)+len(items)*len(`,"status":"completed"`))
	rebuiltInput = append(rebuiltInput, '[')
	for index, itemBody := range items {
		if needsBackfill[index] {
			var err error
			itemBody, err = sjson.SetBytes(itemBody, "status", openAIResponsesInputBackfilledStatus)
			if err != nil {
				return nil, false, fmt.Errorf("set input.%d.status: %w", index, err)
			}
		}
		if index > 0 {
			rebuiltInput = append(rebuiltInput, ',')
		}
		rebuiltInput = append(rebuiltInput, itemBody...)
	}
	rebuiltInput = append(rebuiltInput, ']')

	backfilled, err := sjson.SetRawBytes(body, "input", rebuiltInput)
	if err != nil {
		return nil, false, fmt.Errorf("replace status-backfilled input: %w", err)
	}
	return backfilled, true, nil
}
