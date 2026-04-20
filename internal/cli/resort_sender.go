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

var resortSenderCmd = &cobra.Command{
	Use:   "resort-sender <folder> <sender-pattern>",
	Short: "Re-sort messages from a specific sender in a folder",
	Long: `Re-sort messages matching a sender pattern in a folder.

The sender pattern supports wildcards:
  *@example.com      - all emails from example.com domain
  user@example.com   - exact email address
  *news*@*           - any address containing "news"

Examples:
  mailflow resort-sender Posts "*@portlandmercury.com" --apply
  mailflow resort-sender Inbox "newsletter@*" --dry-run
  mailflow resort-sender "Old/To Read" "*@substack.com" --apply --recursive`,
	Args: cobra.ExactArgs(2),
	RunE: runResortSender,
}

func init() {
	resortSenderCmd.Flags().Bool("dry-run", false, "preview moves without applying")
	resortSenderCmd.Flags().Bool("apply", false, "apply moves")
	resortSenderCmd.Flags().Bool("recursive", false, "scan subfolders recursively")
	resortSenderCmd.Flags().Duration("since", 0, "only process messages received since duration")
	resortSenderCmd.Flags().Bool("fast", false, "skip body-based rules and fetch minimal fields")
}

func runResortSender(cmd *cobra.Command, args []string) error {
	folder := args[0]
	senderPattern := args[1]

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	apply, _ := cmd.Flags().GetBool("apply")
	recursive, _ := cmd.Flags().GetBool("recursive")
	since, _ := cmd.Flags().GetDuration("since")
	fast, _ := cmd.Flags().GetBool("fast")
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")

	if !dryRun && !apply {
		return errors.New("must specify --dry-run or --apply")
	}
	if dryRun && apply {
		return errors.New("cannot use --dry-run and --apply together")
	}

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

	report, err := env.ResortSender(ctx, folder, senderPattern, engine.ResortSenderOptions{
		DryRun:    dryRun,
		Recursive: recursive,
		Since:     since,
		Fast:      fast,
		ConfigDir: cfgDir,
	})
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

	slog.Info("resort-sender complete",
		"pattern", senderPattern,
		"scanned", report.Scanned,
		"matched", report.Matched,
		"moved", report.Moved,
		"unmatched", report.Unmatched,
		"duration", time.Since(report.StartedAt))
	return nil
}
