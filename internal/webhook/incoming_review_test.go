package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const incomingMailboxObjectID = "11111111-2222-3333-4444-555555555555"
const incomingMailbox = "review@example.com"

func newIncomingFixture(t *testing.T) *IncomingReview {
	t.Helper()
	capture, err := NewIncomingReview(filepath.Join(t.TempDir(), "spool"), "tenant", incomingMailboxObjectID, incomingMailbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.setSubscriptionID("owned-subscription"); err != nil {
		t.Fatal(err)
	}
	return capture
}

func incomingNotification(capture *IncomingReview) Notification {
	return Notification{
		ChangeType: "created", ClientState: capture.ClientState(),
		TenantID: "tenant", SubscriptionID: "owned-subscription",
		Resource:     "Users/" + incomingMailboxObjectID + "/Messages/immutable-message-id",
		ResourceData: &ResourceData{ID: "immutable-message-id"},
	}
}

func postIncoming(t *testing.T, capture *IncomingReview, notifications ...Notification) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(NotificationPayload{Value: notifications})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	capture.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/incoming-review", bytes.NewReader(body)))
	return response
}

func TestIncomingReviewDurableRedeliveryAndRestart(t *testing.T) {
	capture := newIncomingFixture(t)
	notification := incomingNotification(capture)
	for range 2 {
		response := postIncoming(t, capture, notification)
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		// The event must already be readable when the HTTP handler returns 202.
		events, err := ReadIncomingReviewEvents(capture.spoolDir, incomingMailbox)
		if err != nil || len(events) != 1 || events[0].Mailbox != incomingMailbox || events[0].MessageID != notification.ResourceData.ID {
			t.Fatalf("events = %#v, error = %v", events, err)
		}
	}
	restarted, err := NewIncomingReview(capture.spoolDir, "tenant", incomingMailboxObjectID, incomingMailbox)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.ClientState() != capture.ClientState() || restarted.subscriptionID() != capture.subscriptionID() {
		t.Fatal("private subscription state did not survive restart")
	}
	if response := postIncoming(t, restarted, notification); response.Code != http.StatusAccepted {
		t.Fatalf("redelivery after restart = %d", response.Code)
	}
	entries, err := os.ReadDir(capture.spoolDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("want state plus one deduplicated event: %v, %v", entries, err)
	}
	event := IncomingReviewEvent{Mailbox: incomingMailbox, MessageID: notification.ResourceData.ID}
	data, err := os.ReadFile(filepath.Join(capture.spoolDir, incomingEventName(event)))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 2 || fields["mailbox"] != incomingMailbox || fields["message_id"] != notification.ResourceData.ID {
		t.Fatalf("record is not exact minimal metadata: %s (%v)", data, err)
	}
	// Reading never acknowledges/deletes, including a message deleted upstream:
	// this source has no Graph message-fetch capability to silently drop it.
	for range 2 {
		events, err := ReadIncomingReviewEvents(capture.spoolDir, incomingMailbox)
		if err != nil || len(events) != 1 {
			t.Fatalf("read removed event: %#v (%v)", events, err)
		}
	}
}

func TestIncomingReviewRejectsUnauthenticatedNotifications(t *testing.T) {
	cases := map[string]func(*Notification){
		"missing tenant":       func(n *Notification) { n.TenantID = "" },
		"wrong tenant":         func(n *Notification) { n.TenantID = "other" },
		"missing client state": func(n *Notification) { n.ClientState = "" },
		"wrong client state":   func(n *Notification) { n.ClientState = "wrong" },
		"missing subscription": func(n *Notification) { n.SubscriptionID = "" },
		"wrong subscription":   func(n *Notification) { n.SubscriptionID = "primary" },
		"missing resource":     func(n *Notification) { n.Resource = "" },
		"wrong mailbox": func(n *Notification) {
			n.Resource = "Users/66666666-7777-8888-9999-aaaaaaaaaaaa/Messages/immutable-message-id"
		},
		"mailbox address":   func(n *Notification) { n.Resource = "Users/" + incomingMailbox + "/Messages/immutable-message-id" },
		"wrong resource id": func(n *Notification) { n.Resource = "Users/" + incomingMailboxObjectID + "/Messages/other" },
		"wrong change":      func(n *Notification) { n.ChangeType = "updated" },
		"missing data":      func(n *Notification) { n.ResourceData = nil },
		"missing id":        func(n *Notification) { n.ResourceData.ID = "" },
		"oversized id":      func(n *Notification) { n.ResourceData.ID = strings.Repeat("x", 2049) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			capture := newIncomingFixture(t)
			notification := incomingNotification(capture)
			mutate(&notification)
			response := postIncoming(t, capture, notification)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d", response.Code)
			}
			events, err := ReadIncomingReviewEvents(capture.spoolDir, incomingMailbox)
			if err != nil || len(events) != 0 {
				t.Fatalf("unauthenticated event persisted: %#v (%v)", events, err)
			}
		})
	}
}

func TestIncomingReviewValidatesWholeBatchBeforePersistence(t *testing.T) {
	capture := newIncomingFixture(t)
	good := incomingNotification(capture)
	bad := incomingNotification(capture)
	bad.TenantID = "other"
	if response := postIncoming(t, capture, good, bad); response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
	events, err := ReadIncomingReviewEvents(capture.spoolDir, incomingMailbox)
	if err != nil || len(events) != 0 {
		t.Fatalf("partially authenticated batch persisted: %#v (%v)", events, err)
	}
}

func TestIncomingReviewPersistenceFailureIsNotAcknowledged(t *testing.T) {
	capture := newIncomingFixture(t)
	notification := incomingNotification(capture)
	event := IncomingReviewEvent{Mailbox: incomingMailbox, MessageID: notification.ResourceData.ID}
	// A directory blocks atomic rename regardless of effective UID/permissions.
	if err := os.Mkdir(filepath.Join(capture.spoolDir, incomingEventName(event)), 0o700); err != nil {
		t.Fatal(err)
	}
	response := postIncoming(t, capture, notification)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed durable write acknowledged: %d", response.Code)
	}
}

func TestIncomingReviewBoundsBodyAndEchoesValidationSafely(t *testing.T) {
	capture := newIncomingFixture(t)
	response := httptest.NewRecorder()
	capture.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/incoming-review", strings.NewReader(strings.Repeat("x", incomingBodyLimit+1))))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", response.Code)
	}
	token := "<script>untrusted & plain</script>"
	response = httptest.NewRecorder()
	capture.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/incoming-review?validationToken="+url.QueryEscape(token), nil))
	if response.Code != http.StatusOK || response.Body.String() != token || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe validation response: %#v", response)
	}
}

func TestIncomingReviewRejectsPublicOrCorruptState(t *testing.T) {
	for _, content := range []string{`{"client_state":"predictable"}`, `not json`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewIncomingReview(dir, "tenant", incomingMailboxObjectID, incomingMailbox); err == nil {
			t.Fatal("invalid state was silently replaced")
		}
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIncomingReview(dir, "tenant", incomingMailboxObjectID, incomingMailbox); err == nil {
		t.Fatal("public spool accepted")
	}
}

func TestIncomingReviewSharesListenerAndPreservesPrimaryHandler(t *testing.T) {
	capture := newIncomingFixture(t)
	processed := make(chan string, 2)
	receiver := New(WebhookConfig{Path: "/webhook"}, func(_ context.Context, id string) error {
		processed <- id
		return nil
	})
	receiver.SetIncomingReview("/incoming-review", capture)
	listener := httptest.NewServer(receiver.routes())
	defer listener.Close()
	body, err := json.Marshal(NotificationPayload{Value: []Notification{incomingNotification(capture)}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(listener.URL+"/incoming-review", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("incoming status = %d", response.StatusCode)
	}
	events, err := ReadIncomingReviewEvents(capture.spoolDir, incomingMailbox)
	if err != nil || len(events) != 1 {
		t.Fatalf("HTTP acceptance preceded persistence: %#v (%v)", events, err)
	}
	body, err = json.Marshal(NotificationPayload{Value: []Notification{{
		ChangeType: "created", ClientState: receiver.ClientState(),
		ResourceData: &ResourceData{ID: "primary-message"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.Post(listener.URL+"/webhook", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("primary status = %d", response.StatusCode)
	}
	select {
	case id := <-processed:
		if id != "primary-message" {
			t.Fatalf("incoming source reached sorting: %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("primary handler no longer processes notifications")
	}
	response, err = http.Get(listener.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
}

func TestIncomingReviewAbsentAddsNoRoute(t *testing.T) {
	receiver := New(WebhookConfig{Path: "/webhook"}, nil)
	response := httptest.NewRecorder()
	receiver.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/incoming-review", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled incoming endpoint status = %d", response.Code)
	}
}

func TestIncomingReviewRequiresNormalizedExplicitMailbox(t *testing.T) {
	for _, mailbox := range []string{
		"",
		"not-an-address",
		"Review <review@example.com>",
		"review@example.com, primary@example.com",
		" review@example.com ",
		"Review@EXAMPLE.COM",
		"review@example.com\n",
		`review@example.com","message_id":"forged`,
	} {
		t.Run(mailbox, func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool")
			if _, err := NewIncomingReview(spool, "tenant", incomingMailboxObjectID, mailbox); err == nil {
				t.Fatal("invalid configured mailbox accepted by capture")
			}
			if _, err := ReadIncomingReviewEvents(spool, mailbox); err == nil {
				t.Fatal("invalid configured mailbox accepted by reader")
			}
			if _, err := os.Stat(spool); !os.IsNotExist(err) {
				t.Fatalf("invalid configured mailbox created spool: %v", err)
			}
		})
	}
}

func TestIncomingReviewUsesConfiguredMailboxAndReaderRejectsMismatch(t *testing.T) {
	const mailbox = "secondary@example.com"
	capture, err := NewIncomingReview(filepath.Join(t.TempDir(), "spool"), "tenant", incomingMailboxObjectID, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.setSubscriptionID("owned-subscription"); err != nil {
		t.Fatal(err)
	}
	notification := incomingNotification(capture)
	if response := postIncoming(t, capture, notification); response.Code != http.StatusAccepted {
		t.Fatalf("configured mailbox status = %d", response.Code)
	}
	events, err := ReadIncomingReviewEvents(capture.spoolDir, mailbox)
	if err != nil || len(events) != 1 || events[0].Mailbox != mailbox || events[0].MessageID != notification.ResourceData.ID {
		t.Fatalf("configured mailbox events = %#v (%v)", events, err)
	}
	if _, err := ReadIncomingReviewEvents(capture.spoolDir, incomingMailbox); err == nil {
		t.Fatal("reader accepted event for a different configured mailbox")
	}
}
