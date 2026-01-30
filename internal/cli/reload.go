package cli

import (
	"fmt"
	"os"
	"syscall"

	"mailflow/internal/config"

	"github.com/spf13/cobra"
)

var reloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Send SIGHUP to running mailflow daemon",
	RunE:  runReload,
}

func runReload(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	pid, err := config.ReadPID(cfgDir)
	if err != nil {
		return err
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if err := process.Signal(syscall.SIGHUP); err != nil {
		return err
	}

	fmt.Printf("sent SIGHUP to pid %d\n", pid)
	return nil
}
