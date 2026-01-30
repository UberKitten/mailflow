package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"mailflow/internal/config"
	"mailflow/internal/engine"
	"mailflow/internal/graph"
	"mailflow/internal/webhook"

	"github.com/spf13/cobra"
)

var webhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Run webhook server for real-time mail processing",
	Long: `Start an HTTP server that receives notifications from Microsoft Graph
when new mail arrives. This provides instant mail processing instead of polling.

The webhook server handles:
- Graph subscription lifecycle (create, renew, delete)
- Incoming notification validation
- Real-time mail processing

Requires webhook config in config.yaml:
  webhook:
    enabled: true
    port: 8792
    path: /webhook
    external_url: https://mailflow.example.com/webhook
`,
	RunE: runWebhook,
}

func init() {
	rootCmd.AddCommand(webhookCmd)
}

func runWebhook(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	cfg, rules, err := config.Load(cfgDir)
	if err != nil {
		return err
	}

	if !cfg.Webhook.Enabled {
		slog.Error("webhook not enabled in config")
		return nil
	}

	if cfg.Webhook.Port == 0 {
		cfg.Webhook.Port = 8792
	}
	if cfg.Webhook.Path == "" {
		cfg.Webhook.Path = "/webhook"
	}

	client, err := graph.NewClient(cfg)
	if err != nil {
		return err
	}

	eng := engine.New(cfg, rules, client)

	// Create webhook server with handler
	whCfg := webhook.WebhookConfig{
		Enabled:     cfg.Webhook.Enabled,
		Port:        cfg.Webhook.Port,
		Path:        cfg.Webhook.Path,
		ExternalURL: cfg.Webhook.ExternalURL,
	}

	handler := func(ctx context.Context, messageID string) error {
		return eng.ProcessSingle(ctx, messageID)
	}

	server := webhook.New(whCfg, handler)

	// Create subscription manager
	subMgr := webhook.NewSubscriptionManager(
		cfg.Graph.BaseURL,
		client.GetToken,
		cfg.Webhook.ExternalURL,
		server.ClientState(),
	)

	// Write PID file
	if err := config.WritePID(cfgDir); err != nil {
		slog.Warn("unable to write pid", "error", err)
	}
	defer config.RemovePID(cfgDir)

	// Set up signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pollInterval := time.Duration(cfg.Webhook.PollIntervalSeconds) * time.Second
	retryInterval := time.Duration(cfg.Webhook.RetryIntervalSeconds) * time.Second

	var pollMu sync.Mutex
	polling := false
	var pollCancel context.CancelFunc

	startPolling := func() {
		pollMu.Lock()
		defer pollMu.Unlock()
		if polling {
			return
		}
		polling = true
		pollCtx, cancelPoll := context.WithCancel(ctx)
		pollCancel = cancelPoll
		slog.Warn("starting polling fallback", "interval", pollInterval)
		go func() {
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()
			for {
				if err := eng.ProcessOnce(pollCtx, 0); err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					slog.Error("polling cycle failed", "error", err)
				}
				select {
				case <-pollCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	stopPolling := func() {
		pollMu.Lock()
		defer pollMu.Unlock()
		if !polling {
			return
		}
		polling = false
		if pollCancel != nil {
			pollCancel()
		}
		slog.Info("polling fallback stopped")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	// Start server FIRST (so it's ready for Graph validation)
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	// Give server time to start listening
	time.Sleep(2 * time.Second)

	// Verify server is responding before creating subscription
	healthURL := fmt.Sprintf("http://localhost:%d/health", cfg.Webhook.Port)
	for i := 0; i < 5; i++ {
		resp, err := http.Get(healthURL)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			slog.Info("webhook server ready")
			break
		}
		if i == 4 {
			slog.Warn("webhook server health check failed, continuing anyway")
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Start renewal loop
	go subMgr.StartRenewalLoop(ctx)

	// Now create subscription (server is ready to handle validation)
	slog.Info("creating Graph subscription", "url", cfg.Webhook.ExternalURL)
	if err := subMgr.CreateOrRenew(ctx); err != nil {
		slog.Error("failed to create subscription", "error", err)
		startPolling()

		go func() {
			ticker := time.NewTicker(retryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					slog.Info("retrying Graph subscription", "interval", retryInterval)
					if err := subMgr.CreateOrRenew(ctx); err != nil {
						slog.Warn("subscription retry failed", "error", err)
						continue
					}
					slog.Info("Graph subscription active, disabling polling fallback")
					stopPolling()
					return
				}
			}
		}()
	}

	// Wait for shutdown signal or error
	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				slog.Info("received SIGHUP, reloading config")
				newCfg, newRules, err := config.Load(cfgDir)
				if err != nil {
					slog.Error("reload failed", "error", err)
					continue
				}
				if err := eng.Reload(newCfg, newRules); err != nil {
					slog.Error("reload failed", "error", err)
					continue
				}
				slog.Info("config reloaded")
			case syscall.SIGINT, syscall.SIGTERM:
				slog.Info("received signal, shutting down", "signal", sig)
				cancel()
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*1e9)
				defer shutdownCancel()
				return server.Stop(shutdownCtx)
			}
		case err := <-errCh:
			if err != nil {
				slog.Error("server error", "error", err)
				return err
			}
			return nil
		}
	}
}
