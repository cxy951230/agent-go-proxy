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

// adaptRequestToChat 把 messages / responses 请求体转换成 Chat Completions 请求体。
func adaptRequestToChat(reqProtocol string, body []byte) ([]byte, error) {
	switch reqProtocol {
	case "messages":
		return anthropicMessagesToChat(body)
	case "responses":
		return openaiResponsesToChat(body)
	}
	return body, nil
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
		Tools      []struct {
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

func openaiResponsesToChat(body []byte) ([]byte, error) {
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
		return nil, err
	}
	chat := map[string]any{"model": src.Model, "stream": src.Stream}
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
	toolDefs := responsesToolsToChat(src.Tools)

	var input []map[string]any
	_ = json.Unmarshal(src.Input, &input)
	for _, item := range input {
		switch item["type"] {
		case "additional_tools":
			if raw, err := json.Marshal(item["tools"]); err == nil {
				toolDefs = append(toolDefs, responsesToolsToChat(raw)...)
			}
		case "function_call":
			msgs = append(msgs, map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"id": asStr(item["call_id"]), "type": "function",
					"function": map[string]any{"name": asStr(item["name"]), "arguments": asStr(item["arguments"])}}}})
		case "function_call_output":
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
			if text := responsesContentText(item["content"]); text != "" {
				msgs = append(msgs, map[string]any{"role": role, "content": text})
			}
		}
	}
	chat["messages"] = msgs
	if len(toolDefs) > 0 {
		chat["tools"] = toolDefs
	}
	return json.Marshal(chat)
}

// responsesToolsToChat 把 Responses 的 tools 数组转成 Chat 的 function tools。
// namespace 分组会递归展开成带 "组名__" 前缀的函数名;custom 类型也归一成 function。
func responsesToolsToChat(raw json.RawMessage) []map[string]any {
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
		params := t["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{
			"name": prefix + asStr(t["name"]), "description": asStr(t["description"]), "parameters": params,
		}})
	}
	for _, t := range arr {
		add("", t)
	}
	return out
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

// ---------------- 响应: chat → native ----------------

type chatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type chatUsage struct {
	Input  int
	Output int
	Total  int
}

func (u chatUsage) total() int {
	if u.Total > 0 {
		return u.Total
	}
	return u.Input + u.Output
}

type chatCompletion struct {
	Text         string
	ToolCalls    []chatToolCall
	FinishReason string
	Usage        chatUsage
}

// adaptChatResponseToNative 把第三方 Chat 响应(SSE 或 JSON)转换回原生协议文本。
func adaptChatResponseToNative(target, chatBody string, stream bool, model string) string {
	c := parseChatResponse(chatBody)
	switch target {
	case "messages":
		if stream {
			return emitAnthropicSSE(c, model)
		}
		return emitAnthropicJSON(c, model)
	case "responses":
		if stream {
			return emitResponsesSSE(c, model)
		}
		return emitResponsesJSON(c, model)
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
					Content   string `json:"content"`
					ToolCalls []struct {
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
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			c.Usage = chatUsage{chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens}
		}
		for _, ch := range chunk.Choices {
			c.Text += ch.Delta.Content
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
				Content   string `json:"content"`
				ToolCalls []struct {
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
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(body), &r) != nil {
		return c
	}
	c.Usage = chatUsage{r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.TotalTokens}
	if len(r.Choices) > 0 {
		c.Text = r.Choices[0].Message.Content
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

func emitResponsesSSE(c chatCompletion, model string) string {
	var b strings.Builder
	respID := "resp_" + randID()
	output := []any{}
	outIdx := 0
	b.WriteString(sse("response.created", map[string]any{"type": "response.created",
		"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": model, "output": []any{}}}))
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
		itemID := "fc_" + randID()
		callID := toolID(tc.ID)
		b.WriteString(sse("response.output_item.added", map[string]any{"type": "response.output_item.added",
			"output_index": outIdx, "item": map[string]any{"id": itemID, "type": "function_call", "status": "in_progress", "call_id": callID, "name": tc.Name, "arguments": ""}}))
		b.WriteString(sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta",
			"item_id": itemID, "output_index": outIdx, "delta": tc.Arguments}))
		b.WriteString(sse("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done",
			"item_id": itemID, "output_index": outIdx, "arguments": tc.Arguments}))
		doneItem := map[string]any{"id": itemID, "type": "function_call", "status": "completed", "call_id": callID, "name": tc.Name, "arguments": tc.Arguments}
		b.WriteString(sse("response.output_item.done", map[string]any{"type": "response.output_item.done",
			"output_index": outIdx, "item": doneItem}))
		output = append(output, doneItem)
		outIdx++
	}
	b.WriteString(sse("response.completed", map[string]any{"type": "response.completed",
		"response": map[string]any{"id": respID, "object": "response", "status": "completed", "model": model, "output": output,
			"usage": map[string]any{"input_tokens": c.Usage.Input, "output_tokens": c.Usage.Output, "total_tokens": c.Usage.total()}}}))
	return b.String()
}

func emitResponsesJSON(c chatCompletion, model string) string {
	output := []any{}
	if c.Text != "" {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": c.Text}}})
	}
	for _, tc := range c.ToolCalls {
		output = append(output, map[string]any{"type": "function_call", "status": "completed",
			"call_id": toolID(tc.ID), "name": tc.Name, "arguments": tc.Arguments})
	}
	return mustJSON(map[string]any{"id": "resp_" + randID(), "object": "response", "status": "completed",
		"model": model, "output": output,
		"usage": map[string]any{"input_tokens": c.Usage.Input, "output_tokens": c.Usage.Output, "total_tokens": c.Usage.total()}})
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
