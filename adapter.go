package main

import (
	"bufio"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 协议适配层:当启用路由的协议是 chat_completions,但请求本身是 Anthropic Messages
// 或 OpenAI Responses 时,把请求转换成 Chat Completions 转发给第三方,再把第三方返回的
// Chat 响应转换回原生协议(SSE 或 JSON)返回给客户端。
//
// 响应采用「读完上游 → 一次性转吐」策略:实现简单、正确、易测;客户端拿到的仍是合法
// 的原生 SSE,只是不是逐 token 增量。够 agent 场景用。

// ---------------- 请求: native → chat ----------------

type adapterState struct {
	Tools map[string]responseToolSpec
}

type responseToolSpec struct {
	Type      string
	Name      string
	Namespace string
}

// adaptRequestToChat 把 messages / responses 请求体转换成 Chat Completions 请求体。
// multimodal 标记目标路由的模型是否支持图片:仅 Responses→Chat 路径消费它,
// false 时丢掉 input_image 只留文字,避免非多模态上游收到 image_url 报错。
func adaptRequestToChat(reqProtocol string, body []byte, multimodal bool) ([]byte, *adapterState, error) {
	switch reqProtocol {
	case "messages":
		out, err := anthropicMessagesToChat(body)
		return out, nil, err
	case "responses":
		return openaiResponsesToChat(body, multimodal)
	}
	return body, nil, nil
}

func anthropicMessagesToChat(body []byte) ([]byte, error) {
	var src struct {
		Model       string          `json:"model"`
		MaxTokens   int             `json:"max_tokens"`
		Stream      bool            `json:"stream"`
		Temperature *float64        `json:"temperature"`
		TopP        *float64        `json:"top_p"`
		System      json.RawMessage `json:"system"`
		Messages    []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	chat := map[string]any{"model": src.Model, "stream": src.Stream}
	if src.MaxTokens > 0 {
		chat["max_tokens"] = src.MaxTokens
	}
	if src.Temperature != nil {
		chat["temperature"] = *src.Temperature
	}
	if src.TopP != nil {
		chat["top_p"] = *src.TopP
	}
	msgs := []map[string]any{}
	if sys := anthropicSystemText(src.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range src.Messages {
		msgs = append(msgs, anthropicContentToChat(m.Role, m.Content)...)
	}
	chat["messages"] = msgs
	if len(src.Tools) > 0 {
		tools := make([]map[string]any, 0, len(src.Tools))
		for _, t := range src.Tools {
			var params any = map[string]any{"type": "object", "properties": map[string]any{}}
			if len(t.InputSchema) > 0 {
				_ = json.Unmarshal(t.InputSchema, &params)
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params,
			}})
		}
		chat["tools"] = tools
	}
	if tc := anthropicToolChoiceToChat(src.ToolChoice); tc != nil {
		chat["tool_choice"] = tc
	}
	return json.Marshal(chat)
}

func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, b := range arr {
			if t, ok := b["text"].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// anthropicContentToChat 把一条 Anthropic 消息转换成一条或多条 Chat 消息。
// assistant 的 tool_use → tool_calls;user 里的 tool_result → 独立的 role=tool 消息。
func anthropicContentToChat(role string, content json.RawMessage) []map[string]any {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []map[string]any{{"role": role, "content": s}}
	}
	var blocks []map[string]any
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	if role == "assistant" {
		var text strings.Builder
		var toolCalls []any
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				text.WriteString(asStr(b["text"]))
			case "tool_use":
				args := "{}"
				if b["input"] != nil {
					if raw, err := json.Marshal(b["input"]); err == nil {
						args = string(raw)
					}
				}
				toolCalls = append(toolCalls, map[string]any{"id": asStr(b["id"]), "type": "function",
					"function": map[string]any{"name": asStr(b["name"]), "arguments": args}})
			}
		}
		msg := map[string]any{"role": "assistant"}
		if text.Len() > 0 {
			msg["content"] = text.String()
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []map[string]any{msg}
	}
	// user / 其它:tool_result 先转成 tool 消息,再把剩余 text/image 作为一条 user 消息
	var toolMsgs []map[string]any
	var userParts []any
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			userParts = append(userParts, map[string]any{"type": "text", "text": asStr(b["text"])})
		case "image":
			if src, ok := b["source"].(map[string]any); ok {
				url := asStr(src["url"])
				if src["type"] == "base64" {
					url = "data:" + asStr(src["media_type"]) + ";base64," + asStr(src["data"])
				}
				if url != "" {
					userParts = append(userParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{"role": "tool",
				"tool_call_id": asStr(b["tool_use_id"]), "content": anthropicToolResultText(b["content"])})
		}
	}
	out := toolMsgs
	if len(userParts) == 1 {
		if m, ok := userParts[0].(map[string]any); ok && m["type"] == "text" {
			return append(out, map[string]any{"role": "user", "content": m["text"]})
		}
	}
	if len(userParts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": userParts})
	}
	return out
}

func anthropicToolResultText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				} else if raw, err := json.Marshal(m); err == nil {
					parts = append(parts, string(raw))
				}
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

func anthropicToolChoiceToChat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if tc.Name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
		}
	}
	return nil
}

func openaiResponsesToChat(body []byte, multimodal bool) ([]byte, *adapterState, error) {
	var src struct {
		Model           string          `json:"model"`
		Stream          bool            `json:"stream"`
		Instructions    string          `json:"instructions"`
		Input           json.RawMessage `json:"input"`
		Tools           json.RawMessage `json:"tools"`
		MaxOutputTokens int             `json:"max_output_tokens"`
		Temperature     *float64        `json:"temperature"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, nil, err
	}
	state := &adapterState{Tools: map[string]responseToolSpec{}}
	chat := map[string]any{"model": src.Model, "stream": src.Stream}
	if src.Stream {
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	if src.MaxOutputTokens > 0 {
		chat["max_tokens"] = src.MaxOutputTokens
	}
	if src.Temperature != nil {
		chat["temperature"] = *src.Temperature
	}
	msgs := []map[string]any{}
	if src.Instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": src.Instructions})
	}
	toolDefs := responsesToolsToChat(src.Tools, state)

	var input []map[string]any
	_ = json.Unmarshal(src.Input, &input)
	// Responses 里一轮助手输出是若干平行 item(reasoning、message、多个 function_call),
	// 但 Chat 协议要求它们归并成【一条】assistant 消息:content 与 tool_calls 共存,
	// 且带 tool_calls 的 assistant 后面必须紧跟对应的 tool 消息。每个 item 各 append
	// 一条 assistant 会把并行调用拆成 assistant(A) → assistant(B) → tool(A) → tool(B),
	// DeepSeek 直接 400:结构上报 "assistant message with 'tool_calls' must be followed
	// by tool messages",拆出来的第二条又挂不上 reasoning_content,思考模式还会报
	// "reasoning_content in the thinking mode must be passed back"。
	//
	// 所以这里用 cur 累积同一轮的 item,遇到 tool 结果、user/system 消息或下一轮的
	// reasoning 才收尾。pendingReasoning 暂存 reasoning 文本,挂到本轮那条 assistant 上。
	pendingReasoning := ""
	var cur map[string]any
	flush := func() {
		if cur != nil {
			msgs = append(msgs, cur)
			cur = nil
		}
	}
	// assistantMsg 返回本轮正在累积的 assistant 消息,没有就新建并挂上 reasoning。
	assistantMsg := func() map[string]any {
		if cur == nil {
			cur = map[string]any{"role": "assistant", "content": nil}
			if pendingReasoning != "" {
				cur["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			}
		}
		return cur
	}
	addToolCall := func(call map[string]any) {
		msg := assistantMsg()
		calls, _ := msg["tool_calls"].([]any)
		msg["tool_calls"] = append(calls, call)
	}
	for _, item := range input {
		switch item["type"] {
		case "additional_tools":
			if raw, err := json.Marshal(item["tools"]); err == nil {
				toolDefs = append(toolDefs, responsesToolsToChat(raw, state)...)
			}
		case "reasoning":
			// reasoning 总是一轮的第一个 item,先把上一轮收尾再暂存。
			flush()
			if text := responsesReasoningText(item); text != "" {
				pendingReasoning = text
			}
		case "custom_tool_call":
			name := asStr(item["name"])
			addToolCall(map[string]any{"id": asStr(item["call_id"]), "type": "function",
				"function": map[string]any{"name": chatToolName("", name), "arguments": mustJSON(map[string]any{"input": asStr(item["input"])})}})
		case "function_call":
			name := asStr(item["name"])
			namespace := asStr(item["namespace"])
			addToolCall(map[string]any{"id": asStr(item["call_id"]), "type": "function",
				"function": map[string]any{"name": chatToolName(namespace, name), "arguments": asStr(item["arguments"])}})
		case "custom_tool_call_output", "function_call_output":
			flush()
			msgs = append(msgs, map[string]any{"role": "tool",
				"tool_call_id": asStr(item["call_id"]), "content": asStr(item["output"])})
		default: // message / 无 type
			role := asStr(item["role"])
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			text := responsesContentText(item["content"])
			if role == "assistant" {
				if text == "" {
					continue
				}
				// 一律并进本轮那条 assistant,不按 message/function_call 的先后做假设:
				// 只要中间没被 tool 结果或 user/system 打断就还是同一轮,拆开就会违反
				// 「带 tool_calls 的 assistant 必须紧跟 tool 消息」。
				msg := assistantMsg()
				if prev, ok := msg["content"].(string); ok && prev != "" {
					msg["content"] = prev + "\n" + text
				} else {
					msg["content"] = text
				}
				continue
			}
			flush()
			// 中间夹了 user/system,说明这段 reasoning 没有对应的 assistant 消息,丢弃避免错挂
			pendingReasoning = ""
			// user/system 消息里可能带 input_image:multimodal 路由才转成 Chat 多模态
			// content 数组转发;否则(默认)只留文字,避免非多模态上游收到 image_url 报错。
			content := responsesContentToChat(item["content"], multimodal)
			if s, ok := content.(string); ok {
				if s != "" {
					msgs = append(msgs, map[string]any{"role": role, "content": s})
				}
			} else {
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			}
		}
	}
	flush()
	// 混合思考模型(DeepSeek/Qwen3/GLM 等经 vLLM/SGLang/opencode 网关)在某轮动态进入
	// 思考模式时,会硬校验历史里 assistant 消息必须带 reasoning_content,否则 400
	// "The `reasoning_content` in the thinking mode must be passed back to the API."。
	// Codex 是 Responses 协议编排器,根本不认这个非标字段,且非思考轮压根没 reasoning
	// 可挂,导致间歇性 400。这里给所有缺 reasoning_content 的 assistant 消息补一个占位串:
	// 真实 reasoning(已由上面 pendingReasoning 挂上的)不覆盖;占位只为满足网关的结构校验,
	// 而历史 reasoning 本就被这些模型在入模时丢弃,不影响当轮思考质量。
	for _, m := range msgs {
		if m["role"] != "assistant" {
			continue
		}
		if _, ok := m["reasoning_content"]; !ok {
			m["reasoning_content"] = placeholderReasoning
		}
	}
	chat["messages"] = msgs
	if len(toolDefs) > 0 {
		chat["tools"] = toolDefs
	}
	out, err := json.Marshal(chat)
	return out, state, err
}

// responsesToolsToChat 把 Responses 的 tools 数组转成 Chat 的 function tools。
// namespace 分组会递归展开成带 "组名__" 前缀的函数名;custom 类型也归一成 function。
func responsesToolsToChat(raw json.RawMessage, state *adapterState) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	var out []map[string]any
	var add func(prefix string, t map[string]any)
	add = func(prefix string, t map[string]any) {
		toolType := asStr(t["type"])
		if t["type"] == "namespace" {
			if subs, ok := t["tools"].([]any); ok {
				for _, s := range subs {
					if sm, ok := s.(map[string]any); ok {
						add(prefix+asStr(t["name"])+"__", sm)
					}
				}
			}
			return
		}
		originalName := asStr(t["name"])
		// 内置工具(web_search / tool_search 等)只有 type 没有 name,
		// 直接转成 Chat function 会得到空函数名,三方(kimi/nvidia/minimax)
		// 一律报 "function name is invalid"。无 name 时统一用 type 兜底。
		if originalName == "" {
			originalName = toolType
		}
		if originalName == "" {
			return
		}
		chatName := prefix + originalName
		params := t["parameters"]
		description := asStr(t["description"])
		if toolType == "custom" {
			params = customToolParameters(originalName)
			description = customToolDescription(originalName, description)
		} else if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if state != nil {
			state.Tools[chatName] = responseToolSpec{
				Type:      toolType,
				Name:      originalName,
				Namespace: strings.TrimSuffix(prefix, "__"),
			}
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": chatName, "description": description, "parameters": params,
		}})
	}
	for _, t := range arr {
		add("", t)
	}
	return out
}

func chatToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

func customToolParameters(name string) map[string]any {
	inputDescription := "Raw freeform tool input. Do not wrap it in markdown."
	switch name {
	case "exec":
		inputDescription = "Raw JavaScript source for Codex exec. Do not pass a shell command directly. Use await tools.exec_command({cmd: \"...\"}) for shell commands. Do not wrap the JavaScript in markdown fences."
	case "apply_patch":
		inputDescription = "Raw apply_patch patch text beginning with *** Begin Patch. Do not wrap it in JSON or markdown fences."
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string", "description": inputDescription},
		},
		"required":             []string{"input"},
		"additionalProperties": false,
	}
}

func customToolDescription(name, original string) string {
	var b strings.Builder
	b.WriteString("Adapter wrapper for Codex custom/freeform tool `")
	b.WriteString(name)
	b.WriteString("`. Call this Chat function with exactly one JSON field `input`, then the proxy will restore it to a Codex `custom_tool_call`.\n")
	switch name {
	case "exec":
		b.WriteString("The `input` value must be raw JavaScript source accepted by Codex exec. To run shell commands, write JavaScript such as `await tools.exec_command({cmd: \"ls\", yield_time_ms: 10000})`; do not send `{cmd: ...}` as the final tool arguments and do not use markdown fences.\n")
	case "apply_patch":
		b.WriteString("The `input` value must be the raw patch text, starting with `*** Begin Patch` and ending with `*** End Patch`.\n")
	}
	if strings.TrimSpace(original) != "" {
		b.WriteString("\nOriginal Codex tool instructions:\n")
		b.WriteString(original)
	}
	return b.String()
}

// placeholderReasoning 是给缺失 reasoning_content 的 assistant 消息补的占位串,
// 仅用于通过思考模型网关的结构校验。取最短的中性内容,尽量不污染生成;若发现被网关
// 喂进模板影响质量,可改成空串或其它值。
const placeholderReasoning = "..."

// responsesReasoningText 从 reasoning item 里取出思考文本:
// 优先 content[].reasoning_text(代理自己吐回去的那份),回落 summary[].summary_text。
func responsesReasoningText(item map[string]any) string {
	if text := responsesContentText(item["content"]); text != "" {
		return text
	}
	return responsesContentText(item["summary"])
}

func responsesContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// responsesContentToChat 把 Responses 的 content 转成 Chat Completions 的 content。
// multimodal=false(默认)时只抽文字、丢掉 input_image,退回字符串,避免非多模态上游
// 收到 image_url 报错。multimodal=true 时:只有文本仍返回 string;一旦含 input_image
// 就返回 []any 多模态数组([{type:text}, {type:image_url}]),把图片转发给上游。
func responsesContentToChat(v any, multimodal bool) any {
	arr, ok := v.([]any)
	if !ok {
		return responsesContentText(v)
	}
	// 非多模态路由:等价于旧行为,只把文字拼起来,图片直接丢弃。
	if !multimodal {
		return responsesContentText(v)
	}
	var parts []any
	var textOnly strings.Builder
	hasImage := false
	for _, p := range arr {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := m["text"].(string); ok {
			parts = append(parts, map[string]any{"type": "text", "text": t})
			textOnly.WriteString(t)
			continue
		}
		if url := responsesImageURL(m); url != "" {
			img := map[string]any{"url": url}
			if d, ok := m["detail"].(string); ok && d != "" {
				img["detail"] = d
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
			hasImage = true
		}
	}
	if !hasImage {
		return textOnly.String()
	}
	return parts
}

// responsesImageURL 从 Responses 的 input_image part 取图片 URL。Codex 发的形态是
// {"type":"input_image","image_url":"data:image/...;base64,...","detail":"high"},
// image_url 是字符串;也兼容 {"image_url":{"url":...}} 的嵌套写法。
func responsesImageURL(m map[string]any) string {
	switch iu := m["image_url"].(type) {
	case string:
		return iu
	case map[string]any:
		return asStr(iu["url"])
	}
	return ""
}

// ---------------- 响应: chat → native ----------------

type chatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type chatUsage struct {
	Input     int
	Output    int
	Total     int
	Cached    int
	Reasoning int
}

func (u chatUsage) total() int {
	if u.Total > 0 {
		return u.Total
	}
	return u.Input + u.Output
}

type chatCompletion struct {
	Text string
	// Reasoning 是思考模型(DeepSeek 等)返回的 reasoning_content。
	// DeepSeek 思考模式要求下一轮把它随 assistant 消息原样回传,否则报
	// "The `reasoning_content` in the thinking mode must be passed back to the API."。
	// 代理不自己缓存,而是转成 Responses 的 reasoning item 交给 Codex CLI 带回来,
	// 详见 responsesReasoningText / emitResponsesSSE。
	Reasoning    string
	ToolCalls    []chatToolCall
	FinishReason string
	Usage        chatUsage
}

// adaptChatResponseToNative 把第三方 Chat 响应(SSE 或 JSON)转换回原生协议文本。
func adaptChatResponseToNative(target, chatBody string, stream bool, model string, state *adapterState) string {
	c := parseChatResponse(chatBody)
	switch target {
	case "messages":
		if stream {
			return emitAnthropicSSE(c, model)
		}
		return emitAnthropicJSON(c, model)
	case "responses":
		if stream {
			return emitResponsesSSE(c, model, state)
		}
		return emitResponsesJSON(c, model, state)
	}
	return chatBody
}

func parseChatResponse(body string) chatCompletion {
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		return parseChatJSON(body)
	}
	return parseChatSSE(body)
}

func parseChatSSE(s string) chatCompletion {
	var c chatCompletion
	byIdx := map[int]*chatToolCall{}
	var order []int
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens         int `json:"prompt_tokens"`
				CompletionTokens     int `json:"completion_tokens"`
				TotalTokens          int `json:"total_tokens"`
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
				PromptTokensDetails  struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			c.Usage = chatUsage{
				Input:     chunk.Usage.PromptTokens,
				Output:    chunk.Usage.CompletionTokens,
				Total:     chunk.Usage.TotalTokens,
				Cached:    fallbackInt(chunk.Usage.PromptTokensDetails.CachedTokens, chunk.Usage.PromptCacheHitTokens),
				Reasoning: chunk.Usage.CompletionTokensDetails.ReasoningTokens,
			}
		}
		for _, ch := range chunk.Choices {
			c.Text += ch.Delta.Content
			c.Reasoning += ch.Delta.ReasoningContent
			if ch.FinishReason != "" {
				c.FinishReason = ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				t, ok := byIdx[tc.Index]
				if !ok {
					t = &chatToolCall{}
					byIdx[tc.Index] = t
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					t.ID = tc.ID
				}
				if tc.Function.Name != "" {
					t.Name = tc.Function.Name
				}
				t.Arguments += tc.Function.Arguments
			}
		}
	}
	for _, i := range order {
		c.ToolCalls = append(c.ToolCalls, *byIdx[i])
	}
	return c
}

func parseChatJSON(body string) chatCompletion {
	var c chatCompletion
	var r struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens         int `json:"prompt_tokens"`
			CompletionTokens     int `json:"completion_tokens"`
			TotalTokens          int `json:"total_tokens"`
			PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
			PromptTokensDetails  struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(body), &r) != nil {
		return c
	}
	c.Usage = chatUsage{
		Input:     r.Usage.PromptTokens,
		Output:    r.Usage.CompletionTokens,
		Total:     r.Usage.TotalTokens,
		Cached:    fallbackInt(r.Usage.PromptTokensDetails.CachedTokens, r.Usage.PromptCacheHitTokens),
		Reasoning: r.Usage.CompletionTokensDetails.ReasoningTokens,
	}
	if len(r.Choices) > 0 {
		c.Text = r.Choices[0].Message.Content
		c.Reasoning = r.Choices[0].Message.ReasoningContent
		c.FinishReason = r.Choices[0].FinishReason
		for _, tc := range r.Choices[0].Message.ToolCalls {
			c.ToolCalls = append(c.ToolCalls, chatToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	return c
}

// ---- Anthropic Messages 输出 ----

func emitAnthropicSSE(c chatCompletion, model string) string {
	var b strings.Builder
	msgID := "msg_" + randID()
	b.WriteString(sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": c.Usage.Input, "output_tokens": 0},
	}}))
	idx := 0
	if c.Text != "" {
		b.WriteString(sse("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "text", "text": ""}}))
		b.WriteString(sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "text_delta", "text": c.Text}}))
		b.WriteString(sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}))
		idx++
	}
	for _, tc := range c.ToolCalls {
		b.WriteString(sse("content_block_start", map[string]any{"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": toolID(tc.ID), "name": tc.Name, "input": map[string]any{}}}))
		b.WriteString(sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": fallback(tc.Arguments, "{}")}}))
		b.WriteString(sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}))
		idx++
	}
	b.WriteString(sse("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(c), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": c.Usage.Output}}))
	b.WriteString(sse("message_stop", map[string]any{"type": "message_stop"}))
	return b.String()
}

func emitAnthropicJSON(c chatCompletion, model string) string {
	content := []any{}
	if c.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": c.Text})
	}
	for _, tc := range c.ToolCalls {
		var input any = map[string]any{}
		if tc.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Arguments), &input)
		}
		content = append(content, map[string]any{"type": "tool_use", "id": toolID(tc.ID), "name": tc.Name, "input": input})
	}
	return mustJSON(map[string]any{"id": "msg_" + randID(), "type": "message", "role": "assistant",
		"model": model, "content": content, "stop_reason": anthropicStopReason(c), "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": c.Usage.Input, "output_tokens": c.Usage.Output}})
}

func anthropicStopReason(c chatCompletion) string {
	if len(c.ToolCalls) > 0 || c.FinishReason == "tool_calls" {
		return "tool_use"
	}
	if c.FinishReason == "length" {
		return "max_tokens"
	}
	return "end_turn"
}

// ---- OpenAI Responses 输出 ----

func emitResponsesSSE(c chatCompletion, model string, state *adapterState) string {
	var b strings.Builder
	respID := "resp_" + randID()
	output := []any{}
	outIdx := 0
	b.WriteString(sse("response.created", map[string]any{"type": "response.created",
		"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": model, "output": []any{}}}))
	// reasoning item 必须排在 message / function_call 之前(真实 OpenAI 也是 output_index=0),
	// 下一轮 openaiResponsesToChat 才能按「reasoning 归属其后第一条 assistant 消息」还原。
	if c.Reasoning != "" {
		itemID := "rs_" + randID()
		b.WriteString(sse("response.output_item.added", map[string]any{"type": "response.output_item.added",
			"output_index": outIdx, "item": map[string]any{"id": itemID, "type": "reasoning", "summary": []any{}}}))
		doneItem := responseReasoningItem(itemID, c.Reasoning)
		b.WriteString(sse("response.output_item.done", map[string]any{"type": "response.output_item.done",
			"output_index": outIdx, "item": doneItem}))
		output = append(output, doneItem)
		outIdx++
	}
	if c.Text != "" {
		itemID := "msg_" + randID()
		b.WriteString(sse("response.output_item.added", map[string]any{"type": "response.output_item.added",
			"output_index": outIdx, "item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}))
		b.WriteString(sse("response.content_part.added", map[string]any{"type": "response.content_part.added",
			"item_id": itemID, "output_index": outIdx, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}}))
		b.WriteString(sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta",
			"item_id": itemID, "output_index": outIdx, "content_index": 0, "delta": c.Text}))
		b.WriteString(sse("response.output_text.done", map[string]any{"type": "response.output_text.done",
			"item_id": itemID, "output_index": outIdx, "content_index": 0, "text": c.Text}))
		b.WriteString(sse("response.content_part.done", map[string]any{"type": "response.content_part.done",
			"item_id": itemID, "output_index": outIdx, "content_index": 0, "part": map[string]any{"type": "output_text", "text": c.Text}}))
		doneItem := map[string]any{"id": itemID, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": c.Text}}}
		b.WriteString(sse("response.output_item.done", map[string]any{"type": "response.output_item.done",
			"output_index": outIdx, "item": doneItem}))
		output = append(output, doneItem)
		outIdx++
	}
	for _, tc := range c.ToolCalls {
		spec := responseSpecForTool(state, tc.Name)
		if spec.Type == "custom" {
			itemID := "ctc_" + randID()
			callID := toolID(tc.ID)
			input := customToolInput(tc.Arguments)
			addedItem := map[string]any{"id": itemID, "type": "custom_tool_call", "status": "in_progress", "call_id": callID, "name": spec.Name, "input": ""}
			b.WriteString(sse("response.output_item.added", map[string]any{"type": "response.output_item.added",
				"output_index": outIdx, "item": addedItem}))
			doneItem := map[string]any{"id": itemID, "type": "custom_tool_call", "status": "completed", "call_id": callID, "name": spec.Name, "input": input}
			b.WriteString(sse("response.output_item.done", map[string]any{"type": "response.output_item.done",
				"output_index": outIdx, "item": doneItem}))
			output = append(output, doneItem)
			outIdx++
			continue
		}
		itemID := "fc_" + randID()
		callID := toolID(tc.ID)
		name := spec.Name
		if name == "" {
			name = tc.Name
		}
		b.WriteString(sse("response.output_item.added", map[string]any{"type": "response.output_item.added",
			"output_index": outIdx, "item": responseFunctionCallItem(itemID, callID, name, spec.Namespace, "", "in_progress")}))
		b.WriteString(sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta",
			"item_id": itemID, "output_index": outIdx, "delta": tc.Arguments}))
		b.WriteString(sse("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done",
			"item_id": itemID, "output_index": outIdx, "arguments": tc.Arguments}))
		doneItem := responseFunctionCallItem(itemID, callID, name, spec.Namespace, tc.Arguments, "completed")
		b.WriteString(sse("response.output_item.done", map[string]any{"type": "response.output_item.done",
			"output_index": outIdx, "item": doneItem}))
		output = append(output, doneItem)
		outIdx++
	}
	b.WriteString(sse("response.completed", map[string]any{"type": "response.completed",
		"response": map[string]any{"id": respID, "object": "response", "status": "completed", "model": model, "output": output,
			"usage": responsesUsage(c.Usage)}}))
	return b.String()
}

func emitResponsesJSON(c chatCompletion, model string, state *adapterState) string {
	output := []any{}
	if c.Reasoning != "" {
		output = append(output, responseReasoningItem("rs_"+randID(), c.Reasoning))
	}
	if c.Text != "" {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": c.Text}}})
	}
	for _, tc := range c.ToolCalls {
		spec := responseSpecForTool(state, tc.Name)
		if spec.Type == "custom" {
			output = append(output, map[string]any{"type": "custom_tool_call", "status": "completed",
				"call_id": toolID(tc.ID), "name": spec.Name, "input": customToolInput(tc.Arguments)})
			continue
		}
		name := spec.Name
		if name == "" {
			name = tc.Name
		}
		output = append(output, responseFunctionCallItem("", toolID(tc.ID), name, spec.Namespace, tc.Arguments, "completed"))
	}
	return mustJSON(map[string]any{"id": "resp_" + randID(), "object": "response", "status": "completed",
		"model": model, "output": output,
		"usage": responsesUsage(c.Usage)})
}

func responsesUsage(u chatUsage) map[string]any {
	return map[string]any{
		"input_tokens":  u.Input,
		"output_tokens": u.Output,
		"total_tokens":  u.total(),
		"input_tokens_details": map[string]any{
			"cached_tokens": u.Cached,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": u.Reasoning,
		},
	}
}

func responseSpecForTool(state *adapterState, chatName string) responseToolSpec {
	if state != nil {
		if spec, ok := state.Tools[chatName]; ok {
			if spec.Name == "" {
				spec.Name = chatName
			}
			return spec
		}
	}
	if namespace, name, ok := strings.Cut(chatName, "__"); ok {
		return responseToolSpec{Type: "function", Name: name, Namespace: namespace}
	}
	return responseToolSpec{Type: "function", Name: chatName}
}

// responseReasoningItem 构造 Responses 的 reasoning item。
// 用 content[].reasoning_text 承载明文思考内容:Codex CLI 的 ResponseItem::Reasoning
// 对 content 的 skip_serializing_if 规则是「含 reasoning_text 就序列化」,所以这条会被
// 原样带回下一轮请求;summary 留空,避免在 TUI 里重复展示。
func responseReasoningItem(itemID, text string) map[string]any {
	return map[string]any{
		"id": itemID, "type": "reasoning", "summary": []any{},
		"content": []any{map[string]any{"type": "reasoning_text", "text": text}},
	}
}

func responseFunctionCallItem(itemID, callID, name, namespace, arguments, status string) map[string]any {
	item := map[string]any{"type": "function_call", "status": status, "call_id": callID, "name": name, "arguments": arguments}
	if itemID != "" {
		item["id"] = itemID
	}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

func customToolInput(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var text string
	if json.Unmarshal([]byte(trimmed), &text) == nil {
		return text
	}
	var obj map[string]any
	if json.Unmarshal([]byte(trimmed), &obj) == nil {
		for _, key := range []string{"input", "cmd", "code", "source", "javascript", "js", "patch"} {
			if value, ok := obj[key]; ok {
				return asStr(value)
			}
		}
	}
	return arguments
}

// ---------------- 小工具 ----------------

func sse(event string, data any) string {
	return "event: " + event + "\ndata: " + mustJSON(data) + "\n\n"
}

func mustJSON(v any) string {
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return "{}"
}

func asStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return ""
}

func fallbackInt(v, alt int) int {
	if v != 0 {
		return v
	}
	return alt
}

var idCounter uint64

func randID() string {
	n := atomic.AddUint64(&idCounter, 1)
	return strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatUint(n, 36)
}

func toolID(id string) string {
	if strings.TrimSpace(id) == "" {
		return "call_" + randID()
	}
	return id
}

// clientWantsStream 判断客户端原始请求是否要求流式;缺省视为 true(Codex/Claude 默认流式)。
func clientWantsStream(body []byte) bool {
	var s struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &s) == nil && s.Stream != nil {
		return *s.Stream
	}
	return true
}
