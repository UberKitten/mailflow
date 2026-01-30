package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	// Start server FIRST (so it's ready for Graph validation)
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	// Give server a moment to start listening
	time.Sleep(500 * time.Millisecond)

	// Start renewal loop
	go subMgr.StartRenewalLoop(ctx)

	// Now create subscription (server is ready to handle validation)
	slog.Info("creating Graph subscription", "url", cfg.Webhook.ExternalURL)
	if err := subMgr.CreateOrRenew(ctx); err != nil {
		slog.Error("failed to create subscription", "error", err)
		// Continue anyway - server will still work for manual testing
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
