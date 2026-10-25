package webhook

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

const incomingBodyLimit = 1 << 20

// IncomingReviewEvent deliberately contains no email content or sender claims.
type IncomingReviewEvent struct {
	Mailbox   string `json:"mailbox"`
	MessageID string `json:"message_id"`
}

type incomingReviewState struct {
	ClientState    string `json:"client_state"`
	SubscriptionID string `json:"subscription_id"`
}

// IncomingReview authenticates notifications and commits metadata before ACK.
// It never fetches messages, evaluates rules, or interprets notification content.
type IncomingReview struct {
	mu       sync.RWMutex
	spoolDir string
	mailbox  string
	tenantID string
	userID   string
	state    incomingReviewState
}

func NewIncomingReview(spoolDir, tenantID, mailboxObjectID, mailbox string) (*IncomingReview, error) {
	if spoolDir == "" || tenantID == "" || !validIncomingSubscriptionID(mailboxObjectID) || !validIncomingMailbox(mailbox) {
		return nil, fmt.Errorf("incoming review requires spool directory, tenant, mailbox object ID and normalized mailbox address")
	}
	if err := makeIncomingDirectory(spoolDir); err != nil {
		return nil, err
	}
	info, err := os.Lstat(spoolDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("incoming review spool must be a private directory")
	}
	capture := &IncomingReview{spoolDir: spoolDir, mailbox: mailbox, tenantID: tenantID, userID: mailboxObjectID}
	path := filepath.Join(spoolDir, ".state.json")
	data, err := readPrivateFile(path, 4096)
	if err == nil {
		if err := json.Unmarshal(data, &capture.state); err != nil {
			return nil, fmt.Errorf("invalid incoming review state")
		}
		secret, err := hex.DecodeString(capture.state.ClientState)
		if err != nil || len(secret) != 32 || (capture.state.SubscriptionID != "" && !validIncomingSubscriptionID(capture.state.SubscriptionID)) {
			return nil, fmt.Errorf("invalid incoming review state")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, err
		}
		capture.state.ClientState = hex.EncodeToString(secret[:])
		if err := capture.saveState(capture.state); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	return capture, nil
}

func (c *IncomingReview) ClientState() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state.ClientState
}

func (c *IncomingReview) subscriptionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state.SubscriptionID
}

func (c *IncomingReview) setSubscriptionID(id string) error {
	if id != "" && !validIncomingSubscriptionID(id) {
		return fmt.Errorf("invalid incoming review subscription ID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state
	state.SubscriptionID = id
	if err := c.saveState(state); err != nil {
		return err
	}
	c.state = state
	return nil
}

func (c *IncomingReview) saveState(state incomingReviewState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeIncomingFile(c.spoolDir, ".state.json", data)
}

func validIncomingMailbox(mailbox string) bool {
	address, err := mail.ParseAddress(mailbox)
	return err == nil && address.Address == mailbox && strings.ToLower(mailbox) == mailbox
}

func validIncomingID(id string) bool {
	if id == "" || len(id) > 2048 {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validIncomingSubscriptionID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, ch := range id {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func (c *IncomingReview) authenticated(n Notification) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.userID == "" || c.state.SubscriptionID == "" || n.TenantID != c.tenantID || n.SubscriptionID != c.state.SubscriptionID || n.ChangeType != "created" || subtle.ConstantTimeCompare([]byte(n.ClientState), []byte(c.state.ClientState)) != 1 || n.ResourceData == nil || !validIncomingID(n.ResourceData.ID) {
		return false
	}
	// Bind Graph's Users/{object-id}/Messages/{id} resource only to the
	// administrator-supplied canonical mailbox ID, never webhook body claims.
	parts := strings.Split(n.Resource, "/")
	if len(parts) != 4 || !strings.EqualFold(parts[0], "users") || !strings.EqualFold(parts[2], "messages") || parts[1] != c.userID {
		return false
	}
	id, err := url.PathUnescape(parts[3])
	return err == nil && id == n.ResourceData.ID
}

func (c *IncomingReview) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if token := r.URL.Query().Get("validationToken"); token != "" {
		if len(token) > 4096 {
			http.Error(w, "invalid validation token", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, token)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, incomingBodyLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return
	}
	var payload NotificationPayload
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Value) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Validate the entire batch before writing any of it.
	for _, notification := range payload.Value {
		if !c.authenticated(notification) {
			http.Error(w, "invalid notification", http.StatusForbidden)
			return
		}
	}
	for _, notification := range payload.Value {
		event := IncomingReviewEvent{Mailbox: c.mailbox, MessageID: notification.ResourceData.ID}
		data, err := json.Marshal(event)
		if err != nil || writeIncomingFile(c.spoolDir, incomingEventName(event), data) != nil {
			http.Error(w, "unable to persist notification", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func incomingEventName(event IncomingReviewEvent) string {
	digest := sha256.Sum256([]byte(event.Mailbox + event.MessageID))
	return hex.EncodeToString(digest[:]) + ".json"
}

// ReadIncomingReviewEvents is read-only: no state creation, credentials, fetch,
// acknowledgement, or deletion. Empty/disabled sources encode as [] rather than null.
func ReadIncomingReviewEvents(spoolDir, mailbox string) ([]IncomingReviewEvent, error) {
	if !validIncomingMailbox(mailbox) {
		return nil, fmt.Errorf("incoming review requires a normalized mailbox address")
	}
	events := make([]IncomingReviewEvent, 0)
	entries, err := os.ReadDir(spoolDir)
	if errors.Is(err, os.ErrNotExist) {
		return events, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if len(name) != 69 || !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("unexpected incoming review spool entry")
		}
		data, err := readPrivateFile(filepath.Join(spoolDir, name), 16*1024)
		if err != nil {
			return nil, err
		}
		var event IncomingReviewEvent
		if err := json.Unmarshal(data, &event); err != nil || event.Mailbox != mailbox || !validIncomingID(event.MessageID) || incomingEventName(event) != name {
			return nil, fmt.Errorf("invalid incoming review event")
		}
		events = append(events, event)
	}
	return events, nil
}

func readPrivateFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > limit {
		return nil, fmt.Errorf("incoming review file must be private, regular and bounded")
	}
	return os.ReadFile(path)
}

// Atomic replacement plus file and directory fsync makes a successful return
// the durable boundary. Re-delivery writes the same content-addressed filename.
func writeIncomingFile(dir, name string, data []byte) error {
	file, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncIncomingDirectory(dir)
}

func syncIncomingDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func makeIncomingDirectory(dir string) error {
	var missing []string
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Persist every new directory entry as well as the event files within it.
	for _, created := range missing {
		if err := syncIncomingDirectory(filepath.Dir(created)); err != nil {
			return err
		}
	}
	return nil
}
