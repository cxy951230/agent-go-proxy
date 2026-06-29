package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const base64PreviewChars = 30

var dataImageBase64Pattern = regexp.MustCompile(`data:image/[^;,\s"\\]+;base64,([A-Za-z0-9+/=]+)`)

func truncateBase64Images(raw string) string {
	if raw == "" || !strings.Contains(raw, "data:image/") || !strings.Contains(raw, ";base64,") {
		return raw
	}

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		truncateBase64ImagesValue(parsed)
		encoded, err := marshalJSONNoEscape(parsed)
		if err == nil {
			return string(encoded)
		}
	}

	return dataImageBase64Pattern.ReplaceAllStringFunc(raw, truncateDataImageBase64)
}

func truncateBase64ImagesValue(v any) {
	switch value := v.(type) {
	case map[string]any:
		for key, item := range value {
			if text, ok := item.(string); ok {
				value[key] = truncateDataImageBase64(text)
				continue
			}
			truncateBase64ImagesValue(item)
		}
	case []any:
		for _, item := range value {
			truncateBase64ImagesValue(item)
		}
	}
}

func truncateDataImageBase64(text string) string {
	return dataImageBase64Pattern.ReplaceAllStringFunc(text, func(match string) string {
		prefixEnd := strings.Index(match, ";base64,")
		if prefixEnd < 0 {
			return match
		}
		prefixEnd += len(";base64,")
		if len(match)-prefixEnd <= base64PreviewChars {
			return match
		}
		return match[:prefixEnd+base64PreviewChars] + "...[truncated]"
	})
}

func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
