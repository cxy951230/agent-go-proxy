package main

import (
	"encoding/json"
	"testing"
)

func TestClientWantsSSE(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"absent means non-stream (SDK default)", `{"model":"gpt-5.6-sol","input":[]}`, false},
		{"explicit true", `{"stream":true}`, true},
		{"explicit false", `{"stream":false}`, false},
		{"invalid body", `not-json`, false},
	}
	for _, c := range cases {
		if got := clientWantsSSE([]byte(c.body)); got != c.want {
			t.Errorf("%s: clientWantsSSE=%v want %v", c.name, got, c.want)
		}
	}
}

func TestAggregateResponsesSSEToJSON(t *testing.T) {
	sse := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\"}]}}\n\n"

	body, ok := aggregateResponsesSSEToJSON(sse)
	if !ok {
		t.Fatal("expected aggregation to succeed")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("aggregated body is not valid JSON: %v", err)
	}
	if obj["id"] != "resp_1" || obj["status"] != "completed" {
		t.Fatalf("unexpected aggregated response: %s", body)
	}
}

// Codex 后端 response.completed 里 output 为空,正文在 output_item.done 事件里。
func TestAggregateRebuildsOutputFromItemDone(t *testing.T) {
	sse := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"你好\"}]}}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"

	body, ok := aggregateResponsesSSEToJSON(sse)
	if !ok {
		t.Fatal("expected aggregation to succeed")
	}
	var obj struct {
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// 按 output_index 排序:reasoning(0) 在前,message(1) 在后
	if len(obj.Output) != 2 || obj.Output[0].Type != "reasoning" || obj.Output[1].Type != "message" {
		t.Fatalf("output not rebuilt correctly: %s", body)
	}
}

func TestAggregateResponsesSSEToJSONNoCompleted(t *testing.T) {
	sse := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	if _, ok := aggregateResponsesSSEToJSON(sse); ok {
		t.Fatal("expected failure when there is no response.completed event")
	}
}
