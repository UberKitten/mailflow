package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"mailflow/internal/config"
	"mailflow/internal/webhook"

	"github.com/spf13/cobra"
)

var incomingReviewEventsCmd = &cobra.Command{
	Use:   "incoming-review-events",
	Short: "Dump durable incoming review metadata without fetching mail",
	Args:  cobra.NoArgs,
	RunE:  runIncomingReviewEvents,
}

func init() {
	rootCmd.AddCommand(incomingReviewEventsCmd)
}

func runIncomingReviewEvents(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	cfg, err := config.LoadMainConfig(cfgDir)
	if err != nil {
		return err
	}
	events := make([]webhook.IncomingReviewEvent, 0)
	if cfg.IncomingReview != nil {
		events, err = webhook.ReadIncomingReviewEvents(cfg.IncomingReview.SpoolDir, cfg.IncomingReview.Mailbox)
		if err != nil {
			return fmt.Errorf("read incoming review events: %w", err)
		}
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
		Events []webhook.IncomingReviewEvent `json:"events"`
	}{Events: events})
}

func configureIncomingReview(cfg *config.Config, server *webhook.Server, tokenFunc func() (string, error)) (*webhook.SubscriptionManager, error) {
	if cfg.IncomingReview == nil {
		return nil, nil
	}
	capture, err := webhook.NewIncomingReview(cfg.IncomingReview.SpoolDir, cfg.Graph.TenantID, cfg.IncomingReview.MailboxObjectID, cfg.IncomingReview.Mailbox)
	if err != nil {
		return nil, fmt.Errorf("initialize incoming review: %w", err)
	}
	server.SetIncomingReview(cfg.IncomingReview.Path, capture)
	return webhook.NewIncomingSubscriptionManager(cfg.Graph.BaseURL, tokenFunc, cfg.IncomingReview.ExternalURL, capture), nil
}

// The extra source uses the existing subscription retry and renewal cadence,
// independently of sorting fallback. Only this goroutine owns its manager.
func runIncomingReviewSubscription(ctx context.Context, manager *webhook.SubscriptionManager, retryInterval time.Duration) {
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Delete(shutdownCtx); err != nil {
			slog.Warn("failed to delete incoming review subscription", "error", err)
		}
	}()
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := manager.CreateOrRenew(attemptCtx)
		cancel()
		interval := 12 * time.Hour
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("incoming review subscription unavailable", "error", err)
			interval = retryInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func sameIncomingReviewConfig(old, next *config.Config) bool {
	if old.IncomingReview == nil || next.IncomingReview == nil {
		return old.IncomingReview == nil && next.IncomingReview == nil
	}
	return *old.IncomingReview == *next.IncomingReview && old.Graph.TenantID == next.Graph.TenantID
}
