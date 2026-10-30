package service

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var benchmarkNamespaceRepairSink []byte

// legacyRepairOpenAIResponsesToolCallNamespaces 是逐项 sjson.SetBytes(body,
// "input.N.namespace") 的旧实现：每补一项都从头扫描并复制整个请求体，补的项数和
// 请求体大小相乘，长 Codex 会话里成了网关最大的 CPU 开销。只留作基准与等价对照。
func legacyRepairOpenAIResponsesToolCallNamespaces(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	topLevel, owner, ambiguous := map[string]bool{}, map[string]string{}, map[string]bool{}
	visitOpenAIResponsesToolDeclarations(body, func(tool gjson.Result) {
		name := strings.TrimSpace(tool.Get("name").String())
		switch strings.TrimSpace(tool.Get("type").String()) {
		case "function", "custom":
			if name != "" {
				topLevel[name] = true
			}
		case "namespace":
			if name == "" {
				return
			}
			tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
				childName := strings.TrimSpace(child.Get("name").String())
				if childName == "" {
					return true
				}
				if prev, ok := owner[childName]; ok && prev != name {
					ambiguous[childName] = true
				}
				owner[childName] = name
				return true
			})
		}
	})
	out := body
	var repairErr error
	index := -1
	input.ForEach(func(_, item gjson.Result) bool {
		index++
		if !item.IsObject() || !isOpenAIResponsesToolCallItemType(item.Get("type").String()) {
			return true
		}
		namespaceValue := item.Get("namespace")
		if namespaceValue.Exists() && namespaceValue.Type != gjson.Null &&
			(namespaceValue.Type != gjson.String || strings.TrimSpace(namespaceValue.String()) != "") {
			return true
		}
		name := strings.TrimSpace(item.Get("name").String())
		namespace, ok := owner[name]
		if !ok || topLevel[name] || ambiguous[name] {
			return true
		}
		out, repairErr = sjson.SetBytes(out, fmt.Sprintf("input.%d.namespace", index), namespace)
		return repairErr == nil
	})
	return out, repairErr
}

// benchmarkNamespaceRepairBody 模拟 sv-1 上 nova 的长 Codex 会话：namespace 工具
// 声明 + turns 轮「用户消息 / 丢了 namespace 的 function_call / 工具输出」。
func benchmarkNamespaceRepairBody(turns int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-6.1-sol","stream":true,"tools":[`)
	sb.WriteString(`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"},{"type":"function","name":"send_message"},{"type":"function","name":"wait_agent"}]},`)
	sb.WriteString(`{"type":"function","name":"exec_command"}],"input":[`)
	calls := []string{"spawn_agent", "send_message", "exec_command", "wait_agent"}
	userText := strings.Repeat("please continue the refactor ", 40)
	args := strings.Repeat("x", 800)
	output := strings.Repeat("line of tool output\\n", 200)
	for i := 0; i < turns; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		callID := "call_" + strconv.Itoa(i)
		sb.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + userText + `"}]},`)
		sb.WriteString(`{"type":"function_call","name":"` + calls[i%len(calls)] + `","call_id":"` + callID + `","arguments":"{\"q\":\"` + args + `\"}"},`)
		sb.WriteString(`{"type":"function_call_output","call_id":"` + callID + `","output":"` + output + `"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func TestRepairOpenAIResponsesToolCallNamespacesMatchesLegacy(t *testing.T) {
	bodies := [][]byte{
		benchmarkNamespaceRepairBody(1),
		benchmarkNamespaceRepairBody(37),
		[]byte(`{"tools":[{"type":"namespace","name":"demo","tools":[{"type":"function","name":"lookup"}]}],"input":[` +
			`{"type":"function_call","name":"lookup","namespace":null,"arguments":"{}"},` +
			`{"type":"custom_tool_call","name":"lookup","namespace":"  ","input":"x"},` +
			`"bare string item",` +
			`{"type":"function_call","name":"lookup","namespace":"kept","arguments":"{}"},` +
			`{"type":"message","role":"user","content":"lookup"},` +
			`{"type":"function_call","name":"lookup","arguments":"{\"namespace\":\"inner\"}"}]}`),
	}
	for i, body := range bodies {
		want, err := legacyRepairOpenAIResponsesToolCallNamespaces(body)
		require.NoError(t, err)
		got, err := repairOpenAIResponsesToolCallNamespaces(body)
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), "body %d", i)
	}
}

func BenchmarkRepairOpenAIResponsesToolCallNamespaces(b *testing.B) {
	for _, turns := range []int{50, 200, 400} {
		body := benchmarkNamespaceRepairBody(turns)
		for _, impl := range []struct {
			name string
			fn   func([]byte) ([]byte, error)
		}{
			{"Legacy", legacyRepairOpenAIResponsesToolCallNamespaces},
			{"Optimized", repairOpenAIResponsesToolCallNamespaces},
		} {
			b.Run(fmt.Sprintf("%s/turns=%d/%dKB", impl.name, turns, len(body)/1024), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					out, err := impl.fn(body)
					if err != nil {
						b.Fatal(err)
					}
					benchmarkNamespaceRepairSink = out
				}
			})
		}
	}
}
