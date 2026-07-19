package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesCustomExecToolSchemaAndRestore(t *testing.T) {
	req := []byte(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run echo"}]},
			{"type":"additional_tools","tools":[
				{"type":"custom","name":"exec","description":"Run JavaScript code to orchestrate/compose tool calls","format":{"type":"grammar"}}
			]}
		]
	}`)

	chatBody, state, err := openaiResponsesToChat(req)
	if err != nil {
		t.Fatalf("openaiResponsesToChat() error = %v", err)
	}

	var chat struct {
		Tools []struct {
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatalf("chat body should be JSON: %v", err)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(chat.Tools))
	}
	fn := chat.Tools[0].Function
	if fn.Name != "exec" {
		t.Fatalf("tool name = %q, want exec", fn.Name)
	}
	if !strings.Contains(fn.Description, "await tools.exec_command") {
		t.Fatalf("exec description should teach JS orchestration, got: %s", fn.Description)
	}
	required, _ := fn.Parameters["required"].([]any)
	if len(required) != 1 || required[0] != "input" {
		t.Fatalf("custom exec should require input, got %#v", fn.Parameters["required"])
	}

	chatResp := `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{\"cmd\":\"await tools.exec_command({cmd: \\\"echo hello\\\"})\"}"}}]},"finish_reason":"tool_calls"}]}`
	native := adaptChatResponseToNative("responses", chatResp, true, "gpt-5.6-sol", state)
	if !strings.Contains(native, `"type":"custom_tool_call"`) {
		t.Fatalf("native response should contain custom_tool_call: %s", native)
	}
	if !strings.Contains(native, `"name":"exec"`) {
		t.Fatalf("native response should preserve exec name: %s", native)
	}
	if !strings.Contains(native, `await tools.exec_command`) {
		t.Fatalf("native response should restore raw JS input: %s", native)
	}
	if strings.Contains(native, `"type":"function_call"`) {
		t.Fatalf("custom exec should not be restored as function_call: %s", native)
	}
}

func TestResponsesNamespacedToolRestoresNamespace(t *testing.T) {
	req := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"role":"user","content":"spawn"},
			{"type":"additional_tools","tools":[
				{"type":"namespace","name":"collaboration","tools":[
					{"type":"function","name":"spawn_agent","description":"Spawn a sub-agent.","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}
				]}
			]}
		]
	}`)

	_, state, err := openaiResponsesToChat(req)
	if err != nil {
		t.Fatalf("openaiResponsesToChat() error = %v", err)
	}

	chatResp := `{"choices":[{"message":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"collaboration__spawn_agent","arguments":"{\"message\":\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`
	native := adaptChatResponseToNative("responses", chatResp, false, "gpt-5.6-sol", state)
	if !strings.Contains(native, `"type":"function_call"`) {
		t.Fatalf("native response should contain function_call: %s", native)
	}
	if !strings.Contains(native, `"namespace":"collaboration"`) {
		t.Fatalf("native response should restore namespace: %s", native)
	}
	if !strings.Contains(native, `"name":"spawn_agent"`) {
		t.Fatalf("native response should restore original tool name: %s", native)
	}
}

func TestResponsesUsageMapsDeepSeekCacheHitTokens(t *testing.T) {
	chatResp := `{
		"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],
		"usage":{
			"prompt_tokens":100,
			"completion_tokens":7,
			"total_tokens":107,
			"prompt_cache_hit_tokens":80,
			"prompt_cache_miss_tokens":20,
			"completion_tokens_details":{"reasoning_tokens":3}
		}
	}`

	native := adaptChatResponseToNative("responses", chatResp, true, "deepseek-chat", nil)
	if !strings.Contains(native, `"cached_tokens":80`) {
		t.Fatalf("native response should expose cached tokens for recorder: %s", native)
	}
	if !strings.Contains(native, `"reasoning_tokens":3`) {
		t.Fatalf("native response should expose reasoning tokens for recorder: %s", native)
	}

	usage := extractUsage(native, providerCodex)
	if usage.CachedInputTokens != 80 {
		t.Fatalf("CachedInputTokens = %d, want 80", usage.CachedInputTokens)
	}
	if usage.ReasoningTokens != 3 {
		t.Fatalf("ReasoningTokens = %d, want 3", usage.ReasoningTokens)
	}
}

func TestResponsesChatRequestIncludesUsageForStreaming(t *testing.T) {
	req := []byte(`{
		"model":"mimo-v2.5-pro",
		"stream":true,
		"input":[{"role":"user","content":"hello"}]
	}`)

	chatBody, _, err := openaiResponsesToChat(req)
	if err != nil {
		t.Fatalf("openaiResponsesToChat() error = %v", err)
	}

	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatalf("chat body should be JSON: %v", err)
	}
	streamOptions, ok := chat["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing from streaming chat request: %s", chatBody)
	}
	if streamOptions["include_usage"] != true {
		t.Fatalf("include_usage = %#v, want true", streamOptions["include_usage"])
	}
}

func TestResponsesUsageMapsMiMoPromptTokenDetails(t *testing.T) {
	chatResp := `{
		"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],
		"usage":{
			"prompt_tokens":100,
			"completion_tokens":7,
			"total_tokens":107,
			"prompt_tokens_details":{"cached_tokens":80},
			"completion_tokens_details":{"reasoning_tokens":3}
		}
	}`

	native := adaptChatResponseToNative("responses", chatResp, true, "mimo-v2.5-pro", nil)
	usage := extractUsage(native, providerCodex)
	if usage.CachedInputTokens != 80 {
		t.Fatalf("CachedInputTokens = %d, want 80", usage.CachedInputTokens)
	}
	if usage.ReasoningTokens != 3 {
		t.Fatalf("ReasoningTokens = %d, want 3", usage.ReasoningTokens)
	}
}

func TestEmptyChatCompletionDetectsNoUsefulOutput(t *testing.T) {
	empty := parseChatResponse(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	if !emptyChatCompletion(empty) {
		t.Fatalf("empty chat completion should be treated as no useful output")
	}

	whitespace := parseChatResponse(`{"choices":[{"message":{"content":"\n\n"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
	if !emptyChatCompletion(whitespace) {
		t.Fatalf("whitespace-only chat completion should be treated as no useful output")
	}

	tool := parseChatResponse(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","function":{"name":"exec","arguments":"{\"input\":\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`)
	if emptyChatCompletion(tool) {
		t.Fatalf("tool call response should be useful output")
	}
}
