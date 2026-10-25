package webhook

import (
	"context"
	"crypto/subtle"
	"fmt"
)

// NewIncomingSubscriptionManager uses a distinct URL and only adopts/deletes
// subscriptions authenticated by this capture's private persistent state.
func NewIncomingSubscriptionManager(baseURL string, tokenFunc func() (string, error), notifyURL string, capture *IncomingReview) *SubscriptionManager {
	manager := NewSubscriptionManager(baseURL, tokenFunc, notifyURL, capture.ClientState(), "users/"+capture.userID+"/messages")
	manager.incoming = capture
	manager.subscriptionID = capture.subscriptionID()
	return manager
}

func (m *SubscriptionManager) createOrRenewIncoming(ctx context.Context) error {
	existing, err := m.list(ctx)
	if err != nil {
		return fmt.Errorf("list incoming review subscriptions: %w", err)
	}
	ownedID := m.incoming.subscriptionID()
	var currentID string
	for _, sub := range existing {
		if sub.NotificationURL != m.notifyURL || sub.Resource != m.resource || sub.ChangeType != "created" || subtle.ConstantTimeCompare([]byte(sub.ClientState), []byte(m.clientState)) != 1 {
			continue
		}
		if !validIncomingSubscriptionID(sub.ID) {
			return fmt.Errorf("invalid incoming review subscription ID")
		}
		if ownedID == "" || sub.ID == ownedID {
			currentID = sub.ID
			break
		}
	}
	if currentID != "" {
		if err := m.renew(ctx, currentID); err == nil {
			if err := m.incoming.setSubscriptionID(currentID); err != nil {
				return err
			}
			m.subscriptionID = currentID
			return nil
		}
		// Only this exact, authenticated owned subscription can be replaced.
		if err := m.delete(ctx, currentID); err != nil {
			return fmt.Errorf("delete unrenewable incoming review subscription: %w", err)
		}
	}
	if err := m.incoming.setSubscriptionID(""); err != nil {
		return err
	}
	m.subscriptionID = ""
	return m.create(ctx)
}
