package cli

import (
	"encoding/json"
	"fmt"

	"mailflow/internal/config"

	"github.com/spf13/cobra"
)

var destinationsCmd = &cobra.Command{
	Use:   "destinations",
	Short: "List supported correction destinations",
	Args:  cobra.NoArgs,
	RunE:  runDestinations,
}

func init() {
	destinationsCmd.Flags().Bool("json", false, "emit structured JSON")
}

func runDestinations(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	_, rules, err := config.Load(cfgDir)
	if err != nil {
		return err
	}

	destinations := rules.Destinations()
	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		payload := struct {
			Destinations []string `json:"destinations"`
		}{Destinations: destinations}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(payload); err != nil {
			return fmt.Errorf("encode destinations: %w", err)
		}
		return nil
	}

	for _, destination := range destinations {
		fmt.Fprintln(cmd.OutOrStdout(), destination)
	}
	return nil
}
