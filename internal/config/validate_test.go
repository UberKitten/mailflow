package config

import (
	"strings"
	"testing"
)

func TestValidateRuleSetDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/10-first.yaml", `version: 1
rules:
  - name: dup-rule
    folder: Inbox
    from: first@example.com
`)

	writeFile(t, dir+"/rules.d/20-second.yaml", `version: 1
rules:
  - name: dup-rule
    folder: Inbox
    from: second@example.com
`)

	_, rules, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	report := ValidateRuleSet(rules, ValidateOptions{})
	if len(report.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(report.Errors))
	}
	if !strings.Contains(report.Errors[0].Message, "dup-rule") {
		t.Fatalf("expected error message to mention duplicate rule, got %q", report.Errors[0].Message)
	}
}

func TestValidateRuleSetBroadOverlapWarningCatchall(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/10-specific.yaml", `version: 1
rules:
  - name: specific
    folder: Posts/Science
    from: author@corp.com
`)

	// Catch-all rule with from_domain matching the specific rule's domain
	writeFile(t, dir+"/rules.d/45-catchall.yaml", `version: 1
rules:
  - name: posts-catchall
    folder: Posts
    catchall: true
    from_domain:
      - corp.com
`)

	_, rules, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	report := ValidateRuleSet(rules, ValidateOptions{})
	if len(report.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(report.Warnings))
	}
	if !strings.Contains(report.Warnings[0].Message, "corp.com") {
		t.Fatalf("expected warning message to mention domain, got %q", report.Warnings[0].Message)
	}
	if !strings.Contains(report.Warnings[0].Message, "catch-all") {
		t.Fatalf("expected warning to mention catch-all, got %q", report.Warnings[0].Message)
	}
}

func TestValidateRuleSetNonCatchallOverlapNoWarning(t *testing.T) {
	// Normal priority ordering: specific address rule in 08, broader domain rule in 10.
	// This is the whole point of the priority system — should NOT warn.
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/08-overrides.yaml", `version: 1
rules:
  - name: specific-override
    folder: Promotions
    from: no-reply@github.com
`)

	writeFile(t, dir+"/rules.d/10-security.yaml", `version: 1
rules:
  - name: security-senders
    folder: Inbox/Security
    from_domain:
      - github.com
`)

	_, rules, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	report := ValidateRuleSet(rules, ValidateOptions{})
	if len(report.Warnings) != 0 {
		t.Fatalf("expected no warnings for normal priority overlap, got %d: %v", len(report.Warnings), report.Warnings)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("expected no errors, got %d", len(report.Errors))
	}
}

func TestValidateRuleSetCleanConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/10-specific.yaml", `version: 1
rules:
  - name: specific
    folder: Posts
    from: author@corp.com
`)

	writeFile(t, dir+"/rules.d/20-broad.yaml", `version: 1
rules:
  - name: broad
    folder: Posts
    from_domain:
      - other.com
`)

	_, rules, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	report := ValidateRuleSet(rules, ValidateOptions{})
	if len(report.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %d", len(report.Warnings))
	}
	if len(report.Errors) != 0 {
		t.Fatalf("expected no errors, got %d", len(report.Errors))
	}
}

func TestValidateRuleSetStrictMode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/10-specific.yaml", `version: 1
rules:
  - name: specific
    folder: Posts/Science
    from: author@corp.com
`)

	// Catch-all overlap triggers warning, strict promotes to error
	writeFile(t, dir+"/rules.d/45-catchall.yaml", `version: 1
rules:
  - name: posts-catchall
    folder: Posts
    catchall: true
    from_domain:
      - corp.com
`)

	_, rules, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	report := ValidateRuleSet(rules, ValidateOptions{})
	report.ApplyStrict()
	if len(report.Errors) != 1 {
		t.Fatalf("expected 1 error after strict, got %d", len(report.Errors))
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("expected warnings cleared after strict, got %d", len(report.Warnings))
	}
}
