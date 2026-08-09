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

// 一轮里的并行 tool_call 必须归并进同一条 assistant 消息。
// 拆成多条会让 DeepSeek 报 "assistant message with 'tool_calls' must be followed by
// tool messages",且第二条挂不上 reasoning_content 又报 "reasoning_content in the
// thinking mode must be passed back"。形状取自真实失败请求(trace 7009)。
func TestParallelToolCallsMergeIntoOneAssistantMessage(t *testing.T) {
	req := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{"role":"user","content":"执行十次"},
			{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"先验证脚本路径"}]},
			{"role":"assistant","content":[{"type":"output_text","text":"先验证脚本路径和参数:"}]},
			{"type":"function_call","call_id":"call_00_a","name":"exec_command","arguments":"{\"cmd\":\"ls a\"}"},
			{"type":"function_call","call_id":"call_01_b","name":"exec_command","arguments":"{\"cmd\":\"ls b\"}"},
			{"type":"function_call_output","call_id":"call_00_a","output":"NOT FOUND"},
			{"type":"function_call_output","call_id":"call_01_b","output":"NOT FOUND"}
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
	if len(chat.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4 (user, assistant, tool, tool): %s", len(chat.Messages), chatBody)
	}
	assistant := chat.Messages[1]
	if assistant["role"] != "assistant" {
		t.Fatalf("messages[1] should be the assistant turn: %s", chatBody)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls len = %d, want 2 merged into one message: %s", len(calls), chatBody)
	}
	// 同一条消息上正文与 tool_calls 共存,reasoning 也挂在这条上
	if assistant["content"] != "先验证脚本路径和参数:" {
		t.Fatalf("assistant content = %#v, want the text of the same turn: %s", assistant["content"], chatBody)
	}
	if assistant["reasoning_content"] != "先验证脚本路径" {
		t.Fatalf("reasoning_content = %#v, want 先验证脚本路径: %s", assistant["reasoning_content"], chatBody)
	}
	// 带 tool_calls 的 assistant 后面必须紧跟它的 tool 消息
	for i, want := range []string{"call_00_a", "call_01_b"} {
		m := chat.Messages[2+i]
		if m["role"] != "tool" || m["tool_call_id"] != want {
			t.Fatalf("messages[%d] = %#v, want tool message for %s: %s", 2+i, m, want, chatBody)
		}
	}
}

// 顺序调用(call → output → call → output)仍然各自成一条 assistant 消息。
func TestSequentialToolCallsStayInSeparateMessages(t *testing.T) {
	req := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{"role":"user","content":"跑两步"},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"ok"},
			{"type":"function_call","call_id":"call_b","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_b","output":"ok"}
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
	roles := []string{}
	for _, m := range chat.Messages {
		roles = append(roles, m["role"].(string))
	}
	want := []string{"user", "assistant", "tool", "assistant", "tool"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v: %s", roles, want, chatBody)
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

// message 排在 function_call 之后时也必须并进同一条 assistant,
// 否则又会退化成 assistant(tool_calls) → assistant(text) → tool(...)。
func TestAssistantTextAfterToolCallStaysInSameMessage(t *testing.T) {
	req := []byte(`{
		"model":"deepseek-v4-pro",
		"input":[
			{"role":"user","content":"跑一下"},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"},
			{"role":"assistant","content":[{"type":"output_text","text":"顺便说明一下"}]},
			{"type":"function_call_output","call_id":"call_a","output":"ok"}
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
	if len(chat.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3 (user, assistant, tool): %s", len(chat.Messages), chatBody)
	}
	assistant := chat.Messages[1]
	if assistant["content"] != "顺便说明一下" {
		t.Fatalf("assistant content = %#v: %s", assistant["content"], chatBody)
	}
	if calls, _ := assistant["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("tool_calls should stay on the same message: %s", chatBody)
	}
	if chat.Messages[2]["role"] != "tool" {
		t.Fatalf("tool message must directly follow the tool_calls message: %s", chatBody)
	}
}
