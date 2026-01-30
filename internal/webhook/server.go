package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config for webhook server
type WebhookConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Port        int    `yaml:"port"`
	Path        string `yaml:"path"`
	ExternalURL string `yaml:"external_url"`
	StateFile   string `yaml:"state_file"`
}

// Notification from Graph API
type Notification struct {
	ChangeType         string        `json:"changeType"`
	ClientState        string        `json:"clientState"`
	Resource           string        `json:"resource"`
	ResourceData       *ResourceData `json:"resourceData,omitempty"`
	SubscriptionID     string        `json:"subscriptionId"`
	SubscriptionExpiry string        `json:"subscriptionExpirationDateTime"`
	TenantID           string        `json:"tenantId"`
}

type ResourceData struct {
	ODataType string `json:"@odata.type"`
	ODataID   string `json:"@odata.id"`
	ID        string `json:"id"`
}

type NotificationPayload struct {
	Value []Notification `json:"value"`
}

// Handler processes incoming mail notifications
type Handler func(ctx context.Context, messageID string) error

type webhookState struct {
	ClientState       string `json:"clientState"`
	LastProcessedTime string `json:"lastProcessedTime,omitempty"`
}

// Server handles Graph webhook notifications
type Server struct {
	config        WebhookConfig
	handler       Handler
	server        *http.Server
	mu            sync.RWMutex
	clientState   string
	lastProcessed time.Time
	stateFile     string
}

// New creates a webhook server
func New(cfg WebhookConfig, handler Handler) *Server {
	state := webhookState{}
	if cfg.StateFile != "" {
		loaded, err := loadWebhookState(cfg.StateFile)
		if err != nil {
			slog.Warn("failed to load webhook state", "error", err)
		} else {
			state = loaded
		}
	}

	if state.ClientState == "" {
		state.ClientState = generateClientState()
		if err := writeWebhookState(cfg.StateFile, state); err != nil {
			slog.Warn("failed to write webhook state", "error", err)
		}
	}

	server := &Server{
		config:      cfg,
		handler:     handler,
		clientState: state.ClientState,
		stateFile:   cfg.StateFile,
	}

	if state.LastProcessedTime != "" {
		if parsed, err := time.Parse(time.RFC3339, state.LastProcessedTime); err != nil {
			slog.Warn("invalid webhook lastProcessedTime", "value", state.LastProcessedTime, "error", err)
		} else {
			server.lastProcessed = parsed
		}
	}

	return server
}

// ClientState returns the state string for subscription creation
func (s *Server) ClientState() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientState
}

// LastProcessedTime returns the last successful webhook processing time.
func (s *Server) LastProcessedTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastProcessed
}

// Start the webhook server
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc(s.config.Path, s.handleWebhook)
	mux.HandleFunc("/health", s.handleHealth)

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	slog.Info("starting webhook server", "port", s.config.Port, "path", s.config.Path)
	return s.server.ListenAndServe()
}

// Stop gracefully shuts down the server
func (s *Server) Stop(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Graph validation request - echo back the token
	if validationToken := r.URL.Query().Get("validationToken"); validationToken != "" {
		slog.Info("webhook validation request received")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(validationToken))
		return
	}

	// Notification request
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("failed to read request body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var payload NotificationPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Error("failed to parse notification", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Respond immediately - process async
	w.WriteHeader(http.StatusAccepted)

	// Process notifications
	go func() {
		for _, notification := range payload.Value {
			// Validate client state
			if notification.ClientState != s.clientState {
				slog.Warn("invalid client state", "expected", s.clientState, "got", notification.ClientState)
				continue
			}

			if notification.ChangeType != "created" {
				continue
			}

			if notification.ResourceData == nil {
				slog.Warn("notification missing resource data")
				continue
			}

			messageID := notification.ResourceData.ID
			if messageID == "" {
				slog.Warn("notification missing message ID")
				continue
			}

			slog.Info("processing new mail notification", "messageId", messageID)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.handler(ctx, messageID); err != nil {
				slog.Error("failed to process notification", "messageId", messageID, "error", err)
			} else {
				s.updateLastProcessedTime(time.Now().UTC())
			}
			cancel()
		}
	}()
}

func generateClientState() string {
	return fmt.Sprintf("mailflow-%d", time.Now().UnixNano())
}

func loadWebhookState(path string) (webhookState, error) {
	var state webhookState
	if path == "" {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func writeWebhookState(path string, state webhookState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (s *Server) updateLastProcessedTime(t time.Time) {
	s.mu.Lock()
	if t.After(s.lastProcessed) {
		s.lastProcessed = t
	}
	state := webhookState{ClientState: s.clientState}
	if !s.lastProcessed.IsZero() {
		state.LastProcessedTime = s.lastProcessed.UTC().Format(time.RFC3339)
	}
	s.mu.Unlock()

	if err := writeWebhookState(s.stateFile, state); err != nil {
		slog.Warn("failed to persist webhook state", "error", err)
	}
}
