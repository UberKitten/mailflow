package graph

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// TestSetCategoriesEmptySerializesAsArray guards against regressing the nil
// slice -> JSON null bug. Microsoft Graph PATCH /messages/{id} rejects
// {"categories": null} with 400 RequestBodyRead even though the schema says
// the field is nullable. The empty array {"categories": []} is accepted and
// is the correct way to clear all categories.
func TestSetCategoriesEmptySerializesAsArray(t *testing.T) {
	cases := []struct {
		name  string
		input []string
	}{
		{"nil_slice", nil},
		{"empty_slice", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					t.Errorf("expected PATCH, got %s", r.Method)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				gotBody = body
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			dir := t.TempDir()
			tokenScript := writeScript(t, dir)
			client := newTestClient(t, server.URL, tokenScript)
			client.httpClient = server.Client()

			if err := client.SetCategories(context.Background(), "msg-1", tc.input); err != nil {
				t.Fatalf("SetCategories: %v", err)
			}

			var decoded map[string]interface{}
			if err := json.Unmarshal(gotBody, &decoded); err != nil {
				t.Fatalf("unmarshal request body: %v (body=%s)", err, string(gotBody))
			}
			cats, ok := decoded["categories"]
			if !ok {
				t.Fatalf("categories field missing from request body: %s", string(gotBody))
			}
			if cats == nil {
				t.Fatalf("categories serialized as null, want []; body=%s", string(gotBody))
			}
			arr, ok := cats.([]interface{})
			if !ok {
				t.Fatalf("categories not an array, got %T; body=%s", cats, string(gotBody))
			}
			if len(arr) != 0 {
				t.Fatalf("expected empty array, got %v", arr)
			}
		})
	}
}
func TestRemoveCategoryRemovesOnlyExactMatchAndPreservesOthers(t *testing.T) {
	var patched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string][]string{
				"categories": {
					"Keep",
					"Sort → Inbox/Security",
					"Sort → Inbox",
					"sort → Inbox/Security",
				},
			})
		case http.MethodPatch:
			var payload struct {
				Categories []string `json:"categories"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode PATCH: %v", err)
			}
			patched = payload.Categories
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	tokenScript := writeScript(t, t.TempDir())
	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()

	remaining, err := client.RemoveCategory(context.Background(), "moved-id", "Sort → Inbox/Security")
	if err != nil {
		t.Fatalf("RemoveCategory: %v", err)
	}
	want := []string{"Keep", "Sort → Inbox", "sort → Inbox/Security"}
	if len(remaining) != len(want) {
		t.Fatalf("remaining = %v, want %v", remaining, want)
	}
	for i := range want {
		if remaining[i] != want[i] {
			t.Fatalf("remaining[%d] = %q, want %q", i, remaining[i], want[i])
		}
	}
	if len(patched) != len(want) {
		t.Fatalf("PATCH categories = %v, want %v", patched, want)
	}
	for i := range want {
		if patched[i] != want[i] {
			t.Fatalf("PATCH categories[%d] = %q, want %q", i, patched[i], want[i])
		}
	}
}

func TestAddThenRemoveCategoryPreservesUnrelatedAndRuleCategories(t *testing.T) {
	state := []string{"Keep", "Sort → Promotions"}
	var patches [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string][]string{"categories": state})
		case http.MethodPatch:
			var payload struct {
				Categories []string `json:"categories"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode PATCH: %v", err)
			}
			state = append([]string(nil), payload.Categories...)
			patches = append(patches, append([]string(nil), state...))
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	tokenScript := writeScript(t, t.TempDir())
	client := newTestClient(t, server.URL, tokenScript)
	client.httpClient = server.Client()
	ctx := context.Background()

	afterAdd, err := client.AddCategories(ctx, "moved-id", []string{"Rule Category", "Keep"})
	if err != nil {
		t.Fatalf("AddCategories: %v", err)
	}
	wantAfterAdd := []string{"Keep", "Sort → Promotions", "Rule Category"}
	if !reflect.DeepEqual(afterAdd, wantAfterAdd) {
		t.Fatalf("after AddCategories = %v, want %v", afterAdd, wantAfterAdd)
	}

	afterRemove, err := client.RemoveCategory(ctx, "moved-id", "Sort → Promotions")
	if err != nil {
		t.Fatalf("RemoveCategory: %v", err)
	}
	wantAfterRemove := []string{"Keep", "Rule Category"}
	if !reflect.DeepEqual(afterRemove, wantAfterRemove) {
		t.Fatalf("after RemoveCategory = %v, want %v", afterRemove, wantAfterRemove)
	}
	wantPatches := [][]string{wantAfterAdd, wantAfterRemove}
	if !reflect.DeepEqual(patches, wantPatches) {
		t.Fatalf("PATCH sequence = %v, want %v", patches, wantPatches)
	}
}

func TestSenderPatternToFilter(t *testing.T) {
	tests := []struct {
		pattern        string
		expectedFilter string
		expectedSearch string
	}{
		// Exact match — uses $filter
		{"user@domain.com", "from/emailAddress/address eq 'user@domain.com'", ""},
		{"USER@DOMAIN.COM", "from/emailAddress/address eq 'user@domain.com'", ""}, // case-insensitive

		// Wildcard suffix patterns — uses $search
		{"*@domain.com", "", "from:domain.com"},
		{"*@DOMAIN.COM", "", "from:domain.com"}, // case-insensitive
		{"*@sub.domain.com", "", "from:sub.domain.com"},

		// Wildcard prefix patterns — uses $search
		{"user@*", "", "from:user@"},
		{"newsletter@*", "", "from:newsletter@"},

		// Complex wildcard patterns — extracts best search term
		{"*news*@domain.com", "", "from:@domain.com"},     // @domain.com is longest
		{"*newsletter*@*", "", "from:newsletter"},         // newsletter is longest
		{"user*@domain.com", "", "from:@domain.com"},      // @domain.com is longest
		{"*-notifications@*", "", "from:-notifications@"}, // -notifications@ is longest

		// Pattern that can't be narrowed — returns empty (must scan all)
		{"*@*", "", ""},

		// Empty pattern
		{"", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			gotFilter, gotSearch := senderPatternToFilter(tt.pattern)
			if gotFilter != tt.expectedFilter {
				t.Errorf("senderPatternToFilter(%q) filter = %q, want %q", tt.pattern, gotFilter, tt.expectedFilter)
			}
			if gotSearch != tt.expectedSearch {
				t.Errorf("senderPatternToFilter(%q) search = %q, want %q", tt.pattern, gotSearch, tt.expectedSearch)
			}
		})
	}
}

func TestMatchSenderPattern(t *testing.T) {
	tests := []struct {
		pattern string
		email   string
		want    bool
	}{
		// Exact match
		{"user@domain.com", "user@domain.com", true},
		{"user@domain.com", "USER@DOMAIN.COM", true}, // case-insensitive
		{"user@domain.com", "other@domain.com", false},

		// Wildcard suffix
		{"*@domain.com", "user@domain.com", true},
		{"*@domain.com", "any.user@domain.com", true},
		{"*@domain.com", "user@other.com", false},

		// Wildcard prefix
		{"user@*", "user@domain.com", true},
		{"user@*", "user@any.domain.com", true},
		{"user@*", "other@domain.com", false},

		// Complex wildcards
		{"*news*@*", "newsletter@domain.com", true},
		{"*news*@*", "daily-news@example.org", true},
		{"*news*@*", "random@domain.com", false},

		// Empty pattern matches all
		{"", "anything@anywhere.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.email, func(t *testing.T) {
			got := matchSenderPattern(tt.pattern, tt.email)
			if got != tt.want {
				t.Errorf("matchSenderPattern(%q, %q) = %v, want %v", tt.pattern, tt.email, got, tt.want)
			}
		})
	}
}
