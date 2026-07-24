package webhook

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWebhookStatePersistsAcrossServerInstances(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nested", "webhook-state.json")
	cfg := WebhookConfig{StateFile: statePath}

	first := New(cfg, nil)
	if first.ClientState() == "" {
		t.Fatal("generated client state is empty")
	}

	processedAt := time.Date(2026, time.July, 24, 12, 34, 56, 0, time.UTC)
	first.updateLastProcessedTime(processedAt)

	second := New(cfg, nil)
	if second.ClientState() != first.ClientState() {
		t.Fatalf("client state changed across restart: got %q, want %q", second.ClientState(), first.ClientState())
	}
	if got := second.LastProcessedTime(); !got.Equal(processedAt) {
		t.Fatalf("last processed time = %s, want %s", got, processedAt)
	}

	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state file mode = %#o, want 0600", got)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(statePath), ".webhook-state.json.tmp-*")); err != nil {
		t.Fatalf("glob temporary state files: %v", err)
	} else if len(matches) != 0 {
		t.Fatalf("temporary state files remain: %v", matches)
	}
}

func TestWebhookStateWriteReplacesExistingFileAndTightensMode(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "webhook-state.json")
	if err := os.WriteFile(statePath, []byte(`{"clientState":"old"}`), 0o644); err != nil {
		t.Fatalf("seed state file: %v", err)
	}

	want := webhookState{ClientState: "new", LastProcessedTime: "2026-07-24T12:34:56Z"}
	if err := writeWebhookState(statePath, want); err != nil {
		t.Fatalf("writeWebhookState: %v", err)
	}
	got, err := loadWebhookState(statePath)
	if err != nil {
		t.Fatalf("loadWebhookState: %v", err)
	}
	if got != want {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("state file mode = %#o, want 0600", gotMode)
	}
}
