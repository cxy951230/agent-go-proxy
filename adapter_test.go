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

// DeepSeek 思考模式要求把上一轮的 reasoning_content 原样带回来,否则 400
// "The `reasoning_content` in the thinking mode must be passed back to the API."。
// 代理不做服务端缓存,而是把它转成 Responses 的 reasoning item 交给 Codex CLI 回传,
// 下一轮再还原到 assistant 消息上。这里覆盖完整来回。
func TestReasoningContentRoundTripsThroughResponsesItem(t *testing.T) {
	chatSSE := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先看\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"文件行数\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	native := adaptChatResponseToNative("responses", chatSSE, true, "deepseek-v4-pro", nil)
	if !strings.Contains(native, `{"text":"先看文件行数","type":"reasoning_text"}`) {
		t.Fatalf("reasoning_content should be emitted as a reasoning item: %s", native)
	}
	// reasoning 必须排在 function_call 之前,下一轮才能按顺序归属
	if strings.Index(native, `"type":"reasoning"`) > strings.Index(native, `"type":"function_call"`) {
		t.Fatalf("reasoning item must precede function_call: %s", native)
	}

	// Codex CLI 会把 reasoning item 原样放回下一轮 input
	req := []byte(`{
		"model":"deepseek-v4-pro",
		"stream":true,
		"input":[
			{"role":"user","content":"数一下行数"},
			{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"先看文件行数"}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"3781"}
		]
	}`)
	chatBody, _, err := openaiResponsesToChat(req)
	if err != nil {
		t.Fatalf("openaiResponsesToChat() error = %v", err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatalf("chat body should be JSON: %v", err)
	}
	var assistant map[string]any
	for _, m := range chat.Messages {
		if m["role"] == "assistant" {
			assistant = m
			break
		}
	}
	if assistant == nil {
		t.Fatalf("assistant message missing: %s", chatBody)
	}
	if assistant["reasoning_content"] != "先看文件行数" {
		t.Fatalf("reasoning_content = %#v, want 先看文件行数: %s", assistant["reasoning_content"], chatBody)
	}
	// reasoning item 本身不能再变成一条独立消息
	for _, m := range chat.Messages {
		if m["role"] == "user" && m["content"] == "先看文件行数" {
			t.Fatalf("reasoning item leaked into a user message: %s", chatBody)
		}
	}
}

// 非思考模型不产出 reasoning item,行为保持不变。
func TestNoReasoningItemWhenModelDoesNotThink(t *testing.T) {
	native := adaptChatResponseToNative("responses",
		`{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`, true, "deepseek-chat", nil)
	if strings.Contains(native, `"type":"reasoning"`) {
		t.Fatalf("no reasoning item expected: %s", native)
	}
}

// reasoning 后面跟的不是 assistant 消息时要丢弃,不能错挂到 user 上。
func TestOrphanReasoningIsDropped(t *testing.T) {
	req := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"孤儿"}]},
			{"role":"user","content":"hi"},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"}
		]
	}`)
	chatBody, _, err := openaiResponsesToChat(req)
	if err != nil {
		t.Fatalf("openaiResponsesToChat() error = %v", err)
	}
	if strings.Contains(string(chatBody), "reasoning_content") {
		t.Fatalf("orphan reasoning should be dropped: %s", chatBody)
	}
}
