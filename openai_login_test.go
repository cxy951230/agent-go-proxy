package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestDecodeIDToken(t *testing.T) {
	claims := map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user-1",
			"chatgpt_account_id": "account-1",
			"chatgpt_plan_type":  "pro",
		},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	email, userID, accountID, plan := decodeIDToken(token)
	if email != "user@example.com" || userID != "user-1" || accountID != "account-1" || plan != "pro" {
		t.Fatalf("unexpected claims: %q %q %q %q", email, userID, accountID, plan)
	}
}

func TestRandomLoginID(t *testing.T) {
	first, err := randomLoginID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomLoginID()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 24 || first == second {
		t.Fatalf("unexpected login ids %q and %q", first, second)
	}
}
