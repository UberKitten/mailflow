package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"mailflow/internal/config"
)

var debugRulesCmd = &cobra.Command{
	Use:   "debug-rules",
	Short: "Debug rule loading",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgDir, _ := cmd.Flags().GetString("config-dir")
		cfg, rules, err := config.Load(cfgDir)
		if err != nil {
			return err
		}
		_ = cfg

		fmt.Println("Rules in order:")
		for i, r := range rules.Rules {
			fmt.Printf("%d. %s -> %s\n", i+1, r.Name, r.Folder)
			if len(r.FromDomain) > 0 {
				// Check if portlandmercury.com is in there
				for _, d := range r.FromDomain {
					if strings.Contains(strings.ToLower(d), "portland") {
						fmt.Printf("   from_domain includes: %s\n", d)
					}
				}
			}
			if len(r.SubjectContains) > 0 {
				for _, s := range r.SubjectContains {
					if strings.Contains(strings.ToLower(s), "miss") {
						fmt.Printf("   subject_contains includes: %s\n", s)
					}
				}
			}
		}
		return nil
	},
}

func init() {
	configCmd.AddCommand(debugRulesCmd)
}
