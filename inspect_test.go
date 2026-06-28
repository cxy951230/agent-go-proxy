package main

import "testing"

func TestFirstUserPromptSkipsInjectedContext(t *testing.T) {
	input := []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{
		{Role: "developer", Content: "internal"},
		{Role: "user", Content: []any{map[string]any{"type": "input_text", "text": "<environment_context>\n  <cwd>/tmp</cwd>\n</environment_context>"}}},
		{Role: "user", Content: []any{map[string]any{"type": "input_text", "text": "你好"}}},
	}

	if got := firstUserPrompt(input); got != "你好" {
		t.Fatalf("firstUserPrompt() = %q, want %q", got, "你好")
	}
}
