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

func TestValidateRuleSetBroadOverlapWarning(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/10-specific.yaml", `version: 1
rules:
  - name: specific
    folder: Inbox/Posts
    from: author@corp.com
`)

	writeFile(t, dir+"/rules.d/20-broad.yaml", `version: 1
rules:
  - name: broad
    folder: Inbox/Posts
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
}

func TestValidateRuleSetCleanConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/config.yaml", "include:\n  - rules.d/*.yaml\n")

	writeFile(t, dir+"/rules.d/10-specific.yaml", `version: 1
rules:
  - name: specific
    folder: Inbox/Posts
    from: author@corp.com
`)

	writeFile(t, dir+"/rules.d/20-broad.yaml", `version: 1
rules:
  - name: broad
    folder: Inbox/Posts
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
    folder: Inbox/Posts
    from: author@corp.com
`)

	writeFile(t, dir+"/rules.d/20-broad.yaml", `version: 1
rules:
  - name: broad
    folder: Inbox/Posts
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
