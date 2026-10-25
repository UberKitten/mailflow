package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const incomingConfigFixture = `graph:
  auth_mode: cert
  client_id: fixture-client
  tenant_id: fixture-tenant
  cert_path: /not-opened/fixture.pfx
webhook:
  enabled: true
  path: /webhook
  external_url: https://notify.example/webhook
incoming_review:
  mailbox: review@example.com
  mailbox_object_id: 11111111-2222-3333-4444-555555555555
  path: /incoming-review
  external_url: https://notify.example/incoming-review
  spool_dir: .incoming-review
`

func TestIncomingReviewConfigValidation(t *testing.T) {
	cases := map[string][2]string{
		"missing mailbox":        {"mailbox: review@example.com", "mailbox: ''"},
		"invalid mailbox":        {"mailbox: review@example.com", "mailbox: not-an-address"},
		"display name":           {"mailbox: review@example.com", "mailbox: Review <review@example.com>"},
		"multiple mailboxes":     {"mailbox: review@example.com", "mailbox: review@example.com, primary@example.com"},
		"mailbox injection":      {"mailbox: review@example.com", `mailbox: 'review@example.com","message_id":"forged'`},
		"missing mailbox object": {"mailbox_object_id: 11111111-2222-3333-4444-555555555555", "mailbox_object_id: ''"},
		"invalid mailbox object": {"mailbox_object_id: 11111111-2222-3333-4444-555555555555", "mailbox_object_id: review@example.com"},
		"nil mailbox object":     {"mailbox_object_id: 11111111-2222-3333-4444-555555555555", "mailbox_object_id: 00000000-0000-0000-0000-000000000000"},
		"disabled receiver":      {"enabled: true", "enabled: false"},
		"delegated auth":         {"auth_mode: cert", "auth_mode: delegated"},
		"missing client":         {"client_id: fixture-client", "client_id: ''"},
		"missing tenant":         {"tenant_id: fixture-tenant", "tenant_id: ''"},
		"missing certificate":    {"cert_path: /not-opened/fixture.pfx", "cert_path: ''"},
		"primary path":           {"path: /incoming-review", "path: /webhook"},
		"health path":            {"path: /incoming-review", "path: /health"},
		"wildcard path":          {"path: /incoming-review", "path: /{mailbox}"},
		"cross origin":           {"external_url: https://notify.example/incoming-review", "external_url: https://other.example/incoming-review"},
		"different path":         {"external_url: https://notify.example/incoming-review", "external_url: https://notify.example/other"},
		"query string":           {"external_url: https://notify.example/incoming-review", "external_url: https://notify.example/incoming-review?secret=x"},
		"insecure origin":        {"https://notify.example", "http://notify.example"},
		"missing spool":          {"spool_dir: .incoming-review", "spool_dir: ''"},
		"outside subtree":        {"spool_dir: .incoming-review", "spool_dir: ../outside"},
		"unknown field":          {"spool_dir: .incoming-review", "spool_dir: .incoming-review\n  fetch_body: true"},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			data := strings.ReplaceAll(incomingConfigFixture, change[0], change[1])
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadMainConfig(dir); err == nil {
				t.Fatal("invalid incoming review config accepted")
			}
		})
	}
}

func TestIncomingReviewConfigResolvesPrivateSpoolWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(incomingConfigFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadMainConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IncomingReview.SpoolDir != filepath.Join(dir, ".incoming-review") {
		t.Fatalf("spool directory = %q", cfg.IncomingReview.SpoolDir)
	}
	if _, err := os.Stat(cfg.IncomingReview.SpoolDir); !os.IsNotExist(err) {
		t.Fatalf("config loading created spool state: %v", err)
	}
}

func TestIncomingReviewConfigNormalizesConfiguredMailbox(t *testing.T) {
	for _, mailbox := range []string{"Review@EXAMPLE.COM", "secondary@example.com", " primary@example.com "} {
		t.Run(mailbox, func(t *testing.T) {
			dir := t.TempDir()
			data := strings.ReplaceAll(incomingConfigFixture, "mailbox: review@example.com", "mailbox: '"+mailbox+"'")
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadMainConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.ToLower(strings.TrimSpace(mailbox)); cfg.IncomingReview.Mailbox != want {
				t.Fatalf("mailbox = %q, want %q", cfg.IncomingReview.Mailbox, want)
			}
		})
	}
}

func TestIncomingReviewAbsentPreservesExistingConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("webhook:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadMainConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IncomingReview != nil || cfg.Webhook.StateFile != "/data/webhook-state.json" || cfg.Webhook.PollIntervalSeconds != 60 {
		t.Fatal("source-absent config changed existing defaults")
	}
}

func TestIncomingReviewConfigRejectsSymlinkSpool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(incomingConfigFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".incoming-review")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMainConfig(dir); err == nil {
		t.Fatal("incoming spool symlink escaped private subtree")
	}
}
