package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mailflow/internal/config"
)

func writeScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "token.sh")
	content := "#!/bin/sh\necho testtoken\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write token script: %v", err)
	}
	return path
}

func newTestClient(t *testing.T, baseURL string, tokenScript string) *Client {
	t.Helper()
	cfg := &config.Config{Graph: config.GraphConfig{BaseURL: baseURL, TokenScript: tokenScript}}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestFindFolderIDByPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/mailFolders":
			json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]string{{"id": "inbox", "displayName": "Inbox"}},
			})
		case "/me/mailFolders/inbox/childFolders":
			json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]string{{"id": "alerts", "displayName": "Alerts"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenScript := writeScript(t, dir)

	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()

	id, err := client.FindFolderIDByPath(context.Background(), "Inbox/Alerts")
	if err != nil {
		t.Fatalf("FindFolderIDByPath: %v", err)
	}
	if id != "alerts" {
		t.Fatalf("expected folder id alerts, got %q", id)
	}
}

func TestFindFolderIDByPathNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{}})
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenScript := writeScript(t, dir)

	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()

	_, err := client.FindFolderIDByPath(context.Background(), "Inbox")
	if err == nil {
		t.Fatalf("expected not found error")
	}
}

func TestFindFolderIDByPathAPIFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenScript := writeScript(t, dir)

	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()

	_, err := client.FindFolderIDByPath(context.Background(), "Inbox")
	if err == nil {
		t.Fatalf("expected API error")
	}
}

func TestListFolderTree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/mailFolders/root/childFolders":
			json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]string{{"id": "child", "displayName": "Child"}},
			})
		case "/me/mailFolders/child/childFolders":
			json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenScript := writeScript(t, dir)

	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()

	folders, err := client.ListFolderTree(context.Background(), "root", "Inbox")
	if err != nil {
		t.Fatalf("ListFolderTree: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("expected 2 folders, got %d", len(folders))
	}
	if folders[1].Path != "Inbox/Child" {
		t.Fatalf("unexpected path: %q", folders[1].Path)
	}
}

func TestListMessagesAPIFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	dir := t.TempDir()
	tokenScript := writeScript(t, dir)

	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()

	_, err := client.ListMessages(context.Background(), "folder", ListOptions{})
	if err == nil {
		t.Fatalf("expected list messages error")
	}
}
