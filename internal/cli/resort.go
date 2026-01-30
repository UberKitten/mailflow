package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"mailflow/internal/config"
	"mailflow/internal/engine"
	"mailflow/internal/graph"

	"github.com/spf13/cobra"
)

var resortCmd = &cobra.Command{
	Use:   "resort <folder>",
	Short: "Re-sort existing messages in a folder",
	Args:  cobra.ExactArgs(1),
	RunE:  runResort,
}

func init() {
	resortCmd.Flags().Bool("dry-run", false, "preview moves without applying")
	resortCmd.Flags().Bool("apply", false, "apply moves")
	resortCmd.Flags().Bool("recursive", false, "scan subfolders recursively")
	resortCmd.Flags().Duration("since", 0, "only process messages received since duration")
	resortCmd.Flags().Bool("fast", false, "skip body-based rules and fetch minimal fields")
}

func runResort(cmd *cobra.Command, args []string) error {
	folder := args[0]
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	apply, _ := cmd.Flags().GetBool("apply")
	recursive, _ := cmd.Flags().GetBool("recursive")
	since, _ := cmd.Flags().GetDuration("since")
	fast, _ := cmd.Flags().GetBool("fast")

	if !dryRun && !apply {
		return errors.New("must specify --dry-run or --apply")
	}
	if dryRun && apply {
		return errors.New("cannot use --dry-run and --apply together")
	}

	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	cfg, rules, err := config.Load(cfgDir)
	if err != nil {
		return err
	}

	client, err := graph.NewClient(cfg)
	if err != nil {
		return err
	}

	env := engine.New(cfg, rules, client)
	ctx := context.Background()

	report, err := env.Resort(ctx, folder, engine.ResortOptions{DryRun: dryRun, Recursive: recursive, Since: since, Fast: fast})
	if err != nil {
		return err
	}

	for _, move := range report.Moves {
		if dryRun {
			fmt.Printf("DRY-RUN: %s -> %s | %s | %s\n", move.FromFolder, move.ToFolder, move.From, move.Subject)
		} else {
			fmt.Printf("MOVED: %s -> %s | %s | %s\n", move.FromFolder, move.ToFolder, move.From, move.Subject)
		}
	}

	slog.Info("resort complete", "processed", report.Total, "moved", report.Moved, "unmatched", report.Unmatched, "duration", time.Since(report.StartedAt))
	return nil
}
