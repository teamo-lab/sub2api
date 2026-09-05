package service

import (
	"encoding/json"
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// rewriteGatewayMessageContentRaw only rebuilds arrays that changed. Kept
// blocks/messages are copied as raw JSON, so opaque thinking fields, unknown
// fields and large tool-argument integers never pass through map[string]any.
func rewriteGatewayMessageContentRaw(body []byte, rewrite func(string, gjson.Result) ([]byte, bool, error)) []byte {
	if !json.Valid(body) {
		return body
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	var rawMessages []string
	modified, failed := false, false
	messages.ForEach(func(_, message gjson.Result) bool {
		raw := message.Raw
		if content := message.Get("content"); message.IsObject() && content.IsArray() {
			out, changed, err := rewrite(message.Get("role").Str, content)
			if err != nil {
				failed = true
				return false
			}
			if changed {
				updated, err := sjson.SetRawBytes([]byte(raw), "content", out)
				if err != nil {
					failed = true
					return false
				}
				raw, modified = string(updated), true
			}
		}
		rawMessages = append(rawMessages, raw)
		return true
	})
	if failed || !modified {
		return body
	}
	out, err := sjson.SetRawBytes(body, "messages", joinGatewayRawArray(rawMessages))
	if err != nil {
		return body
	}
	return out
}

// Rebuilding each changed array once avoids repeated whole-body path deletions
// for histories containing many empty blocks. Only tool_result.content is
// recursively inspected; arbitrary tool arguments remain opaque.
func filterGatewayContentRaw(content gjson.Result, keep func(gjson.Result) bool, recurseToolResults bool) ([]byte, bool, error) {
	return filterGatewayContentRawDepth(content, keep, recurseToolResults, 0)
}

func filterGatewayContentRawDepth(content gjson.Result, keep func(gjson.Result) bool, recurseToolResults bool, depth int) ([]byte, bool, error) {
	// Pathological nested tool_result arrays must not turn a cheap pre-filter
	// into unbounded repeated parsing. Fail safely with the original body.
	if depth > 64 {
		return nil, false, errors.New("gateway content nesting exceeds rewrite limit")
	}
	var blocks []string
	modified := false
	var rewriteErr error
	content.ForEach(func(_, block gjson.Result) bool {
		if !keep(block) {
			modified = true
			return true
		}
		raw := block.Raw
		if recurseToolResults && block.Get("type").Str == "tool_result" {
			if nested := block.Get("content"); nested.IsArray() {
				out, changed, err := filterGatewayContentRawDepth(nested, keep, true, depth+1)
				if err != nil {
					rewriteErr = err
					return false
				}
				if changed {
					updated, err := sjson.SetRawBytes([]byte(raw), "content", out)
					if err != nil {
						rewriteErr = err
						return false
					}
					raw, modified = string(updated), true
				}
			}
		}
		blocks = append(blocks, raw)
		return true
	})
	if rewriteErr != nil || !modified {
		return nil, false, rewriteErr
	}
	return joinGatewayRawArray(blocks), true, nil
}

func joinGatewayRawArray(values []string) []byte {
	size := 2
	for _, value := range values {
		size += len(value) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, value := range values {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, value...)
	}
	return append(out, ']')
}
