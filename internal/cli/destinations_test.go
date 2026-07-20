package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunDestinationsJSONLoadsSortedRealRules(t *testing.T) {
	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "rules.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("include:\n  - rules.d/*.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rules := `version: 1
rules:
  - name: tech
    folder: Posts/Tech
    from: tech@example.com
  - name: promotions
    folder: Promotions
    from: deals@example.com
  - name: duplicate
    folder: Posts/Tech
    from: other@example.com
  - name: notify
    notify_only: true
    from: alerts@example.com
`
	if err := os.WriteFile(filepath.Join(configDir, "rules.d", "rules.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	root := &cobra.Command{Use: "mailflow"}
	root.Flags().String("config-dir", configDir, "")
	cmd := &cobra.Command{Use: "destinations"}
	cmd.Flags().Bool("json", true, "")
	root.AddCommand(cmd)

	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runDestinations(cmd, nil); err != nil {
		t.Fatalf("runDestinations: %v", err)
	}

	want := "{\"destinations\":[\"Inbox\",\"Posts/Tech\",\"Promotions\"]}\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}
