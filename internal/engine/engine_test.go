package engine

import (
	"testing"

	"mailflow/internal/config"
	"mailflow/internal/graph"
)

func TestMatchPatternsAndCaseInsensitive(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:            "match",
			From:            []string{"*@example.com"},
			To:              []string{"user@*"},
			FromDomain:      []string{"example.com"},
			ToDomain:        []string{"example.net"},
			SubjectContains: []string{"Important"},
			BodyContains:    []string{"secret"},
			CaseInsensitive: true,
		},
	}}

	msg := graph.Message{
		From:    "Sender@Example.com",
		To:      []string{"USER@EXAMPLE.NET"},
		Subject: "important notice",
		Body:    "Here is the Secret code",
	}

	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "match" {
		t.Fatalf("expected rule match, got %#v", rule)
	}
}

func TestMatchPatternWildcards(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "from wildcard", From: []string{"*@domain.com"}},
		{Name: "to wildcard", To: []string{"user@*"}},
	}}

	msg := graph.Message{From: "a@domain.com", To: []string{"user@else.com"}}

	if Match(&config.RuleSet{Rules: []config.Rule{rules.Rules[0]}}, msg, MatchOptions{}) == nil {
		t.Fatalf("expected from wildcard match")
	}
	if Match(&config.RuleSet{Rules: []config.Rule{rules.Rules[1]}}, msg, MatchOptions{}) == nil {
		t.Fatalf("expected to wildcard match")
	}
}

func TestMatchNoRulesAndNoMatch(t *testing.T) {
	if Match(&config.RuleSet{}, graph.Message{}, MatchOptions{}) != nil {
		t.Fatalf("expected nil match for empty rules")
	}

	rules := &config.RuleSet{Rules: []config.Rule{{Name: "from", From: []string{"a@b.com"}}}}
	msg := graph.Message{From: "other@b.com"}
	if Match(rules, msg, MatchOptions{}) != nil {
		t.Fatalf("expected no match")
	}
}

func TestMatchFirstWins(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "first", From: []string{"*@example.com"}},
		{Name: "second", From: []string{"user@example.com"}},
	}}

	msg := graph.Message{From: "user@example.com"}
	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "first" {
		t.Fatalf("expected first match, got %#v", rule)
	}
}

func TestMatchSubjectAndBodyContainsAny(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:            "contains",
			SubjectContains: []string{"alpha", "beta"},
			BodyContains:    []string{"one", "two"},
		},
	}}

	msg := graph.Message{Subject: "beta release", Body: "count to two"}
	if Match(rules, msg, MatchOptions{}) == nil {
		t.Fatalf("expected match for contains list")
	}
}

func TestBuildPushoverExtraction(t *testing.T) {
	msg := graph.Message{
		Subject:  "Your code",
		From:     "sender@example.com",
		Body:     "Your code is: 123456",
		BodyHTML: "<a href=\"https://example.com/reset\">Reset</a>",
	}

	cfg := &config.PushoverRule{
		Title:   "OTP for ${from}",
		Message: "Code: ${code}",
		Extract: []config.ExtractPattern{
			{Pattern: "not a match"},
			{Pattern: "code is: (\\d{6})", Capture: "code"},
		},
	}

	payload := BuildPushover(cfg, msg)
	if payload.Title != "OTP for sender@example.com" {
		t.Fatalf("unexpected title: %q", payload.Title)
	}
	if payload.Message != "Code: 123456" {
		t.Fatalf("unexpected message: %q", payload.Message)
	}
}

func TestBuildPushoverNamedCaptureAndFallback(t *testing.T) {
	msg := graph.Message{Subject: "Fallback subject", Body: "OTP: 654321"}

	cfg := &config.PushoverRule{
		Title:    "${subject}",
		Fallback: "No match", // used because Message empty
		Extract: []config.ExtractPattern{
			{Pattern: "OTP: (?P<code>\\d{6})"},
		},
	}

	payload := BuildPushover(cfg, msg)
	if payload.Message != "No match" {
		t.Fatalf("expected fallback message, got %q", payload.Message)
	}
}

func TestBuildPushoverHTMLExtraction(t *testing.T) {
	msg := graph.Message{
		Subject:  "Link",
		Body:     "plain text",
		BodyHTML: "<p>Click <a href=\"https://example.com/verify\">here</a></p>",
	}

	cfg := &config.PushoverRule{
		Message: "${link}",
		Extract: []config.ExtractPattern{
			{Pattern: "href=\\\"(?P<link>https://[^\\\"]+)\\\""},
		},
	}

	payload := BuildPushover(cfg, msg)
	if payload.Message != "https://example.com/verify" {
		t.Fatalf("expected link from html, got %q", payload.Message)
	}
}

func TestBuildPushoverDefaultMessage(t *testing.T) {
	msg := graph.Message{Subject: "Subject default"}
	cfg := &config.PushoverRule{}

	payload := BuildPushover(cfg, msg)
	if payload.Message != "Subject default" {
		t.Fatalf("expected subject fallback, got %q", payload.Message)
	}
}
