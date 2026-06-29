package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTruncateBase64ImagesJSON(t *testing.T) {
	longImage := "data:image/png;base64," + strings.Repeat("A", 80)
	raw := `{"input":[{"content":[{"type":"input_image","image_url":"` + longImage + `"}]}]}`

	got := truncateBase64Images(raw)
	if strings.Contains(got, strings.Repeat("A", 31)) {
		t.Fatalf("base64 was not truncated: %s", got)
	}
	if !strings.Contains(got, "data:image/png;base64,"+strings.Repeat("A", 30)+"...[truncated]") {
		t.Fatalf("missing expected truncated image preview: %s", got)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("truncated JSON should still be valid: %v\n%s", err, got)
	}
}

func TestTruncateBase64ImagesPlainText(t *testing.T) {
	longImage := "data:image/jpeg;base64," + strings.Repeat("B", 80)
	got := truncateBase64Images("before " + longImage + " after")

	if strings.Contains(got, strings.Repeat("B", 31)) {
		t.Fatalf("base64 was not truncated: %s", got)
	}
	if !strings.Contains(got, "data:image/jpeg;base64,"+strings.Repeat("B", 30)+"...[truncated]") {
		t.Fatalf("missing expected truncated image preview: %s", got)
	}
	if !strings.Contains(got, "before ") || !strings.Contains(got, " after") {
		t.Fatalf("surrounding text was not preserved: %s", got)
	}
}

func TestTruncateBase64ImagesLeavesShortImages(t *testing.T) {
	shortImage := "data:image/png;base64," + strings.Repeat("C", 30)
	got := truncateBase64Images(`{"image_url":"` + shortImage + `"}`)

	if strings.Contains(got, "[truncated]") {
		t.Fatalf("short image should not be truncated: %s", got)
	}
}
