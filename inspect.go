package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	providerCodex  = "codex"
	providerClaude = "claude"
)

// agentLabel 把内部 provider 标识映射成看板展示用的 Agent 名字。
func agentLabel(provider string) string {
	if provider == providerClaude {
		return "Claude"
	}
	return "Codex"
}

type requestMeta struct {
	SessionID   string
	AccountID   string
	WindowID    string
	TurnID      string
	FirstPrompt string
	Model       string
}

type sseEvent struct {
	Index int             `json:"index"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
	Text  string          `json:"text,omitempty"`
	Time  string          `json:"time"`
}

type usageStats struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	TotalTokens       int `json:"total_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
}

func requestMetaFromHTTP(r *http.Request, body []byte, provider string) requestMeta {
	return requestMetaFromHeaders(r.Header, body, provider)
}

// modelFromBody 轻量解析请求体里的 model,用于日志按模型分文件。
func modelFromBody(body []byte) string {
	var parsed struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &parsed)
	return strings.TrimSpace(parsed.Model)
}

func requestMetaFromHeaders(headers http.Header, body []byte, provider string) requestMeta {
	if provider == providerClaude {
		return claudeRequestMeta(headers, body)
	}
	// 新版 Codex CLI 发的是 Session-Id(连字符),旧版是 Session_id(下划线)。
	// HTTP 头名大小写不敏感,但 '-' 和 '_' 是不同字符,两个都认,连字符优先。
	sessionID := firstHeaderAny(headers, "Session-Id", "Session_id")
	accountID := firstHeader(headers, "Chatgpt-Account-Id")
	windowID := firstHeader(headers, "X-Codex-Window-Id")
	if sessionID == "" && windowID != "" {
		sessionID = strings.SplitN(windowID, ":", 2)[0]
	}
	if sessionID == "" {
		sessionID = firstHeader(headers, "X-Client-Request-Id")
	}
	turnID := ""
	if raw := firstHeader(headers, "X-Codex-Turn-Metadata"); raw != "" {
		var meta struct {
			SessionID string `json:"session_id"`
			TurnID    string `json:"turn_id"`
		}
		if json.Unmarshal([]byte(raw), &meta) == nil {
			if sessionID == "" {
				sessionID = meta.SessionID
			}
			turnID = meta.TurnID
		}
	}

	var parsed struct {
		Model string `json:"model"`
		Input []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"input"`
	}
	_ = json.Unmarshal(body, &parsed)

	return requestMeta{
		SessionID:   fallback(sessionID, "unknown-"+time.Now().Format("20060102150405.000000")),
		AccountID:   accountID,
		WindowID:    windowID,
		TurnID:      turnID,
		FirstPrompt: firstUserPrompt(parsed.Input),
		Model:       parsed.Model,
	}
}

func claudeRequestMeta(headers http.Header, body []byte) requestMeta {
	var parsed struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &parsed)

	sessionID := firstHeader(headers, "X-Session-Id")
	accountID := ""
	// Claude Code 把会话/账号塞进 metadata.user_id，且它是「字符串套 JSON」:
	// {"device_id":"...","account_uuid":"...","session_id":"..."}
	if uid := strings.TrimSpace(parsed.Metadata.UserID); uid != "" {
		var inner struct {
			AccountUUID string `json:"account_uuid"`
			SessionID   string `json:"session_id"`
			DeviceID    string `json:"device_id"`
		}
		if json.Unmarshal([]byte(uid), &inner) == nil {
			if sessionID == "" {
				sessionID = inner.SessionID
			}
			accountID = fallback(inner.AccountUUID, inner.DeviceID)
		}
	}

	firstPrompt := firstUserPromptClaude(parsed.Messages)
	// max_tokens<=1 是 Claude Code 启动时的 quota/warmup 探测(content 常为 "quota")，
	// 不是真实对话，给占位串让后续真实请求覆盖掉。
	if parsed.MaxTokens > 0 && parsed.MaxTokens <= 1 {
		firstPrompt = "未捕获到用户 prompt。"
	}

	return requestMeta{
		SessionID:   fallback(sessionID, "unknown-"+time.Now().Format("20060102150405.000000")),
		AccountID:   accountID,
		FirstPrompt: firstPrompt,
		Model:       parsed.Model,
	}
}

// isClaudeQuotaProbe 判断是否是 Claude Code 启动时的 quota/warmup 探测请求
// (max_tokens<=1)。这类请求不是真实对话,其失败(如 429)不应算作会话错误。
func isClaudeQuotaProbe(body []byte) bool {
	var parsed struct {
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	return parsed.MaxTokens > 0 && parsed.MaxTokens <= 1
}

func firstUserPromptClaude(messages []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) string {
	for _, item := range messages {
		if item.Role != "user" {
			continue
		}
		if text := firstRealText(item.Content); text != "" {
			return limitString(text, 240)
		}
	}
	return "未捕获到用户 prompt。"
}

// firstRealText 从 Claude 消息内容里取第一段非注入上下文的纯文本。
// 必须逐 block 判断:真实消息常是 [<system-reminder>, <system-reminder>, 真正的prompt]，
// 若按整条拼接判断会因开头是 system-reminder 而被整条丢弃。
func firstRealText(content any) string {
	switch v := content.(type) {
	case string:
		if t := strings.TrimSpace(v); t != "" && !isInjectedContext(t) {
			return t
		}
	case []any:
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			text, _ := m["text"].(string)
			if text = strings.TrimSpace(text); text != "" && !isInjectedContext(text) {
				return text
			}
		}
	}
	return ""
}

func firstHeader(h http.Header, key string) string {
	values := h.Values(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// firstHeaderAny 按顺序返回第一个非空的头值,用于兼容同义但拼写不同的头名
// (如 Session-Id / Session_id)。
func firstHeaderAny(h http.Header, keys ...string) string {
	for _, key := range keys {
		if value := firstHeader(h, key); value != "" {
			return value
		}
	}
	return ""
}

func firstUserPrompt(input []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) string {
	for _, item := range input {
		if item.Role != "user" {
			continue
		}
		text := contentText(item.Content)
		text = strings.TrimSpace(text)
		if text != "" && !isInjectedContext(text) {
			return limitString(text, 240)
		}
	}
	return "未捕获到用户 prompt。"
}

func isInjectedContext(text string) bool {
	trimmed := strings.TrimSpace(text)
	prefixes := []string{
		"# AGENTS.md instructions",
		"<environment_context>",
		"<permissions instructions>",
		"<app-context>",
		"<collaboration_mode>",
		"<skills_instructions>",
		"<plugins_instructions>",
		"<skill>",
		// Claude Code 注入的上下文
		"<system-reminder>",
		"<command-message>",
		"<command-name>",
		"<local-command-stdout>",
		// Claude Code 辅助请求(会话标题生成等)的包裹标签,非真实用户 prompt
		"<session>",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func contentText(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case []any:
		var b strings.Builder
		for _, part := range value {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func parseSSEEvents(body string) []sseEvent {
	events := make([]sseEvent, 0, 32)
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1024), 1024*1024*8)
	var eventName string
	var dataLines []string
	flush := func() {
		if eventName == "" && len(dataLines) == 0 {
			return
		}
		text := strings.Join(dataLines, "\n")
		ev := sseEvent{
			Index: len(events) + 1,
			Event: fallback(eventName, "message"),
			Text:  text,
			Time:  time.Now().Format(time.RFC3339Nano),
		}
		if json.Valid([]byte(text)) {
			ev.Data = json.RawMessage(text)
		}
		events = append(events, ev)
		eventName = ""
		dataLines = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return events
}

func extractUsage(body, provider string) usageStats {
	return extractUsageFromEvents(parseSSEEvents(body), provider)
}

func extractUsageFromEvents(events []sseEvent, provider string) usageStats {
	if provider == providerClaude {
		return claudeUsageFromEvents(events)
	}
	var out usageStats
	for _, ev := range events {
		if ev.Event != "response.completed" || len(ev.Data) == 0 {
			continue
		}
		var payload struct {
			Response struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
					InputDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
					OutputDetails struct {
						ReasoningTokens int `json:"reasoning_tokens"`
					} `json:"output_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil {
			out.InputTokens = payload.Response.Usage.InputTokens
			out.OutputTokens = payload.Response.Usage.OutputTokens
			out.TotalTokens = payload.Response.Usage.TotalTokens
			out.CachedInputTokens = payload.Response.Usage.InputDetails.CachedTokens
			out.ReasoningTokens = payload.Response.Usage.OutputDetails.ReasoningTokens
		}
	}
	return out
}

// claudeUsage 是 Anthropic usage 对象的通用结构,message_start.message.usage
// 与 message_delta.usage 共用同一套字段。
type claudeUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	OutputDetails       struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// claudeUsageFromEvents 解析 Anthropic SSE 的 token 用量,并对齐 Codex 的语义:
//   - Anthropic 把输入拆成 input_tokens(全新) / cache_creation(首次写缓存) / cache_read(命中缓存),
//     三者都是被处理的输入,所以 input = 三者之和,与 Codex 的 input_tokens(含缓存)口径一致;
//   - cached 取 cache_read(真正复用、便宜的那部分),与 Codex 的 cached_tokens 一致;
//   - 输入侧字段在 message_start 已给全,最终 output 以 message_delta 为准。
func claudeUsageFromEvents(events []sseEvent) usageStats {
	var start, delta claudeUsage
	for _, ev := range events {
		if len(ev.Data) == 0 {
			continue
		}
		switch ev.Event {
		case "message_start":
			var payload struct {
				Message struct {
					Usage claudeUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				start = payload.Message.Usage
			}
		case "message_delta":
			var payload struct {
				Usage claudeUsage `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				delta = payload.Usage
			}
		}
	}
	// 输入侧优先用 delta(同样给全且为最终值),缺失则回退 start
	pick := func(a, b int) int {
		if a > 0 {
			return a
		}
		return b
	}
	return usageFromClaude(claudeUsage{
		InputTokens:         pick(delta.InputTokens, start.InputTokens),
		OutputTokens:        pick(delta.OutputTokens, start.OutputTokens),
		CacheCreationTokens: pick(delta.CacheCreationTokens, start.CacheCreationTokens),
		CacheReadTokens:     pick(delta.CacheReadTokens, start.CacheReadTokens),
		OutputDetails: struct {
			ThinkingTokens int `json:"thinking_tokens"`
		}{ThinkingTokens: pick(delta.OutputDetails.ThinkingTokens, start.OutputDetails.ThinkingTokens)},
	})
}

// claudeUsageFromJSONBody 解析非流式响应:Claude Code 会发 stream=false 的辅助请求
// (如会话标题、话题判定),usage 直接在响应 JSON 顶层,没有 SSE 事件。
func claudeUsageFromJSONBody(body string) usageStats {
	var payload struct {
		Usage claudeUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return usageStats{}
	}
	return usageFromClaude(payload.Usage)
}

// usageFromClaude 把 Anthropic 的 usage 折算成对齐 Codex 口径的统计:
// input 含全部被处理输入(新增+写缓存+读缓存),cached 取读缓存命中。
func usageFromClaude(u claudeUsage) usageStats {
	var out usageStats
	out.InputTokens = u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
	out.CachedInputTokens = u.CacheReadTokens
	out.OutputTokens = u.OutputTokens
	out.ReasoningTokens = u.OutputDetails.ThinkingTokens
	out.TotalTokens = out.InputTokens + out.OutputTokens
	return out
}

func fallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func limitString(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
