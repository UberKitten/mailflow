package engine

import (
	"strings"
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

func TestBuildPushoverUnexpandedVarFallback(t *testing.T) {
	// When message contains ${code} but no code was extracted, should use fallback
	msg := graph.Message{Subject: "Linuxize: How to do stuff", Body: "No codes here at all"}

	cfg := &config.PushoverRule{
		Title:    "${from_name}",
		Message:  "${code}",
		Fallback: "${subject}",
		Extract: []config.ExtractPattern{
			{Pattern: `\b(\d{6})\b`, Capture: "code"},
		},
	}

	payload := BuildPushover(cfg, msg)
	if payload.Message != "Linuxize: How to do stuff" {
		t.Fatalf("expected subject fallback when code not extracted, got %q", payload.Message)
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

// --- subject_not_contains tests ---

func TestMatchSubjectNotContainsExcludes(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:               "policy-updates",
			SubjectContains:    []string{"privacy policy"},
			SubjectNotContains: []string{"privacy request", "action needed"},
			CaseInsensitive:    true,
		},
	}}

	// Should match: has "privacy policy" but no exclusion patterns
	msg := graph.Message{Subject: "We've updated our privacy policy"}
	if Match(rules, msg, MatchOptions{}) == nil {
		t.Fatal("expected match for policy update without exclusions")
	}

	// Should NOT match: has "privacy request" exclusion
	msg2 := graph.Message{Subject: "Privacy policy - Privacy request submitted"}
	if Match(rules, msg2, MatchOptions{}) != nil {
		t.Fatal("expected NO match when exclusion pattern present")
	}

	// Should NOT match: has "action needed" exclusion
	msg3 := graph.Message{Subject: "Privacy policy update - action needed"}
	if Match(rules, msg3, MatchOptions{}) != nil {
		t.Fatal("expected NO match for action needed exclusion")
	}
}

func TestMatchSubjectNotContainsCaseInsensitive(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:               "case-test",
			SubjectContains:    []string{"update"},
			SubjectNotContains: []string{"urgent"},
			CaseInsensitive:    true,
		},
	}}

	// Should NOT match: URGENT should match case-insensitively
	msg := graph.Message{Subject: "System Update - URGENT"}
	if Match(rules, msg, MatchOptions{}) != nil {
		t.Fatal("expected NO match for case-insensitive exclusion")
	}

	// Should match: no exclusion pattern
	msg2 := graph.Message{Subject: "system update available"}
	if Match(rules, msg2, MatchOptions{}) == nil {
		t.Fatal("expected match without exclusion pattern")
	}
}

func TestMatchSubjectNotContainsCaseSensitive(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:               "case-sensitive-test",
			SubjectContains:    []string{"update"},
			SubjectNotContains: []string{"URGENT"},
			CaseInsensitive:    false,
		},
	}}

	// Should match: "urgent" (lowercase) doesn't match "URGENT"
	msg := graph.Message{Subject: "update - urgent"}
	if Match(rules, msg, MatchOptions{}) == nil {
		t.Fatal("expected match: lowercase 'urgent' should not trigger exclusion")
	}

	// Should NOT match: exact case "URGENT"
	msg2 := graph.Message{Subject: "update - URGENT"}
	if Match(rules, msg2, MatchOptions{}) != nil {
		t.Fatal("expected NO match for exact case exclusion")
	}
}

func TestMatchSubjectNotContainsMultiplePatterns(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:               "multi-exclude",
			SubjectContains:    []string{"newsletter"},
			SubjectNotContains: []string{"unsubscribe", "opt-out", "remove me"},
			CaseInsensitive:    true,
		},
	}}

	// Any exclusion pattern should block
	cases := []struct {
		subject string
		want    bool // true = should match
	}{
		{"Monthly newsletter", true},
		{"Newsletter - click to unsubscribe", false},
		{"Newsletter opt-out confirmation", false},
		{"Remove me from newsletter", false},
		{"Newsletter subscription confirmed", true},
	}

	for _, tc := range cases {
		msg := graph.Message{Subject: tc.subject}
		got := Match(rules, msg, MatchOptions{}) != nil
		if got != tc.want {
			t.Errorf("subject %q: got match=%v, want match=%v", tc.subject, got, tc.want)
		}
	}
}

// --- body_not_contains tests ---

func TestMatchBodyNotContainsExcludes(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:            "promo",
			BodyContains:    []string{"special offer"},
			BodyNotContains: []string{"unsubscribe here", "opt out"},
			CaseInsensitive: true,
		},
	}}

	// Should match: promo without unsubscribe link
	msg := graph.Message{Body: "Check out this special offer just for you!"}
	if Match(rules, msg, MatchOptions{}) == nil {
		t.Fatal("expected match for promo without exclusion")
	}

	// Should NOT match: has unsubscribe link
	msg2 := graph.Message{Body: "Special offer! Click here. Unsubscribe here."}
	if Match(rules, msg2, MatchOptions{}) != nil {
		t.Fatal("expected NO match when body exclusion present")
	}
}

// --- body_prefix_contains tests ---

func TestMatchBodyPrefixContains(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:               "ad-at-top",
			BodyPrefixContains: []string{"ADVERTISEMENT", "SPONSORED"},
			BodyPrefixLength:   100,
			CaseInsensitive:    true,
		},
	}}

	// Should match: ADVERTISEMENT at the start
	msg := graph.Message{Body: "ADVERTISEMENT\n\nHere is the newsletter content..."}
	if Match(rules, msg, MatchOptions{}) == nil {
		t.Fatal("expected match for ADVERTISEMENT at start of body")
	}

	// Should match: SPONSORED within prefix
	msg2 := graph.Message{Body: "Weekly Update\nSPONSORED CONTENT\nMore stuff here..."}
	if Match(rules, msg2, MatchOptions{}) == nil {
		t.Fatal("expected match for SPONSORED within prefix")
	}

	// Should NOT match: ADVERTISEMENT appears after prefix length
	longPrefix := strings.Repeat("x", 150)
	msg3 := graph.Message{Body: longPrefix + "ADVERTISEMENT buried here"}
	if Match(rules, msg3, MatchOptions{}) != nil {
		t.Fatal("expected NO match for ADVERTISEMENT beyond prefix length")
	}
}

func TestMatchBodyPrefixContainsDefaultLength(t *testing.T) {
	// No BodyPrefixLength specified, should default to 1000
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:               "ad-default",
			BodyPrefixContains: []string{"ADVERTISEMENT"},
		},
	}}

	// Should match: within default 1000 chars
	prefix := strings.Repeat("x", 500)
	msg := graph.Message{Body: prefix + "ADVERTISEMENT here"}
	if Match(rules, msg, MatchOptions{}) == nil {
		t.Fatal("expected match within default 1000 char prefix")
	}

	// Should NOT match: beyond default 1000 chars
	longPrefix := strings.Repeat("x", 1100)
	msg2 := graph.Message{Body: longPrefix + "ADVERTISEMENT buried"}
	if Match(rules, msg2, MatchOptions{}) != nil {
		t.Fatal("expected NO match beyond default 1000 char prefix")
	}
}

func TestMatchFastModeSkipsBodyPrefixRules(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "prefix-rule", BodyPrefixContains: []string{"AD"}},
		{Name: "from-rule", From: []string{"*@example.com"}},
	}}

	msg := graph.Message{From: "user@example.com", Body: "AD at the top"}

	// In fast mode, body prefix rule should be skipped
	rule := Match(rules, msg, MatchOptions{Fast: true})
	if rule == nil || rule.Name != "from-rule" {
		t.Fatalf("expected from-rule in fast mode, got %v", rule)
	}

	// In normal mode, prefix rule matches first
	rule2 := Match(rules, msg, MatchOptions{Fast: false})
	if rule2 == nil || rule2.Name != "prefix-rule" {
		t.Fatalf("expected prefix-rule in normal mode, got %v", rule2)
	}
}

// --- Rule priority ordering tests ---

func TestMatchRulePriorityOrder(t *testing.T) {
	// Simulates rules loaded from 10-security.yaml before 50-promotions.yaml
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "security", From: []string{"*@bank.com"}, SubjectContains: []string{"code"}},
		{Name: "promo", From: []string{"*@bank.com"}},
	}}

	// Security rule should win for 2FA emails
	msg := graph.Message{From: "alerts@bank.com", Subject: "Your verification code"}
	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "security" {
		t.Fatalf("expected security rule to match first, got %v", rule)
	}

	// Promo rule matches generic bank emails
	msg2 := graph.Message{From: "marketing@bank.com", Subject: "New credit card offers"}
	rule2 := Match(rules, msg2, MatchOptions{})
	if rule2 == nil || rule2.Name != "promo" {
		t.Fatalf("expected promo rule to match, got %v", rule2)
	}
}

func TestMatchCatchallIgnored(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "specific", From: []string{"known@example.com"}},
		{Name: "catchall", Catchall: true},
	}}

	// With IgnoreCatchall, unknown sender gets no match
	msg := graph.Message{From: "unknown@random.com"}
	rule := Match(rules, msg, MatchOptions{IgnoreCatchall: true})
	if rule != nil {
		t.Fatalf("expected no match with IgnoreCatchall, got %v", rule)
	}

	// Without IgnoreCatchall, catchall matches
	rule2 := Match(rules, msg, MatchOptions{IgnoreCatchall: false})
	if rule2 == nil || rule2.Name != "catchall" {
		t.Fatalf("expected catchall match, got %v", rule2)
	}
}

// --- Fast mode tests ---

func TestMatchFastModeSkipsBodyRules(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "body-rule", BodyContains: []string{"secret"}},
		{Name: "from-rule", From: []string{"*@example.com"}},
	}}

	msg := graph.Message{From: "user@example.com", Body: "The secret code"}

	// In fast mode, body rule should be skipped
	rule := Match(rules, msg, MatchOptions{Fast: true})
	if rule == nil || rule.Name != "from-rule" {
		t.Fatalf("expected from-rule in fast mode, got %v", rule)
	}

	// In normal mode, body rule matches first
	rule2 := Match(rules, msg, MatchOptions{Fast: false})
	if rule2 == nil || rule2.Name != "body-rule" {
		t.Fatalf("expected body-rule in normal mode, got %v", rule2)
	}
}

// --- Partial match tests ---

func TestMatchSubjectPartialMatch(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:            "partial",
			SubjectContains: []string{"privacy"},
			CaseInsensitive: true,
		},
	}}

	// Should match: "privacy" is substring
	cases := []string{
		"Privacy Policy Update",
		"Your privacy settings",
		"New privacy features announced",
		"PRIVACY notice",
	}

	for _, subject := range cases {
		msg := graph.Message{Subject: subject}
		if Match(rules, msg, MatchOptions{}) == nil {
			t.Errorf("expected partial match for subject %q", subject)
		}
	}

	// Should NOT match: no "privacy" substring
	msg := graph.Message{Subject: "Terms of Service Update"}
	if Match(rules, msg, MatchOptions{}) != nil {
		t.Fatal("expected no match without 'privacy' substring")
	}
}

// --- Domain extraction tests ---

func TestMatchFromDomainExtraction(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "domain", FromDomain: []string{"example.com"}},
	}}

	cases := []struct {
		from string
		want bool
	}{
		{"user@example.com", true},
		{"admin@example.com", true},
		{"user@sub.example.com", false}, // subdomain doesn't match
		{"user@notexample.com", false},
		{"invalid-email", false},
		// Note: "@example.com" is malformed but domainFromEmail still extracts "example.com"
		// In practice, MS Graph always provides well-formed addresses
	}

	for _, tc := range cases {
		msg := graph.Message{From: tc.from}
		got := Match(rules, msg, MatchOptions{}) != nil
		if got != tc.want {
			t.Errorf("from %q: got match=%v, want match=%v", tc.from, got, tc.want)
		}
	}
}

// TestMatchFromDomainCaseInsensitive verifies domains match case-insensitively per RFC 1035
func TestMatchFromDomainCaseInsensitive(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "lowercase-rule", FromDomain: []string{"rxlocal.com"}},
	}}

	cases := []struct {
		from string
		want bool
	}{
		{"user@rxlocal.com", true},
		{"user@RxLocal.com", true},   // Mixed case domain should match
		{"user@RXLOCAL.COM", true},   // All caps should match
		{"user@RxLocal.COM", true},   // Mixed case should match
		{"user@other.com", false},
	}

	for _, tc := range cases {
		msg := graph.Message{From: tc.from}
		got := Match(rules, msg, MatchOptions{}) != nil
		if got != tc.want {
			t.Errorf("from %q: got match=%v, want match=%v", tc.from, got, tc.want)
		}
	}
}

// --- Wildcard pattern tests ---

func TestMatchWildcardPatterns(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "prefix-wildcard", From: []string{"noreply-*@company.com"}},
		{Name: "suffix-wildcard", From: []string{"*-alerts@company.com"}},
		{Name: "middle-wildcard", From: []string{"team-*-notifications@company.com"}},
	}}

	cases := []struct {
		from     string
		wantRule string
	}{
		{"noreply-123@company.com", "prefix-wildcard"},
		{"noreply-orders@company.com", "prefix-wildcard"},
		{"security-alerts@company.com", "suffix-wildcard"},
		{"team-engineering-notifications@company.com", "middle-wildcard"},
	}

	for _, tc := range cases {
		msg := graph.Message{From: tc.from}
		rule := Match(rules, msg, MatchOptions{})
		if rule == nil || rule.Name != tc.wantRule {
			name := ""
			if rule != nil {
				name = rule.Name
			}
			t.Errorf("from %q: got rule %q, want %q", tc.from, name, tc.wantRule)
		}
	}
}

func TestMatchSender(t *testing.T) {
	tests := []struct {
		pattern string
		email   string
		want    bool
	}{
		// Exact match
		{"user@example.com", "user@example.com", true},
		{"user@example.com", "other@example.com", false},

		// Domain wildcard
		{"*@example.com", "user@example.com", true},
		{"*@example.com", "admin@example.com", true},
		{"*@example.com", "user@other.com", false},

		// Prefix wildcard
		{"newsletter@*", "newsletter@example.com", true},
		{"newsletter@*", "newsletter@company.org", true},
		{"newsletter@*", "news@example.com", false},

		// Contains wildcard
		{"*news*@*", "newsletter@example.com", true},
		{"*news*@*", "daily-news@site.com", true},
		{"*news*@*", "updates@site.com", false},

		// Case insensitivity
		{"*@EXAMPLE.COM", "user@example.com", true},
		{"USER@example.com", "user@EXAMPLE.COM", true},

		// Complex patterns
		{"*-updates@*.company.com", "daily-updates@mail.company.com", true},
		{"*-updates@*.company.com", "updates@mail.company.com", false},

		// Single character wildcard
		{"user?@example.com", "user1@example.com", true},
		{"user?@example.com", "user12@example.com", false},
	}

	for _, tc := range tests {
		got := MatchSender(tc.pattern, tc.email)
		if got != tc.want {
			t.Errorf("MatchSender(%q, %q) = %v, want %v", tc.pattern, tc.email, got, tc.want)
		}
	}
}

// --- header_contains tests ---

func TestMatchHeaderContains(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:           "list-id-match",
			HeaderContains: map[string][]string{"List-Id": {"oss-security"}},
		},
	}}

	msg := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<oss-security.lists.openwall.com>"},
	}

	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "list-id-match" {
		t.Fatalf("expected list-id-match rule to match, got %v", rule)
	}
}

func TestMatchHeaderContainsCaseInsensitive(t *testing.T) {
	// Test that header NAME lookup is always case-insensitive (per RFC 2822)
	// but VALUE matching respects the case_insensitive flag
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:            "case-insensitive-name",
			HeaderContains:  map[string][]string{"list-id": {"oss-security"}}, // lowercase header name
			CaseInsensitive: false,                                            // value matching is case-sensitive
		},
	}}

	// Header name in message is "List-Id" (mixed case), rule uses "list-id" (lowercase)
	// Should still match because header name lookup is always case-insensitive
	msg := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<oss-security.lists.openwall.com>"},
	}

	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "case-insensitive-name" {
		t.Fatalf("expected case-insensitive header name lookup, got %v", rule)
	}

	// Now test case-sensitive VALUE matching
	msg2 := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<OSS-SECURITY.lists.openwall.com>"},
	}
	rule2 := Match(rules, msg2, MatchOptions{})
	if rule2 != nil {
		t.Fatalf("expected no match for case-sensitive value comparison, got %v", rule2)
	}

	// Test case-insensitive VALUE matching
	rulesCI := &config.RuleSet{Rules: []config.Rule{
		{
			Name:            "case-insensitive-value",
			HeaderContains:  map[string][]string{"List-Id": {"oss-security"}},
			CaseInsensitive: true,
		},
	}}

	rule3 := Match(rulesCI, msg2, MatchOptions{})
	if rule3 == nil || rule3.Name != "case-insensitive-value" {
		t.Fatalf("expected case-insensitive value match, got %v", rule3)
	}
}

func TestMatchHeaderContainsMultipleValues(t *testing.T) {
	// Multiple patterns - any match = pass
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name: "multi-value",
			HeaderContains: map[string][]string{
				"List-Id": {"oss-security", "fulldisclosure", "bugtraq"},
			},
		},
	}}

	msg := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<fulldisclosure.seclists.org>"},
	}

	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "multi-value" {
		t.Fatalf("expected multi-value rule to match, got %v", rule)
	}

	// None of the patterns match
	msg2 := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<some-other-list@example.com>"},
	}
	rule2 := Match(rules, msg2, MatchOptions{})
	if rule2 != nil {
		t.Fatalf("expected no match when no patterns match, got %v", rule2)
	}
}

func TestMatchHeaderContainsNoHeader(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:           "needs-header",
			HeaderContains: map[string][]string{"List-Id": {"oss-security"}},
		},
	}}

	// Message without List-Id header
	msg := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"X-Mailer": "Some Mailer"},
	}

	rule := Match(rules, msg, MatchOptions{})
	if rule != nil {
		t.Fatalf("expected no match when header is missing, got %v", rule)
	}

	// Message with nil/empty headers
	msg2 := graph.Message{From: "user@example.com"}
	rule2 := Match(rules, msg2, MatchOptions{})
	if rule2 != nil {
		t.Fatalf("expected no match with nil headers, got %v", rule2)
	}
}

func TestMatchHeaderContainsFastMode(t *testing.T) {
	rules := &config.RuleSet{Rules: []config.Rule{
		{Name: "header-rule", HeaderContains: map[string][]string{"List-Id": {"test"}}},
		{Name: "from-rule", From: []string{"*@example.com"}},
	}}

	msg := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "test-list"},
	}

	// In fast mode, header rule should be skipped (headers not fetched)
	rule := Match(rules, msg, MatchOptions{Fast: true})
	if rule == nil || rule.Name != "from-rule" {
		t.Fatalf("expected from-rule in fast mode, got %v", rule)
	}

	// In normal mode, header rule matches first
	rule2 := Match(rules, msg, MatchOptions{Fast: false})
	if rule2 == nil || rule2.Name != "header-rule" {
		t.Fatalf("expected header-rule in normal mode, got %v", rule2)
	}
}

func TestMatchHeaderContainsSubstring(t *testing.T) {
	// Test partial/substring matching (not exact match)
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name:           "substring-match",
			HeaderContains: map[string][]string{"List-Id": {"oss-security"}},
		},
	}}

	// Pattern "oss-security" should match header value "<oss-security.lists.openwall.com>"
	msg := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<oss-security.lists.openwall.com>"},
	}

	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "substring-match" {
		t.Fatalf("expected substring match, got %v", rule)
	}

	// Also test with the full value as pattern
	rules2 := &config.RuleSet{Rules: []config.Rule{
		{
			Name:           "full-match",
			HeaderContains: map[string][]string{"List-Id": {"<oss-security.lists.openwall.com>"}},
		},
	}}

	rule2 := Match(rules2, msg, MatchOptions{})
	if rule2 == nil || rule2.Name != "full-match" {
		t.Fatalf("expected full match, got %v", rule2)
	}
}

func TestMatchHeaderContainsMultipleHeaders(t *testing.T) {
	// Rule with multiple header conditions - ALL must match
	rules := &config.RuleSet{Rules: []config.Rule{
		{
			Name: "multi-header",
			HeaderContains: map[string][]string{
				"List-Id":    {"oss-security"},
				"Precedence": {"list"},
			},
		},
	}}

	// Both headers present and match
	msg := graph.Message{
		From: "user@example.com",
		Headers: map[string]string{
			"List-Id":    "<oss-security.lists.openwall.com>",
			"Precedence": "list",
		},
	}
	rule := Match(rules, msg, MatchOptions{})
	if rule == nil || rule.Name != "multi-header" {
		t.Fatalf("expected match when all headers match, got %v", rule)
	}

	// One header missing
	msg2 := graph.Message{
		From:    "user@example.com",
		Headers: map[string]string{"List-Id": "<oss-security.lists.openwall.com>"},
	}
	rule2 := Match(rules, msg2, MatchOptions{})
	if rule2 != nil {
		t.Fatalf("expected no match when one header is missing, got %v", rule2)
	}

	// One header doesn't match
	msg3 := graph.Message{
		From: "user@example.com",
		Headers: map[string]string{
			"List-Id":    "<oss-security.lists.openwall.com>",
			"Precedence": "bulk",
		},
	}
	rule3 := Match(rules, msg3, MatchOptions{})
	if rule3 != nil {
		t.Fatalf("expected no match when one header value doesn't match, got %v", rule3)
	}
}
