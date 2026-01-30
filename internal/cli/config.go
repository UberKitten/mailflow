package cli

import (
	"fmt"

	"mailflow/internal/config"

	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Config operations",
}

var configCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate configuration",
	RunE:  runConfigCheck,
}

func init() {
	configCmd.AddCommand(configCheckCmd)
	configCheckCmd.Flags().Bool("verbose", false, "print parsed config summary")
}

func runConfigCheck(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	verbose, _ := cmd.Flags().GetBool("verbose")
	_, rules, err := config.Load(cfgDir)
	if err != nil {
		return err
	}
	if verbose {
		fmt.Printf("Config loaded: %s\n", cfgDir)
		fmt.Printf("Rules: %d\n", len(rules.Rules))
		fmt.Printf("Folders: %d\n", len(rules.Folders))
	}
	fmt.Println("ok")
	return nil
}
