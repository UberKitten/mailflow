package cli

import (
	"context"
	"fmt"
	"strings"

	"mailflow/internal/config"
	"mailflow/internal/engine"
	"mailflow/internal/graph"

	"github.com/spf13/cobra"
)

var debugCmd = &cobra.Command{
	Use:   "debug <message-id> [verb]",
	Short: "Debug rule matching for a single email",
	Long: `Debug rule matching for a single email.

Verbs:
  (none)    Show which rules match (default)
  move      Match rules and move to folder
  label     Apply on_match actions (categories, flags) without moving
  notify    Send pushover for matching notify_only rules
  process   Full processing: move + label + notify

Examples:
  mailflow debug '<message-id>'              # show matches
  mailflow debug '<message-id>' move         # debug + move
  mailflow debug '<message-id>' notify       # test pushover
  mailflow debug '<message-id>' process      # full processing`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runDebug,
}

func init() {
	rootCmd.AddCommand(debugCmd)
}

func runDebug(cmd *cobra.Command, args []string) error {
	msgID := args[0]
	verb := "debug"
	if len(args) > 1 {
		verb = strings.ToLower(args[1])
	}

	// Validate verb
	validVerbs := map[string]bool{
		"debug": true, "move": true, "label": true, "notify": true, "process": true,
	}
	if !validVerbs[verb] {
		return fmt.Errorf("unknown verb %q (valid: debug, move, label, notify, process)", verb)
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

	// Detect if this is an RFC 5322 Message-ID (contains @ or angle brackets)
	// vs a Graph API message ID
	var msg *graph.Message
	if strings.Contains(msgID, "@") || strings.HasPrefix(msgID, "<") {
		// Ensure angle brackets are present
		if !strings.HasPrefix(msgID, "<") {
			msgID = "<" + msgID + ">"
		}
		msg, err = client.GetMessageByInternetMessageID(ctx, msgID)
	} else {
		msg, err = client.GetMessage(ctx, msgID)
	}
	if err != nil {
		return err
	}

	// Always show debug output first
	result, err := env.MatchWithDebug(msg)
	if err != nil {
		return err
	}

	fmt.Printf("Email: %q from %s\n\n", msg.Subject, msg.From)
	fmt.Printf("Checking %d rules...\n\n", len(result.Rules))

	for _, rule := range result.Rules {
		ruleLabel := rule.Rule.Source
		if ruleLabel == "" {
			ruleLabel = rule.Rule.Name
		}
		status := "✗"
		if rule.Matched {
			status = "✓"
		}
		line := fmt.Sprintf("%s %s", status, ruleLabel)
		if rule.Matched {
			line += fmt.Sprintf(" - MATCHED → %s", rule.Rule.Folder)
		}
		fmt.Println(line)

		if len(rule.Conditions) == 0 {
			fmt.Println("  └─ no conditions")
			fmt.Println()
			continue
		}

		for _, cond := range rule.Conditions {
			condStatus := "match"
			if !cond.Matched {
				condStatus = "no match"
			}

			details := fmt.Sprintf("%s: %s", cond.Name, condStatus)
			got := formatConditionValue(cond.Got, cond.GotList)
			want := formatList(cond.Want)
			if got != "" || want != "" {
				details += fmt.Sprintf(" (got: %s, want: %s)", got, want)
			}
			if len(cond.MatchedValues) > 0 {
				details += fmt.Sprintf(" (matched: %s)", formatList(cond.MatchedValues))
			}
			if cond.Note != "" {
				details += fmt.Sprintf(" (%s)", cond.Note)
			}

			fmt.Printf("  └─ %s\n", details)
		}
		fmt.Println()
	}

	// Now handle the verb
	switch verb {
	case "debug":
		// Just show what would happen
		if result.MatchedRule != nil {
			fmt.Printf("Result: Would move to %s (rule: %s)\n", result.MatchedRule.Folder, result.MatchedRule.Source)
		} else {
			fmt.Println("Result: No rule matched")
		}

	case "move":
		// Move the email
		if result.MatchedRule != nil {
			if err := env.ProcessSingle(ctx, msg.ID); err != nil {
				return fmt.Errorf("failed to move: %w", err)
			}
			fmt.Printf("Result: Moved to %s (rule: %s)\n", result.MatchedRule.Folder, result.MatchedRule.Source)
		} else {
			fmt.Println("Result: No rule matched, not moved")
		}

	case "label":
		// Apply on_match actions without moving
		if result.MatchedRule != nil {
			env.ApplyOnMatch(ctx, msg.ID, *msg, result.MatchedRule, engine.OnMatchOptions{AllowPushover: false})
			fmt.Printf("Result: Applied labels/categories (rule: %s)\n", result.MatchedRule.Source)
		} else {
			fmt.Println("Result: No rule matched, no labels to apply")
		}

	case "notify":
		// Send pushover notifications for notify_only rules
		notifyRules := engine.MatchNotifyOnly(env.Rules(), *msg)
		if len(notifyRules) > 0 {
			for _, notifyRule := range notifyRules {
				env.ApplyOnMatch(ctx, msg.ID, *msg, notifyRule, engine.OnMatchOptions{AllowPushover: true})
				fmt.Printf("Result: Sent pushover (rule: %s)\n", notifyRule.Name)
			}
		} else {
			fmt.Println("Result: No notify_only rules matched")
		}

	case "process":
		// Full processing: move + labels + notify
		if err := env.ProcessSingle(ctx, msg.ID); err != nil {
			return fmt.Errorf("failed to process: %w", err)
		}
		if result.MatchedRule != nil {
			fmt.Printf("Result: Processed (moved to %s)\n", result.MatchedRule.Folder)
		} else {
			fmt.Println("Result: Processed (no move, applied actions)")
		}
	}

	return nil
}

func formatConditionValue(value string, values []string) string {
	if len(values) > 0 {
		return formatList(values)
	}
	if value == "" {
		return ""
	}
	return fmt.Sprintf("%q", formatValue(value))
}

func formatList(values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func formatValue(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\t", " ")
	value = strings.TrimSpace(value)
	if len(value) > 160 {
		return value[:157] + "..."
	}
	return value
}
