package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mailflow/internal/config"
	"mailflow/internal/engine"
	"mailflow/internal/graph"

	"github.com/spf13/cobra"
)

var applyActionsCmd = &cobra.Command{
	Use:   "apply-actions [folder]",
	Short: "Apply on_match actions without moving messages",
	Long: `Apply on_match actions (categories, flag, mark_read, pushover) to existing
messages without moving them. Useful for retroactive labeling.

Examples:
  mailflow apply-actions "Inbox/Posts/Gaming" --sender "*@aftermath.site"
  mailflow apply-actions "Inbox/Posts/Politics" --rule posts-politics-currentaffairs
  mailflow apply-actions --message-id <message-id> --rule posts-politics-currentaffairs
`,
	RunE: runApplyActions,
}

func init() {
	rootCmd.AddCommand(applyActionsCmd)
	applyActionsCmd.Flags().String("rule", "", "only apply actions for a specific rule name")
	applyActionsCmd.Flags().String("sender", "", "only apply actions for sender pattern (client-side match)")
	applyActionsCmd.Flags().Duration("since", 0, "only process messages received since duration (e.g. 72h)")
	applyActionsCmd.Flags().Bool("fast", false, "skip body-based rules for speed")
	applyActionsCmd.Flags().Bool("pushover", false, "enable pushover notifications for matched rules")
	applyActionsCmd.Flags().String("message-id", "", "apply actions to a single message (Graph ID or RFC 5322 Message-ID)")
}

func runApplyActions(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	ruleName, _ := cmd.Flags().GetString("rule")
	senderPattern, _ := cmd.Flags().GetString("sender")
	since, _ := cmd.Flags().GetDuration("since")
	fast, _ := cmd.Flags().GetBool("fast")
	allowPushover, _ := cmd.Flags().GetBool("pushover")
	messageID, _ := cmd.Flags().GetString("message-id")

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

	if messageID != "" {
		msg, err := getMessageByID(ctx, client, messageID)
		if err != nil {
			return err
		}

		if senderPattern != "" && !engine.MatchSender(senderPattern, msg.From) {
			fmt.Printf("message sender %s does not match pattern %q\n", msg.From, senderPattern)
			return nil
		}

		rule := selectRuleForMessage(env, msg, ruleName, fast)
		if rule == nil {
			fmt.Println("no rule matched")
			return nil
		}

		env.ApplyOnMatch(ctx, msg.ID, *msg, rule, engine.OnMatchOptions{AllowPushover: allowPushover})
		fmt.Printf("applied actions: %q from %s (rule: %s)\n", msg.Subject, msg.From, rule.Name)
		return nil
	}

	if len(args) < 1 {
		return fmt.Errorf("folder required when --message-id is not set")
	}
	folder := args[0]

	folderID, err := client.FindFolderIDByPath(ctx, folder)
	if err != nil {
		return err
	}

	opts := graph.ListOptions{Since: since, Fast: fast}

	var scanned, matched, applied int
	var lastApplied time.Time

	err = client.StreamMessages(ctx, folderID, opts, func(msg graph.Message) error {
		scanned++

		if senderPattern != "" && !engine.MatchSender(senderPattern, msg.From) {
			return nil
		}

		rule := selectRuleForMessage(env, &msg, ruleName, fast)
		if rule == nil {
			return nil
		}
		matched++

		env.ApplyOnMatch(ctx, msg.ID, msg, rule, engine.OnMatchOptions{AllowPushover: allowPushover})
		applied++
		lastApplied = time.Now()
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("scanned=%d matched=%d applied=%d folder=%s\n", scanned, matched, applied, folder)
	if !lastApplied.IsZero() {
		fmt.Printf("last applied: %s\n", lastApplied.Format(time.RFC3339))
	}

	return nil
}

func selectRuleForMessage(env *engine.Engine, msg *graph.Message, ruleName string, fast bool) *config.Rule {
	if ruleName == "" {
		return engine.Match(env.Rules(), *msg, engine.MatchOptions{Fast: fast})
	}

	// Use debug match to evaluate a specific rule
	debug, err := env.MatchWithDebug(msg)
	if err != nil {
		return nil
	}
	for _, dr := range debug.Rules {
		if dr.Rule.Name == ruleName && dr.Matched {
			return dr.Rule
		}
	}
	return nil
}

// getMessageByID fetches a message by Graph ID or RFC 5322 Message-ID
func getMessageByID(ctx context.Context, client *graph.Client, msgID string) (*graph.Message, error) {
	// Detect if this is an RFC 5322 Message-ID (contains @ or angle brackets)
	if strings.Contains(msgID, "@") || strings.HasPrefix(msgID, "<") {
		if !strings.HasPrefix(msgID, "<") {
			msgID = "<" + msgID + ">"
		}
		return client.GetMessageByInternetMessageID(ctx, msgID)
	}
	return client.GetMessage(ctx, msgID)
}
