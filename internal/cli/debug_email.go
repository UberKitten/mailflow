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

var debugEmailCmd = &cobra.Command{
	Use:   "debug-email <message-id>",
	Short: "Debug rule matching for a single email",
	Args:  cobra.ExactArgs(1),
	RunE:  runDebugEmail,
}

func init() {
	debugEmailCmd.Flags().Bool("apply", false, "actually move the email (not just debug)")
	rootCmd.AddCommand(debugEmailCmd)
}

func runDebugEmail(cmd *cobra.Command, args []string) error {
	msgID := args[0]
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

	apply, _ := cmd.Flags().GetBool("apply")

	if result.MatchedRule != nil {
		if apply {
			if err := env.ProcessSingle(ctx, msg.ID); err != nil {
				return fmt.Errorf("failed to move: %w", err)
			}
			fmt.Printf("Result: Moved to %s (rule: %s)\n", result.MatchedRule.Folder, result.MatchedRule.Source)
		} else {
			fmt.Printf("Result: Would move to %s (rule: %s)\n", result.MatchedRule.Folder, result.MatchedRule.Source)
		}
	} else {
		fmt.Println("Result: No rule matched")
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
