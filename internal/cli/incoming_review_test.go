package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"mailflow/internal/config"
	"mailflow/internal/webhook"

	"github.com/spf13/cobra"
)

func TestIncomingReviewEventsCommand(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "configured"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			configuration := "webhook:\n  enabled: true\n"
			want := "{\"events\":[]}\n"
			if enabled {
				configuration = `graph:
  auth_mode: cert
  client_id: fixture-client
  tenant_id: fixture-tenant
  cert_path: /not-opened/fixture.pfx
webhook:
  enabled: true
  external_url: https://notify.example/webhook
incoming_review:
  mailbox: review@example.com
  mailbox_object_id: 11111111-2222-3333-4444-555555555555
  path: /incoming-review
  external_url: https://notify.example/incoming-review
  spool_dir: .incoming-review
`
				spool := filepath.Join(dir, ".incoming-review")
				if err := os.Mkdir(spool, 0o700); err != nil {
					t.Fatal(err)
				}
				record := `{"mailbox":"review@example.com","message_id":"immutable-id"}`
				digest := sha256.Sum256([]byte("review@example.comimmutable-id"))
				if err := os.WriteFile(filepath.Join(spool, hex.EncodeToString(digest[:])+".json"), []byte(record), 0o600); err != nil {
					t.Fatal(err)
				}
				want = "{\"events\":[" + record + "]}\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configuration), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{Use: "mailflow"}
			cmd.Flags().String("config-dir", dir, "fixture config")
			var output bytes.Buffer
			cmd.SetOut(&output)
			if err := runIncomingReviewEvents(cmd, nil); err != nil {
				t.Fatal(err)
			}
			if output.String() != want {
				t.Fatalf("output = %q, want %q", output.String(), want)
			}
			if enabled {
				if _, err := os.Stat(filepath.Join(dir, ".incoming-review", ".state.json")); !os.IsNotExist(err) {
					t.Fatalf("read-only dump created private subscription state: %v", err)
				}
			}
		})
	}
}

func TestIncomingReviewAbsentDoesNotConfigureSubscription(t *testing.T) {
	server := webhook.New(webhook.WebhookConfig{}, nil)
	manager, err := configureIncomingReview(&config.Config{}, server, func() (string, error) {
		t.Fatal("source-absent configuration accessed credentials")
		return "", nil
	})
	if err != nil || manager != nil {
		t.Fatalf("absent incoming source configured manager: %v (%v)", manager, err)
	}
}
