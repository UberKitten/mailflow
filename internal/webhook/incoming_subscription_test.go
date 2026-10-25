package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIncomingSubscriptionImmutableLifecycleAndPrimaryIsolation(t *testing.T) {
	capture := newIncomingFixture(t)
	if err := capture.setSubscriptionID(""); err != nil {
		t.Fatal(err)
	}
	const primaryURL = "https://notify.example/webhook"
	const incomingURL = "https://notify.example/incoming-review"
	const resource = "users/" + incomingMailboxObjectID + "/messages"
	var created *Subscription
	var renewals, ownedDeletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions":
			subscriptions := []Subscription{
				{ID: "primary", Resource: "users/66666666-7777-8888-9999-aaaaaaaaaaaa/messages", NotificationURL: primaryURL, ClientState: "primary-state", ChangeType: "created"},
				{ID: "foreign-same-url", Resource: "users/66666666-7777-8888-9999-aaaaaaaaaaaa/messages", NotificationURL: incomingURL, ClientState: "foreign-state", ChangeType: "created"},
				{ID: "foreign-same-resource", Resource: resource, NotificationURL: incomingURL, ClientState: "foreign-state", ChangeType: "created"},
			}
			if created != nil {
				subscriptions = append(subscriptions, *created)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": subscriptions})
		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions":
			if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
				t.Errorf("create missing immutable preference: %q", r.Header.Get("Prefer"))
			}
			var subscription Subscription
			if err := json.NewDecoder(r.Body).Decode(&subscription); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if subscription.Resource != resource || subscription.ChangeType != "created" || subscription.NotificationURL != incomingURL || subscription.ClientState != capture.ClientState() {
				t.Error("incorrect incoming subscription contract")
			}
			subscription.ID = "new-owned-subscription"
			created = &subscription
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(subscription)
		case r.Method == http.MethodPatch && r.URL.Path == "/subscriptions/new-owned-subscription":
			renewals++
			if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
				t.Error("renewal missing immutable preference")
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == "/subscriptions/new-owned-subscription":
			ownedDeletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected mutation/request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	manager := NewIncomingSubscriptionManager(server.URL, func() (string, error) { return "fixture-token", nil }, incomingURL, capture)
	if err := manager.CreateOrRenew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if capture.subscriptionID() != "new-owned-subscription" {
		t.Fatal("created subscription ownership was not persisted")
	}
	// Restart must renew the owned source, not delete primary or any foreign
	// subscription even when it shares this incoming URL/resource.
	restarted, err := NewIncomingReview(capture.spoolDir, "tenant", incomingMailboxObjectID, incomingMailbox)
	if err != nil {
		t.Fatal(err)
	}
	manager = NewIncomingSubscriptionManager(server.URL, func() (string, error) { return "fixture-token", nil }, incomingURL, restarted)
	if err := manager.CreateOrRenew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if renewals != 1 {
		t.Fatalf("owned renewal count = %d", renewals)
	}
	if err := manager.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ownedDeletes != 1 {
		t.Fatalf("owned deletion count = %d", ownedDeletes)
	}
}

func TestIncomingSubscriptionListFailureDoesNotCreateDuplicates(t *testing.T) {
	capture := newIncomingFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation after failed list: %s", r.Method)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	manager := NewIncomingSubscriptionManager(server.URL, func() (string, error) { return "fixture-token", nil }, "https://notify.example/incoming-review", capture)
	if err := manager.CreateOrRenew(context.Background()); err == nil {
		t.Fatal("expected list failure")
	}
}
