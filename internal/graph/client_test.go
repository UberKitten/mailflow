package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailflow/internal/config"

	"golang.org/x/sync/semaphore"
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

func TestSetImportancePatchesMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/me/messages/message-id" {
			t.Errorf("path = %s, want /me/messages/message-id", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if got, want := string(body), `{"importance":"high"}`; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, writeScript(t, t.TempDir()))
	client.httpClient = server.Client()
	if err := client.SetImportance(context.Background(), "message-id", "high"); err != nil {
		t.Fatalf("SetImportance: %v", err)
	}
}

func TestSetImportanceReportsGraphFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid importance", http.StatusBadRequest)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, writeScript(t, t.TempDir()))
	client.httpClient = server.Client()
	err := client.SetImportance(context.Background(), "message-id", "high")
	if err == nil {
		t.Fatal("SetImportance succeeded, want Graph failure")
	}
	if !strings.Contains(err.Error(), "400 Bad Request") || !strings.Contains(err.Error(), "invalid importance") {
		t.Fatalf("error = %q, want Graph status and response body", err)
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

func TestHasSentMessageByInternetMessageIDSearchesOnlySentItems(t *testing.T) {
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/me/mailFolders/sentitems/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		filter := r.URL.Query().Get("$filter")
		filters = append(filters, filter)
		switch filter {
		case "internetMessageId eq '<missing@example.com>'":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
		case "internetMessageId eq '<sent.o''ne@example.com>'":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{{"id": "sent-id"}}})
		default:
			t.Errorf("unexpected filter: %q", filter)
			http.Error(w, "unexpected filter", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	client := newTestClient(t, server.URL, writeScript(t, dir))
	client.httpClient = server.Client()

	matched, err := client.HasSentMessageByInternetMessageID(context.Background(), []string{
		"<missing@example.com>",
		"<sent.o'ne@example.com>",
	})
	if err != nil {
		t.Fatalf("HasSentMessageByInternetMessageID: %v", err)
	}
	if !matched {
		t.Fatal("expected a Sent Items match")
	}
	if len(filters) != 2 {
		t.Fatalf("filters = %v, want both references checked in order", filters)
	}
}

type graphRoundTripper func(*http.Request) (*http.Response, error)

func (f graphRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type graphCloseCounter struct {
	io.Reader
	closes *atomic.Int32
}

func (b graphCloseCounter) Close() error {
	b.closes.Add(1)
	return nil
}

func newRetryTestClient(t *testing.T, transport graphRoundTripper) *Client {
	t.Helper()
	client := newTestClient(t, "https://graph.invalid", writeScript(t, t.TempDir()))
	client.httpClient = &http.Client{Transport: transport}
	return client
}

func graphTestResponse(status int, retryAfter string, closes *atomic.Int32) *http.Response {
	header := make(http.Header)
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       graphCloseCounter{Reader: strings.NewReader("response"), closes: closes},
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"", 0, false},
		{"0", 0, true},
		{" 12 ", 12 * time.Second, true},
		{"9223372036", 9223372036 * time.Second, true},
		{"9223372037", 0, false},
		{"18446744073709551616", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"1.5", 0, false},
		{"nonsense", 0, false},
		{"Fri, 31 Dec 9999 23:59:59 GMT", 0, false},
		{"Sun, 06 Nov 1994 08:49:37 GMT", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, valid := parseRetryAfter(tt.value)
			if got != tt.want || valid != tt.valid {
				t.Fatalf("parseRetryAfter(%q) = %v, %v; want %v, %v", tt.value, got, valid, tt.want, tt.valid)
			}
		})
	}
	date := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	before := time.Now()
	delay, valid := parseRetryAfter(date.Format(http.TimeFormat))
	after := time.Now()
	if !valid || delay < date.Sub(after) || delay > date.Sub(before) {
		t.Fatalf("HTTP-date delay = %v, %v; target %v", delay, valid, date)
	}
}

func TestGraphRetryAfterNeverRetriesEarly(t *testing.T) {
	for _, dateHeader := range []bool{false, true} {
		t.Run(fmt.Sprintf("date=%v", dateHeader), func(t *testing.T) {
			var calls atomic.Int32
			var closes atomic.Int32
			var notBefore time.Time
			client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					header := "1"
					notBefore = time.Now().Add(time.Second)
					if dateHeader {
						notBefore = time.Now().Add(2 * time.Second).UTC().Truncate(time.Second)
						header = notBefore.Format(http.TimeFormat)
					}
					return graphTestResponse(http.StatusTooManyRequests, header, &closes), nil
				}
				if time.Now().Before(notBefore) {
					t.Error("Graph retry sent before Retry-After minimum")
				}
				return graphTestResponse(http.StatusOK, "", &closes), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			resp, err := client.doWithRetry(ctx, http.MethodGet, "https://graph.invalid/messages", nil)
			if err != nil {
				t.Fatalf("doWithRetry: %v", err)
			}
			resp.Body.Close()
			if calls.Load() != 2 || closes.Load() != 2 {
				t.Fatalf("calls=%d closes=%d, want 2 each", calls.Load(), closes.Load())
			}
		})
	}
}

func TestGraphRetryRecreatesBodiesAfter429(t *testing.T) {
	for _, kind := range []string{"bytes-reader", "bytes-buffer", "strings-reader"} {
		t.Run(kind, func(t *testing.T) {
			const payload = `{"destinationId":"folder"}`
			var body io.Reader
			switch kind {
			case "bytes-reader":
				body = bytes.NewReader([]byte(payload))
			case "bytes-buffer":
				body = bytes.NewBufferString(payload)
			case "strings-reader":
				body = strings.NewReader(payload)
			}
			var calls atomic.Int32
			var closes atomic.Int32
			client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
				got, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil || string(got) != payload {
					t.Errorf("request body = %q, %v; want %q", got, err, payload)
				}
				if calls.Add(1) == 1 {
					return graphTestResponse(http.StatusTooManyRequests, "0", &closes), nil
				}
				return graphTestResponse(http.StatusCreated, "", &closes), nil
			})
			resp, err := client.doWithRetry(context.Background(), http.MethodPost, "https://graph.invalid/move", body)
			if err != nil {
				t.Fatalf("doWithRetry: %v", err)
			}
			resp.Body.Close()
			if calls.Load() != 2 || closes.Load() != 2 {
				t.Fatalf("calls=%d closes=%d, want 2 each", calls.Load(), closes.Load())
			}
		})
	}
}

func TestGraphRetryDoesNotReplayUnsafeRequests(t *testing.T) {
	for _, tt := range []struct {
		name   string
		method string
		status int
		body   io.Reader
	}{
		{"ambiguous-post", http.MethodPost, http.StatusServiceUnavailable, strings.NewReader(`{}`)},
		{"ambiguous-patch", http.MethodPatch, http.StatusInternalServerError, strings.NewReader(`{}`)},
		{"consumed-reader", http.MethodGet, http.StatusTooManyRequests, io.LimitReader(strings.NewReader(`{}`), 2)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			var closes atomic.Int32
			client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				io.Copy(io.Discard, req.Body)
				req.Body.Close()
				return graphTestResponse(tt.status, "0", &closes), nil
			})
			resp, err := client.doWithRetry(context.Background(), tt.method, "https://graph.invalid/messages", tt.body)
			if err != nil {
				t.Fatalf("doWithRetry: %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("status=%d, want %d", resp.StatusCode, tt.status)
			}
			resp.Body.Close()
			if calls.Load() != 1 || closes.Load() != 1 {
				t.Fatalf("calls=%d closes=%d, want 1 each", calls.Load(), closes.Load())
			}
		})
	}
}

func TestGraphRetryStopsAfterSixAttempts(t *testing.T) {
	var calls atomic.Int32
	var closes atomic.Int32
	client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return graphTestResponse(http.StatusServiceUnavailable, "0", &closes), nil
	})
	client.sema = make(chan struct{}, 1)
	resp, err := client.doWithRetry(context.Background(), http.MethodGet, "https://graph.invalid/messages", nil)
	if err == nil || resp != nil {
		t.Fatalf("response=%v error=%v, want retry exhaustion", resp, err)
	}
	if calls.Load() != 6 || closes.Load() != 6 || len(client.sema) != 0 {
		t.Fatalf("calls=%d closes=%d permits=%d, want 6, 6, 0", calls.Load(), closes.Load(), len(client.sema))
	}
	requests, retries, _, _ := client.Metrics()
	if requests != 6 || retries != 5 {
		t.Fatalf("metrics=%d requests/%d retries, want 6/5", requests, retries)
	}
}

func TestGraphRetryCancellationAndBudget(t *testing.T) {
	var calls atomic.Int32
	var closes atomic.Int32
	client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > graphRetryBudget {
			t.Error("request is missing bounded retry deadline")
		}
		return graphTestResponse(http.StatusTooManyRequests, "9223372036", &closes), nil
	})
	client.sema = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	resp, err := client.doWithRetry(ctx, http.MethodGet, "https://graph.invalid/messages", nil)
	if !errors.Is(err, context.DeadlineExceeded) || resp != nil {
		t.Fatalf("response=%v error=%v, want deadline exceeded", resp, err)
	}
	if calls.Load() != 1 || closes.Load() != 1 || len(client.sema) != 0 {
		t.Fatalf("calls=%d closes=%d permits=%d, want 1, 1, 0", calls.Load(), closes.Load(), len(client.sema))
	}
}

func TestGraphCooldownAppliesToOtherRequests(t *testing.T) {
	var calls atomic.Int32
	var closes atomic.Int32
	client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		io.Copy(io.Discard, req.Body)
		req.Body.Close()
		return graphTestResponse(http.StatusTooManyRequests, "1", &closes), nil
	})
	// An unreplayable caller still publishes the mailbox's cooldown.
	body := io.LimitReader(strings.NewReader(`{}`), 2)
	resp, err := client.doWithRetry(context.Background(), http.MethodPost, "https://graph.invalid/move", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = client.doWithRetry(ctx, http.MethodGet, "https://graph.invalid/messages", nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("second request error=%v calls=%d, want canceled cooldown and no new transmission", err, calls.Load())
	}
}

func TestGraphSemaphoreLivesUntilBodyClose(t *testing.T) {
	var calls atomic.Int32
	var closes atomic.Int32
	var responseContext context.Context
	client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		responseContext = req.Context()
		return graphTestResponse(http.StatusOK, "", &closes), nil
	})
	client.sema = make(chan struct{}, 1)
	first, err := client.doWithRetry(context.Background(), http.MethodGet, "https://graph.invalid/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	if err := responseContext.Err(); err != nil {
		t.Fatalf("response context canceled before body consumption: %v", err)
	}
	deadline, ok := responseContext.Deadline()
	if !ok || time.Until(deadline) > graphRetryBudget || time.Until(deadline) < graphRetryBudget-time.Second {
		t.Fatalf("default request deadline=%v, present=%v; want five-minute budget", deadline, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = client.doWithRetry(ctx, http.MethodGet, "https://graph.invalid/messages", nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 || len(client.sema) != 1 {
		t.Fatalf("error=%v calls=%d permits=%d, want canceled waiter and held permit", err, calls.Load(), len(client.sema))
	}
	first.Body.Close()
	first.Body.Close()
	if closes.Load() != 1 || len(client.sema) != 0 {
		t.Fatalf("closes=%d permits=%d, want idempotent close and released permit", closes.Load(), len(client.sema))
	}
	if !errors.Is(responseContext.Err(), context.Canceled) {
		t.Fatalf("response context not canceled on Close: %v", responseContext.Err())
	}
	third, err := client.doWithRetry(context.Background(), http.MethodGet, "https://graph.invalid/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	third.Body.Close()
	if calls.Load() != 2 || closes.Load() != 2 {
		t.Fatalf("calls=%d closes=%d, want 2 each", calls.Load(), closes.Load())
	}
}

func TestGraphCooldownDoesNotShortenAndIsCancelable(t *testing.T) {
	client := newRetryTestClient(t, nil)
	client.extendCooldown(time.Hour)
	client.cooldownMu.Lock()
	original := client.cooldownUntil
	client.cooldownMu.Unlock()
	client.extendCooldown(time.Millisecond)
	client.cooldownMu.Lock()
	current := client.cooldownUntil
	client.cooldownMu.Unlock()
	if current != original {
		t.Fatalf("cooldown shortened from %v to %v", original, current)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.waitCooldown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitCooldown error=%v, want context canceled", err)
	}
}

func TestFindFolderClosesPageBeforeNextRequest(t *testing.T) {
	var calls atomic.Int32
	var closes atomic.Int32
	client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		payload := `{"value":[{"id":"wanted","displayName":"Target"}]}`
		if n == 1 {
			payload = `{"value":[],"@odata.nextLink":"https://graph.invalid/page2"}`
		} else if closes.Load() != 1 {
			t.Error("previous page still open")
		}
		resp := graphTestResponse(http.StatusOK, "", &closes)
		resp.Body = graphCloseCounter{Reader: strings.NewReader(payload), closes: &closes}
		return resp, nil
	})
	client.sema = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	id, err := client.FindFolderIDByPath(ctx, "Target")
	if err != nil || id != "wanted" {
		t.Fatalf("FindFolderIDByPath = %q, %v", id, err)
	}
	if calls.Load() != 2 || closes.Load() != 2 {
		t.Fatalf("calls=%d closes=%d, want 2 each", calls.Load(), closes.Load())
	}
}

func TestGraphAuthenticationWaitsAreCancelable(t *testing.T) {
	for _, cert := range []bool{false, true} {
		t.Run(fmt.Sprintf("cert=%v", cert), func(t *testing.T) {
			lock := semaphore.NewWeighted(1)
			if err := lock.Acquire(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			defer lock.Release(1)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			var err error
			if cert {
				p := &certTokenProvider{mu: lock}
				_, err = p.getToken(ctx)
			} else {
				p := &tokenProvider{mu: lock}
				_, err = p.getToken(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("getToken error=%v, want deadline exceeded waiting for provider", err)
			}
		})
	}
}

func TestGraphTokenRefreshUsesRequestContext(t *testing.T) {
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	var calls atomic.Int32
	http.DefaultClient = &http.Client{Transport: graphRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte(`{"refresh_token":"test-refresh-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := newTokenProvider("test-client", "test-tenant", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = provider.getToken(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("getToken error=%v calls=%d, want canceled refresh request", err, calls.Load())
	}
}

func TestGraphTokenScriptIsCancelable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := tokenFromScript(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("tokenFromScript error=%v, want context deadline exceeded", err)
	}
}

func TestGraphTransportErrorIsNotReplayedAndReleasesPermit(t *testing.T) {
	var calls atomic.Int32
	transportErr := errors.New("ambiguous transport failure")
	client := newRetryTestClient(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, transportErr
	})
	client.sema = make(chan struct{}, 1)
	resp, err := client.doWithRetry(context.Background(), http.MethodPost, "https://graph.invalid/move", strings.NewReader(`{}`))
	if resp != nil || !errors.Is(err, transportErr) || calls.Load() != 1 || len(client.sema) != 0 {
		t.Fatalf("response=%v error=%v calls=%d permits=%d; want unreplayed error and released permit",
			resp, err, calls.Load(), len(client.sema))
	}
}
