package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// debugEmailCmd is deprecated - use "mailflow debug" instead
var debugEmailCmd = &cobra.Command{
	Use:    "debug-email <message-id>",
	Hidden: true, // Hide from help
	Args:   cobra.ExactArgs(1),
	RunE:   runDebugEmailLegacy,
}

func init() {
	debugEmailCmd.Flags().Bool("apply", false, "actually move the email (not just debug)")
	rootCmd.AddCommand(debugEmailCmd)
}

func runDebugEmailLegacy(cmd *cobra.Command, args []string) error {
	apply, _ := cmd.Flags().GetBool("apply")

	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "┌─────────────────────────────────────────────────────────────┐")
	fmt.Fprintln(os.Stderr, "│  Note: debug-email is now 'mailflow debug <id> [verb]'      │")
	fmt.Fprintln(os.Stderr, "│                                                             │")
	fmt.Fprintln(os.Stderr, "│    mailflow debug <id>         # show matches               │")
	fmt.Fprintln(os.Stderr, "│    mailflow debug <id> move    # move email                 │")
	fmt.Fprintln(os.Stderr, "│    mailflow debug <id> notify  # test pushover              │")
	fmt.Fprintln(os.Stderr, "│    mailflow debug <id> process # full processing            │")
	fmt.Fprintln(os.Stderr, "└─────────────────────────────────────────────────────────────┘")
	fmt.Fprintln(os.Stderr, "")

	// Still run the command - delegate to the new debug command
	verb := "debug"
	if apply {
		verb = "move"
	}
	newArgs := []string{args[0], verb}
	return runDebug(cmd, newArgs)
}
