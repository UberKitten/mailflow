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
