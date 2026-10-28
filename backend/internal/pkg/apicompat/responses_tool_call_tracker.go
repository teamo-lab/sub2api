package apicompat

import "strings"

// responsesToolCallRef 记录 Responses 流里的一个工具调用（function_call /
// custom_tool_call）以及已经转发出去的参数。
type responsesToolCallRef struct {
	index  int // 在下游 tool_calls（或缓冲累加器）里的序号
	itemID string
	callID string
	idSent bool // 下游是否已经收到 call id
	args   strings.Builder
	done   bool // 已收到 *.done / output_item.done，之后的增量一律丢弃
}

// missingArguments 返回完整参数里还没转发的那段。已转发部分必须是完整参数的前缀，
// 否则无法在不重复的前提下补齐，返回空串。
func (r *responsesToolCallRef) missingArguments(final string) string {
	return missingToolArguments(r.args.String(), final)
}

func missingToolArguments(sent, final string) string {
	if final == "" || len(final) <= len(sent) || !strings.HasPrefix(final, sent) {
		return ""
	}
	return final[len(sent):]
}

// responsesToolCallTracker 把参数事件对应到工具调用。
//
// 官方上游按 output_index 对齐 output_item.added 和参数事件；部分中转上游只在
// function_call_arguments.done / output_item.done 里给完整参数，或者参数事件的
// output_index 缺失、与 added 对不上。这里依次按 item_id、call_id、output_index
// 查找，都找不到时，如果只有一个未结束的调用就归给它。
type responsesToolCallTracker struct {
	calls     []*responsesToolCallRef
	byOutput  map[int]*responsesToolCallRef
	ambiguous map[int]bool // 多个调用落在同一个 output_index（上游没给），不能按它匹配
	byItemID  map[string]*responsesToolCallRef
	byCallID  map[string]*responsesToolCallRef
}

func (t *responsesToolCallTracker) register(outputIndex int, item *ResponsesOutput) *responsesToolCallRef {
	if t.byOutput == nil {
		t.byOutput = make(map[int]*responsesToolCallRef)
		t.ambiguous = make(map[int]bool)
		t.byItemID = make(map[string]*responsesToolCallRef)
		t.byCallID = make(map[string]*responsesToolCallRef)
	}
	ref := &responsesToolCallRef{index: len(t.calls), itemID: item.ID, callID: item.CallID}
	t.calls = append(t.calls, ref)
	if t.byOutput[outputIndex] != nil || t.ambiguous[outputIndex] {
		delete(t.byOutput, outputIndex)
		t.ambiguous[outputIndex] = true
	} else {
		t.byOutput[outputIndex] = ref
	}
	if item.ID != "" {
		t.byItemID[item.ID] = ref
	}
	if item.CallID != "" {
		t.byCallID[item.CallID] = ref
	}
	return ref
}

func (t *responsesToolCallTracker) resolve(outputIndex int, itemID, callID string) *responsesToolCallRef {
	if itemID != "" {
		if ref := t.byItemID[itemID]; ref != nil {
			return ref
		}
	}
	if callID != "" {
		if ref := t.byCallID[callID]; ref != nil {
			return ref
		}
	}
	// 事件带了 call_id 却和候选调用的 call_id 不同，说明不是同一个调用。
	conflicts := func(ref *responsesToolCallRef) bool {
		return callID != "" && ref.callID != "" && ref.callID != callID
	}
	if ref := t.byOutput[outputIndex]; ref != nil && !conflicts(ref) {
		return ref
	}
	var open *responsesToolCallRef
	for _, ref := range t.calls {
		if ref.done {
			continue
		}
		if open != nil {
			return nil
		}
		open = ref
	}
	if open != nil && conflicts(open) {
		return nil
	}
	return open
}

func isResponsesToolCallItem(item *ResponsesOutput) bool {
	return item != nil && (item.Type == "function_call" || item.Type == "custom_tool_call")
}

// responsesToolItemArguments 返回工具调用项携带的完整参数（custom_tool_call 为 input）。
func responsesToolItemArguments(item *ResponsesOutput) string {
	if item.Type == "custom_tool_call" {
		return item.Input
	}
	return item.Arguments
}
