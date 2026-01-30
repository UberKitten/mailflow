package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Config for webhook server
type WebhookConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Port        int    `yaml:"port"`
	Path        string `yaml:"path"`
	ExternalURL string `yaml:"external_url"`
}

// Notification from Graph API
type Notification struct {
	ChangeType         string            `json:"changeType"`
	ClientState        string            `json:"clientState"`
	Resource           string            `json:"resource"`
	ResourceData       *ResourceData     `json:"resourceData,omitempty"`
	SubscriptionID     string            `json:"subscriptionId"`
	SubscriptionExpiry string            `json:"subscriptionExpirationDateTime"`
	TenantID           string            `json:"tenantId"`
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

// Server handles Graph webhook notifications
type Server struct {
	config  WebhookConfig
	handler Handler
	server  *http.Server
	mu      sync.RWMutex
	state   string // client state for validation
}

// New creates a webhook server
func New(cfg WebhookConfig, handler Handler) *Server {
	return &Server{
		config:  cfg,
		handler: handler,
		state:   fmt.Sprintf("mailflow-%d", time.Now().UnixNano()),
	}
}

// ClientState returns the state string for subscription creation
func (s *Server) ClientState() string {
	return s.state
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
			if notification.ClientState != s.state {
				slog.Warn("invalid client state", "expected", s.state, "got", notification.ClientState)
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
			}
			cancel()
		}
	}()
}
