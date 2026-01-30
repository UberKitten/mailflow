package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mailflow/internal/config"
	"mailflow/internal/engine"
	"mailflow/internal/graph"

	"github.com/spf13/cobra"
)

var processCmd = &cobra.Command{
	Use:   "process",
	Short: "Process new mail in the inbox",
	RunE:  runProcess,
}

func init() {
	processCmd.Flags().Bool("watch", false, "watch continuously (daemon mode)")
	processCmd.Flags().Duration("since", 0, "process mail received since duration (e.g. 1h)")
	processCmd.Flags().Duration("interval", 1*time.Minute, "poll interval for watch mode")
}

func runProcess(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	watch, _ := cmd.Flags().GetBool("watch")
	since, _ := cmd.Flags().GetDuration("since")
	interval, _ := cmd.Flags().GetDuration("interval")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, rules, err := config.Load(cfgDir)
	if err != nil {
		return err
	}

	client, err := graph.NewClient(cfg)
	if err != nil {
		return err
	}

	env := engine.New(cfg, rules, client)

	if watch {
		if err := config.WritePID(cfgDir); err != nil {
			slog.Warn("unable to write pid", "error", err)
		}
		defer config.RemovePID(cfgDir)

		// Handle signals
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			for sig := range sigCh {
				switch sig {
				case syscall.SIGHUP:
					slog.Info("received SIGHUP, reloading config")
					newCfg, newRules, err := config.Load(cfgDir)
					if err != nil {
						slog.Error("reload failed", "error", err)
						continue
					}
					if err := env.Reload(newCfg, newRules); err != nil {
						slog.Error("reload failed", "error", err)
						continue
					}
					slog.Info("config reloaded")
				case syscall.SIGINT, syscall.SIGTERM:
					slog.Info("shutting down")
					cancel()
					return
				}
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			if err := env.ProcessOnce(ctx, since); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				slog.Error("process cycle failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}

	return env.ProcessOnce(ctx, since)
}
