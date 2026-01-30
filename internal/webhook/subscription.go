package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Subscription represents a Graph webhook subscription
type Subscription struct {
	ID                      string `json:"id,omitempty"`
	ChangeType              string `json:"changeType"`
	NotificationURL         string `json:"notificationUrl"`
	Resource                string `json:"resource"`
	ExpirationDateTime      string `json:"expirationDateTime"`
	ClientState             string `json:"clientState"`
	LatestSupportedTLSVer   string `json:"latestSupportedTlsVersion,omitempty"`
}

// SubscriptionManager handles Graph subscription lifecycle
type SubscriptionManager struct {
	baseURL        string
	tokenFunc      func() (string, error)
	notifyURL      string
	clientState    string
	resource       string
	subscriptionID string
}

// NewSubscriptionManager creates a new manager
// resource is the Graph API resource to watch, e.g. "me/mailFolders('Inbox')/messages"
func NewSubscriptionManager(baseURL string, tokenFunc func() (string, error), notifyURL, clientState, resource string) *SubscriptionManager {
	if resource == "" {
		resource = "me/mailFolders('Inbox')/messages"
	}
	return &SubscriptionManager{
		baseURL:     baseURL,
		tokenFunc:   tokenFunc,
		notifyURL:   notifyURL,
		clientState: clientState,
		resource:    resource,
	}
}

// CreateOrRenew ensures an active subscription exists
func (m *SubscriptionManager) CreateOrRenew(ctx context.Context) error {
	// Try to list existing subscriptions first
	existing, err := m.list(ctx)
	if err != nil {
		slog.Warn("failed to list subscriptions", "error", err)
	}

	// Find our subscription
	for _, sub := range existing {
		if sub.NotificationURL == m.notifyURL {
			// Try to renew
			if err := m.renew(ctx, sub.ID); err != nil {
				slog.Warn("failed to renew subscription, will create new", "error", err)
				// Delete and recreate
				_ = m.delete(ctx, sub.ID)
			} else {
				m.subscriptionID = sub.ID
				slog.Info("renewed existing subscription", "id", sub.ID)
				return nil
			}
		}
	}

	// Create new subscription
	return m.create(ctx)
}

func (m *SubscriptionManager) create(ctx context.Context) error {
	token, err := m.tokenFunc()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	// Max expiration is 3 days for mail
	expiry := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)

	sub := Subscription{
		ChangeType:         "created",
		NotificationURL:    m.notifyURL,
		Resource:           m.resource,
		ExpirationDateTime: expiry,
		ClientState:        m.clientState,
	}

	body, _ := json.Marshal(sub)
	req, err := http.NewRequestWithContext(ctx, "POST", m.baseURL+"/subscriptions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("create subscription: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create subscription failed: %s - %s", resp.Status, string(respBody))
	}

	var created Subscription
	if err := json.Unmarshal(respBody, &created); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	m.subscriptionID = created.ID
	slog.Info("created subscription", "id", created.ID, "expires", created.ExpirationDateTime)
	return nil
}

func (m *SubscriptionManager) renew(ctx context.Context, id string) error {
	token, err := m.tokenFunc()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	expiry := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]string{
		"expirationDateTime": expiry,
	})

	req, err := http.NewRequestWithContext(ctx, "PATCH", m.baseURL+"/subscriptions/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("renew subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("renew failed: %s - %s", resp.Status, string(respBody))
	}

	slog.Info("renewed subscription", "id", id, "expires", expiry)
	return nil
}

func (m *SubscriptionManager) list(ctx context.Context) ([]Subscription, error) {
	token, err := m.tokenFunc()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", m.baseURL+"/subscriptions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list subscriptions: %s", resp.Status)
	}

	var result struct {
		Value []Subscription `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Value, nil
}

func (m *SubscriptionManager) delete(ctx context.Context, id string) error {
	token, err := m.tokenFunc()
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "DELETE", m.baseURL+"/subscriptions/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// Delete removes the current subscription (call on shutdown)
func (m *SubscriptionManager) Delete(ctx context.Context) error {
	if m.subscriptionID == "" {
		return nil
	}
	slog.Info("deleting subscription", "id", m.subscriptionID)
	return m.delete(ctx, m.subscriptionID)
}

// StartRenewalLoop runs in background to keep subscription alive
func (m *SubscriptionManager) StartRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour) // Renew every 12 hours
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.subscriptionID != "" {
				if err := m.renew(ctx, m.subscriptionID); err != nil {
					slog.Error("subscription renewal failed", "error", err)
					// Try to create new
					if err := m.create(ctx); err != nil {
						slog.Error("failed to create new subscription", "error", err)
					}
				}
			}
		}
	}
}
