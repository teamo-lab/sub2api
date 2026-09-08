package relay

import (
	"errors"
	"testing"
)

func TestChatObserverToolBoundary(t *testing.T) {
	var o ChatObserver
	for _, payload := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`,
	} {
		if err := o.Observe(payload); err != nil {
			t.Fatal(err)
		}
		if o.Ready || o.Complete() {
			t.Fatal("partial tool or usage committed")
		}
	}
	if err := o.Observe(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`); err != nil {
		t.Fatal(err)
	}
	if !o.Ready || !o.Complete() {
		t.Fatal("finished tool withheld")
	}
}

func TestChatObserverDoesNotConfuseUsageAndSuccess(t *testing.T) {
	var o ChatObserver
	_ = o.Observe(`{"choices":[{"delta":{"content":"hello"}}],"usage":{"completion_tokens":1}}`)
	if !o.Ready || o.Complete() {
		t.Fatal("running usage is not a terminal")
	}
	_ = o.Observe(`{"error":{"code":"server_error","message":"temporarily unavailable"}}`)
	if o.Complete() || len(o.Failure) == 0 {
		t.Fatal("failure lost")
	}
	if o.Observe("[DONE]") == nil || o.Complete() {
		t.Fatal("later terminal erased failure")
	}
}

func TestChatObserverRejectsDoneWithPartialTool(t *testing.T) {
	var o ChatObserver
	_ = o.Observe(`{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`)
	if err := o.Observe("[DONE]"); !errors.Is(err, ErrIncompleteTool) {
		t.Fatal(err)
	}
}
