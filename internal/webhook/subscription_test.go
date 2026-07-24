package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateOrRenewCreatesRequestedMailboxResource(t *testing.T) {
	var created Subscription
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []Subscription{}})
		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Subscription{ID: "created", ExpirationDateTime: created.ExpirationDateTime})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resource := "users/configured@example.com/mailFolders('unsorted-id')/messages"
	manager := NewSubscriptionManager(server.URL, func() (string, error) { return "token", nil }, "https://notify.example/webhook", "state", resource)
	before := time.Now().UTC()
	if err := manager.CreateOrRenew(context.Background()); err != nil {
		t.Fatalf("CreateOrRenew: %v", err)
	}

	if created.Resource != resource {
		t.Fatalf("created resource = %q, want %q", created.Resource, resource)
	}
	expiry, err := time.Parse(time.RFC3339, created.ExpirationDateTime)
	if err != nil {
		t.Fatalf("parse expiration: %v", err)
	}
	if expiry.Before(before.Add(71*time.Hour+55*time.Minute)) || expiry.After(before.Add(72*time.Hour+5*time.Minute)) {
		t.Fatalf("expiration %s is not about 72 hours after %s", expiry, before)
	}
}

func TestCreateOrRenewReplacesStaleMailboxResource(t *testing.T) {
	const (
		notifyURL     = "https://notify.example/webhook"
		staleResource = "me/mailFolders('unsorted-id')/messages"
		wantResource  = "users/configured@example.com/mailFolders('unsorted-id')/messages"
	)
	var deletedStale bool
	var createdResource string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []Subscription{{
				ID:              "stale",
				NotificationURL: notifyURL,
				Resource:        staleResource,
			}}})
		case r.Method == http.MethodDelete && r.URL.Path == "/subscriptions/stale":
			deletedStale = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions":
			var sub Subscription
			if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			createdResource = sub.Resource
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Subscription{ID: "replacement", ExpirationDateTime: sub.ExpirationDateTime})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewSubscriptionManager(server.URL, func() (string, error) { return "token", nil }, notifyURL, "state", wantResource)
	if err := manager.CreateOrRenew(context.Background()); err != nil {
		t.Fatalf("CreateOrRenew: %v", err)
	}
	if !deletedStale {
		t.Fatal("stale resource subscription was not deleted")
	}
	if createdResource != wantResource {
		t.Fatalf("replacement resource = %q, want %q", createdResource, wantResource)
	}
}

func TestCreateOrRenewReconcilesStaleSubscriptionAfterCurrent(t *testing.T) {
	const (
		notifyURL     = "https://notify.example/webhook"
		staleResource = "me/mailFolders('unsorted-id')/messages"
		wantResource  = "users/configured@example.com/mailFolders('unsorted-id')/messages"
	)
	var deletedStale, renewedCurrent bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []Subscription{
				{ID: "current", NotificationURL: notifyURL, Resource: wantResource},
				{ID: "stale", NotificationURL: notifyURL, Resource: staleResource},
			}})
		case r.Method == http.MethodDelete && r.URL.Path == "/subscriptions/stale":
			deletedStale = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == "/subscriptions/current":
			renewedCurrent = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			t.Fatal("created a duplicate subscription instead of renewing the current one")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewSubscriptionManager(server.URL, func() (string, error) { return "token", nil }, notifyURL, "state", wantResource)
	if err := manager.CreateOrRenew(context.Background()); err != nil {
		t.Fatalf("CreateOrRenew: %v", err)
	}
	if !deletedStale {
		t.Fatal("stale subscription listed after the current one was not deleted")
	}
	if !renewedCurrent {
		t.Fatal("current subscription was not renewed")
	}
}

func TestCreateOrRenewAbortsWhenStaleDeleteFails(t *testing.T) {
	const (
		notifyURL     = "https://notify.example/webhook"
		staleResource = "me/mailFolders('unsorted-id')/messages"
		wantResource  = "users/configured@example.com/mailFolders('unsorted-id')/messages"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []Subscription{{
				ID:              "stale",
				NotificationURL: notifyURL,
				Resource:        staleResource,
			}}})
		case r.Method == http.MethodDelete && r.URL.Path == "/subscriptions/stale":
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		case r.Method == http.MethodPost:
			t.Fatal("created a replacement after stale deletion failed")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewSubscriptionManager(server.URL, func() (string, error) { return "token", nil }, notifyURL, "state", wantResource)
	if err := manager.CreateOrRenew(context.Background()); err == nil {
		t.Fatal("expected stale deletion error")
	}
}

func TestCreateOrRenewRejectsEmptyMailboxResourceBeforeRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	manager := NewSubscriptionManager(server.URL, func() (string, error) { return "token", nil }, "https://notify.example/webhook", "state", "")
	if err := manager.CreateOrRenew(context.Background()); err == nil {
		t.Fatal("expected empty mailbox resource error")
	}
}
