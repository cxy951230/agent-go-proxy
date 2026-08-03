package main

import (
	"errors"
	"testing"
)

func TestHeroSMSCancelTerminal(t *testing.T) {
	cases := []struct {
		name string
		text string
		err  error
	}{
		{name: "activation not active text", text: `{"title":"ACTIVATION_NOT_ACTIVE","details":"Activation is terminated. Action not available."}`, err: errors.New("HeroSMS cancelActivation HTTP 409")},
		{name: "terminated text", text: "Activation is terminated. Action not available.", err: errors.New("HeroSMS cancelActivation HTTP 409")},
		{name: "empty 204", err: errors.New("HeroSMS cancelActivation HTTP 204: ")},
		{name: "legacy no activation", text: "NO_ACTIVATION", err: errors.New("HeroSMS cancelActivation HTTP 400")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !heroSMSCancelTerminal(tc.text, tc.err) {
				t.Fatalf("expected terminal for text=%q err=%v", tc.text, tc.err)
			}
		})
	}
}

func TestHeroSMSCancelTerminalFalseForRetryableError(t *testing.T) {
	if heroSMSCancelTerminal("", errors.New("HeroSMS cancelActivation HTTP 500: upstream timeout")) {
		t.Fatal("HTTP 500 should not be considered terminal")
	}
}
