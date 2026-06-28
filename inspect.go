package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

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

func requestMetaFromHTTP(r *http.Request, body []byte) requestMeta {
	return requestMetaFromHeaders(r.Header, body)
}

func requestMetaFromHeaders(headers http.Header, body []byte) requestMeta {
	sessionID := firstHeader(headers, "Session_id")
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

func firstHeader(h http.Header, key string) string {
	values := h.Values(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
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
		"<environment_context>",
		"<permissions instructions>",
		"<app-context>",
		"<collaboration_mode>",
		"<skills_instructions>",
		"<plugins_instructions>",
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

func extractUsage(body string) usageStats {
	return extractUsageFromEvents(parseSSEEvents(body))
}

func extractUsageFromEvents(events []sseEvent) usageStats {
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
