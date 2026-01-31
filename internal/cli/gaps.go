package cli

import (
	"context"
	"fmt"
	"sort"

	"mailflow/internal/config"
	"mailflow/internal/engine"
	"mailflow/internal/graph"

	"github.com/spf13/cobra"
)

var gapsCmd = &cobra.Command{
	Use:   "gaps <folder>",
	Short: "Find emails that do not match any rule",
	Args:  cobra.ExactArgs(1),
	RunE:  runGaps,
}

func init() {
	gapsCmd.Flags().Int("top", 50, "show top N senders")
	gapsCmd.Flags().Bool("fast", false, "skip body-based rules")
	gapsCmd.Flags().Bool("recursive", false, "scan subfolders recursively")
}

func runGaps(cmd *cobra.Command, args []string) error {
	folder := args[0]
	top, _ := cmd.Flags().GetInt("top")
	fast, _ := cmd.Flags().GetBool("fast")
	recursive, _ := cmd.Flags().GetBool("recursive")
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

	result, err := env.Gaps(ctx, folder, engine.GapsOptions{Fast: fast, Recursive: recursive})
	if err != nil {
		return err
	}

	type pair struct {
		Domain string
		Count  int
	}
	var pairs []pair
	for domain, count := range result.ByDomain {
		pairs = append(pairs, pair{Domain: domain, Count: count})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Count > pairs[j].Count })

	if top > 0 && len(pairs) > top {
		pairs = pairs[:top]
	}

	for _, p := range pairs {
		fmt.Printf("%s\t%d\n", p.Domain, p.Count)
	}

	return nil
}
