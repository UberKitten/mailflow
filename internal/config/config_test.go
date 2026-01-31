package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func TestLoadMainConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "graph: {}\n")

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	if cfg.Graph.BaseURL == "" || !strings.Contains(cfg.Graph.BaseURL, "graph.microsoft.com") {
		t.Fatalf("expected default base_url, got %q", cfg.Graph.BaseURL)
	}
	if cfg.Graph.TokenScript == "" {
		t.Fatalf("expected default token_script")
	}
}

func TestLoadSendersMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	senders, err := loadSenders(dir)
	if err != nil {
		t.Fatalf("loadSenders: %v", err)
	}
	if len(senders) != 0 {
		t.Fatalf("expected empty senders, got %d", len(senders))
	}
}

func TestLoadRulesResolvesRefsAndOrdersFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "senders.d", "corp.yaml"), "name: corp\ndomains:\n  - corp.com\naddresses:\n  - notify@corp.com\n")

	writeFile(t, filepath.Join(dir, "rules.d", "b.yaml"), `version: 1
rules:
  - name: B rule
    folder: Inbox/Corp
    from_domain: !ref corp
`)

	writeFile(t, filepath.Join(dir, "rules.d", "a.yaml"), `version: 1
rules:
  - name: A rule
    folder: Inbox/Corp
    from: user@example.com
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	senders, err := loadSenders(dir)
	if err != nil {
		t.Fatalf("loadSenders: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, senders)
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	if len(ruleset.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(ruleset.Rules))
	}
	if ruleset.Rules[0].Name != "A rule" {
		t.Fatalf("expected alphabetical order, got first=%q", ruleset.Rules[0].Name)
	}

	refRule := ruleset.Rules[1]
	if len(refRule.FromDomain) != 1 || refRule.FromDomain[0] != "corp.com" {
		t.Fatalf("expected resolved from_domain, got %#v", refRule.FromDomain)
	}
	if len(refRule.From) != 1 || refRule.From[0] != "notify@corp.com" {
		t.Fatalf("expected resolved from addresses, got %#v", refRule.From)
	}
}

func TestLoadRulesFolderSectionAddsFolderPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
folders:
  alerts:
    path: Inbox/Alerts
    rules:
      - name: Alert rule
        from: alerts@example.com
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}
	if ruleset.Rules[0].Folder != "Inbox/Alerts" {
		t.Fatalf("expected folder path injected, got %q", ruleset.Rules[0].Folder)
	}
	if ruleset.Folders["alerts"] != "Inbox/Alerts" {
		t.Fatalf("expected folders map entry, got %#v", ruleset.Folders)
	}
}

func TestLoadRulesErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(dir string)
		wantErr string
	}{
		{
			name: "missing rules files",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")
			},
			wantErr: "no rules files found",
		},
		{
			name: "missing folder in flat rule",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")
				writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: Missing folder
    from: user@example.com
`)
			},
			wantErr: "rule missing folder",
		},
		{
			name: "unknown sender ref",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")
				writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: Ref rule
    folder: Inbox
    from_domain: !ref missing
`)
			},
			wantErr: "unknown sender ref",
		},
		{
			name: "invalid rules yaml",
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")
				writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), "not: [valid")
			},
			wantErr: "parse rules file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(dir)
			cfg, err := loadMainConfig(dir)
			if err != nil {
				t.Fatalf("loadMainConfig: %v", err)
			}
			_, err = loadRules(dir, cfg, map[string]SenderList{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadMainConfigMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := loadMainConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "read config.yaml") {
		t.Fatalf("expected missing file error, got %v", err)
	}
}

// --- subject_not_contains and body_not_contains YAML parsing tests ---

func TestLoadRulesSubjectNotContains(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: policy-updates
    folder: Archive
    subject_contains:
      - "privacy policy"
      - "terms of service"
    subject_not_contains:
      - "privacy request"
      - "action needed"
    case_insensitive: true
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	if len(ruleset.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(ruleset.Rules))
	}

	rule := ruleset.Rules[0]
	if len(rule.SubjectContains) != 2 {
		t.Fatalf("expected 2 subject_contains, got %d", len(rule.SubjectContains))
	}
	if len(rule.SubjectNotContains) != 2 {
		t.Fatalf("expected 2 subject_not_contains, got %d", len(rule.SubjectNotContains))
	}
	if rule.SubjectNotContains[0] != "privacy request" {
		t.Fatalf("unexpected subject_not_contains[0]: %q", rule.SubjectNotContains[0])
	}
	if !rule.CaseInsensitive {
		t.Fatal("expected case_insensitive=true")
	}
}

func TestLoadRulesBodyNotContains(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: newsletter
    folder: Inbox/Posts
    body_contains:
      - "newsletter"
    body_not_contains:
      - "unsubscribe"
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	rule := ruleset.Rules[0]
	if len(rule.BodyNotContains) != 1 {
		t.Fatalf("expected 1 body_not_contains, got %d", len(rule.BodyNotContains))
	}
	if rule.BodyNotContains[0] != "unsubscribe" {
		t.Fatalf("unexpected body_not_contains[0]: %q", rule.BodyNotContains[0])
	}
}

func TestLoadRulesNotContainsSingleValue(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	// Test single string value (not array)
	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: single-value-test
    folder: Archive
    subject_not_contains: "urgent"
    body_not_contains: "spam"
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	rule := ruleset.Rules[0]
	if len(rule.SubjectNotContains) != 1 || rule.SubjectNotContains[0] != "urgent" {
		t.Fatalf("expected single subject_not_contains='urgent', got %v", rule.SubjectNotContains)
	}
	if len(rule.BodyNotContains) != 1 || rule.BodyNotContains[0] != "spam" {
		t.Fatalf("expected single body_not_contains='spam', got %v", rule.BodyNotContains)
	}
}

// --- Sender list loading tests ---

func TestLoadSendersMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "senders.d", "orders.yaml"), `name: orders
domains:
  - amazon.com
  - ebay.com
addresses:
  - orders@shop.com
`)

	writeFile(t, filepath.Join(dir, "senders.d", "security.yaml"), `name: security
domains:
  - google.com
addresses:
  - security@apple.com
  - noreply@microsoft.com
`)

	senders, err := loadSenders(dir)
	if err != nil {
		t.Fatalf("loadSenders: %v", err)
	}

	if len(senders) != 2 {
		t.Fatalf("expected 2 sender lists, got %d", len(senders))
	}

	orders := senders["orders"]
	if len(orders.Domains) != 2 {
		t.Fatalf("expected 2 domains in orders, got %d", len(orders.Domains))
	}
	if len(orders.Addresses) != 1 {
		t.Fatalf("expected 1 address in orders, got %d", len(orders.Addresses))
	}

	security := senders["security"]
	if len(security.Addresses) != 2 {
		t.Fatalf("expected 2 addresses in security, got %d", len(security.Addresses))
	}
}

func TestLoadSendersFallbackToFilename(t *testing.T) {
	dir := t.TempDir()

	// No "name" field - should use filename as key
	writeFile(t, filepath.Join(dir, "senders.d", "health.yaml"), `domains:
  - hospital.com
  - pharmacy.com
`)

	senders, err := loadSenders(dir)
	if err != nil {
		t.Fatalf("loadSenders: %v", err)
	}

	// Should be keyed by filename "health"
	health, ok := senders["health"]
	if !ok {
		t.Fatalf("expected sender list keyed by 'health', got keys: %v", senders)
	}
	if len(health.Domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(health.Domains))
	}
}

func TestLoadRulesRefResolvesDomainsAndAddresses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "senders.d", "health.yaml"), `name: health
domains:
  - hospital.com
  - clinic.org
addresses:
  - appointments@healthsystem.com
`)

	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: health-emails
    folder: Inbox/Health
    from_domain: !ref health
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	senders, err := loadSenders(dir)
	if err != nil {
		t.Fatalf("loadSenders: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, senders)
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	rule := ruleset.Rules[0]

	// !ref should resolve both domains AND addresses
	if len(rule.FromDomain) != 2 {
		t.Fatalf("expected 2 from_domain, got %d: %v", len(rule.FromDomain), rule.FromDomain)
	}
	if len(rule.From) != 1 {
		t.Fatalf("expected 1 from (from addresses), got %d: %v", len(rule.From), rule.From)
	}
	if rule.From[0] != "appointments@healthsystem.com" {
		t.Fatalf("unexpected from[0]: %q", rule.From[0])
	}
}

func TestLoadRulesToDomainRef(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "senders.d", "rss.yaml"), `name: rss
domains:
  - rss.uberkitten.com
addresses:
  - rss@uberkitten.com
`)

	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: rss-feeds
    folder: Inbox/Posts
    to_domain: !ref rss
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	senders, err := loadSenders(dir)
	if err != nil {
		t.Fatalf("loadSenders: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, senders)
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	rule := ruleset.Rules[0]
	if len(rule.ToDomain) != 1 || rule.ToDomain[0] != "rss.uberkitten.com" {
		t.Fatalf("expected to_domain resolved, got %v", rule.ToDomain)
	}
	if len(rule.To) != 1 || rule.To[0] != "rss@uberkitten.com" {
		t.Fatalf("expected to addresses resolved, got %v", rule.To)
	}
}

// --- Rule file ordering tests ---

func TestLoadRulesFilePriority(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	// Simulate priority-based naming: 10-security, 50-promo, 99-catchall
	writeFile(t, filepath.Join(dir, "rules.d", "50-promo.yaml"), `version: 1
rules:
  - name: promo
    folder: Inbox/Promo
    from: promo@example.com
`)

	writeFile(t, filepath.Join(dir, "rules.d", "10-security.yaml"), `version: 1
rules:
  - name: security
    folder: Inbox/Security
    from: security@example.com
`)

	writeFile(t, filepath.Join(dir, "rules.d", "99-catchall.yaml"), `version: 1
rules:
  - name: catchall
    folder: Inbox
    catchall: true
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	// Files should be sorted alphabetically (10 < 50 < 99)
	if len(ruleset.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(ruleset.Rules))
	}
	if ruleset.Rules[0].Name != "security" {
		t.Fatalf("expected security first, got %q", ruleset.Rules[0].Name)
	}
	if ruleset.Rules[1].Name != "promo" {
		t.Fatalf("expected promo second, got %q", ruleset.Rules[1].Name)
	}
	if ruleset.Rules[2].Name != "catchall" {
		t.Fatalf("expected catchall last, got %q", ruleset.Rules[2].Name)
	}
}

// --- YAML edge cases ---

func TestLoadRulesSubjectContainsAny(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	// Test subject_contains_any alias
	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: aliases
    folder: Archive
    subject_contains_any:
      - "pattern1"
      - "pattern2"
    body_contains_any:
      - "body1"
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	rule := ruleset.Rules[0]
	if len(rule.SubjectContains) != 2 {
		t.Fatalf("expected 2 subject_contains from _any alias, got %d", len(rule.SubjectContains))
	}
	if len(rule.BodyContains) != 1 {
		t.Fatalf("expected 1 body_contains from _any alias, got %d", len(rule.BodyContains))
	}
}

func TestLoadRulesOnMatchMarkRead(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "include:\n  - rules.d/*.yaml\n")

	writeFile(t, filepath.Join(dir, "rules.d", "rules.yaml"), `version: 1
rules:
  - name: auto-archive
    folder: Archive
    from: noreply@example.com
    on_match:
      mark_read: true
`)

	cfg, err := loadMainConfig(dir)
	if err != nil {
		t.Fatalf("loadMainConfig: %v", err)
	}
	ruleset, err := loadRules(dir, cfg, map[string]SenderList{})
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	rule := ruleset.Rules[0]
	if rule.OnMatch == nil {
		t.Fatal("expected on_match to be parsed")
	}
	if !rule.OnMatch.MarkRead {
		t.Fatal("expected mark_read=true")
	}
}
