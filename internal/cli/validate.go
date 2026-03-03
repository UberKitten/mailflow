package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mailflow/internal/config"

	"github.com/spf13/cobra"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate rules configuration",
	RunE:  runValidate,
}

func init() {
	validateCmd.Flags().Bool("strict", false, "treat warnings as errors")
	validateCmd.Flags().Bool("diff", false, "limit overlap warnings to changed rules")
}

func runValidate(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")
	strict, _ := cmd.Flags().GetBool("strict")
	diffFlag, _ := cmd.Flags().GetBool("diff")

	_, rules, err := config.Load(cfgDir)
	if err != nil {
		return err
	}

	diffInfo, err := detectDiffRules(cfgDir, diffFlag)
	if err != nil {
		return err
	}

	opts := config.ValidateOptions{}
	if diffInfo.Enabled {
		opts.OnlyRules = diffInfo.ChangedRules
		opts.Detailed = true
	}

	report := config.ValidateRuleSet(rules, opts)
	warningCount := len(report.Warnings)
	if strict {
		report.ApplyStrict()
	}

	out := cmd.OutOrStdout()
	for _, issue := range report.Errors {
		fmt.Fprintf(out, "ERROR: %s\n", issue.Message)
	}
	for _, issue := range report.Warnings {
		fmt.Fprintf(out, "WARNING: %s\n", issue.Message)
	}
	if len(report.Errors) == 0 && len(report.Warnings) == 0 {
		fmt.Fprintln(out, "ok")
	}
	if diffInfo.Enabled {
		fmt.Fprintf(out, "%d rules changed, %d warnings\n", diffInfo.ChangedCount, warningCount)
	}

	if len(report.Errors) > 0 {
		return errors.New("validation failed")
	}
	return nil
}

type diffResult struct {
	Enabled      bool
	ChangedRules map[string]bool
	ChangedCount int
}

func detectDiffRules(cfgDir string, force bool) (diffResult, error) {
	gitRoot, err := gitRootForDir(cfgDir)
	if err != nil {
		if force {
			return diffResult{}, err
		}
		return diffResult{}, nil
	}

	dirty, err := gitHasChanges(gitRoot)
	if err != nil {
		return diffResult{}, err
	}

	var baseRef string
	if dirty {
		baseRef = "HEAD"
	} else {
		if !gitHasRef(gitRoot, "HEAD~1") {
			if force {
				return diffResult{}, fmt.Errorf("diff requested but no prior commit found")
			}
			return diffResult{}, nil
		}
		baseRef = "HEAD~1"
	}

	files, err := gitChangedFiles(gitRoot, baseRef, dirty)
	if err != nil {
		return diffResult{}, err
	}

	ruleFiles := filterRuleFiles(gitRoot, cfgDir, files)
	changedRules, err := changedRulesFromFiles(gitRoot, baseRef, ruleFiles)
	if err != nil {
		return diffResult{}, err
	}

	return diffResult{
		Enabled:      true,
		ChangedRules: changedRules,
		ChangedCount: len(changedRules),
	}, nil
}

func gitRootForDir(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git root: %s", strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func gitHasChanges(gitRoot string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = gitRoot
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func gitHasRef(gitRoot, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", ref)
	cmd.Dir = gitRoot
	return cmd.Run() == nil
}

func gitChangedFiles(gitRoot, baseRef string, dirty bool) ([]string, error) {
	args := []string{"diff", "--name-only", "--diff-filter=AMR"}
	if dirty {
		args = append(args, baseRef)
	} else {
		args = append(args, baseRef, "HEAD")
	}
	files, err := gitLines(gitRoot, args...)
	if err != nil {
		return nil, err
	}

	if dirty {
		untracked, err := gitLines(gitRoot, "ls-files", "--others", "--exclude-standard")
		if err != nil {
			return nil, err
		}
		files = append(files, untracked...)
	}

	return uniqueStrings(files), nil
}

func gitLines(gitRoot string, args ...string) ([]string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = gitRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

func filterRuleFiles(gitRoot, cfgDir string, files []string) []string {
	relConfigDir, err := filepath.Rel(gitRoot, cfgDir)
	if err != nil {
		return nil
	}

	relConfigDir = filepath.ToSlash(relConfigDir)
	if relConfigDir == "." {
		relConfigDir = ""
	}

	prefix := relConfigDir
	if prefix != "" {
		prefix += "/"
	}
	prefix += "rules.d/"

	var ruleFiles []string
	for _, file := range files {
		file = filepath.ToSlash(file)
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		if strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml") {
			ruleFiles = append(ruleFiles, file)
		}
	}
	return uniqueStrings(ruleFiles)
}

func changedRulesFromFiles(gitRoot, baseRef string, files []string) (map[string]bool, error) {
	changed := map[string]bool{}

	for _, file := range files {
		currentPath := filepath.Join(gitRoot, filepath.FromSlash(file))
		currentData, err := os.ReadFile(currentPath)
		if err != nil {
			continue
		}
		currentRules, err := config.ParseRuleFile(currentData, file)
		if err != nil {
			return nil, fmt.Errorf("parse rules file %s: %w", file, err)
		}

		currentSignatures, err := ruleSignatureMap(currentRules)
		if err != nil {
			return nil, fmt.Errorf("rules signature %s: %w", file, err)
		}

		baseData, err := gitShowFile(gitRoot, baseRef, file)
		baseSignatures := map[string]string{}
		if err == nil {
			baseRules, err := config.ParseRuleFile(baseData, file)
			if err != nil {
				return nil, fmt.Errorf("parse base rules file %s: %w", file, err)
			}
			baseSignatures, err = ruleSignatureMap(baseRules)
			if err != nil {
				return nil, fmt.Errorf("base rules signature %s: %w", file, err)
			}
		}

		for name, signature := range currentSignatures {
			baseSignature, ok := baseSignatures[name]
			if !ok || baseSignature != signature {
				changed[name] = true
			}
		}
	}

	return changed, nil
}

func ruleSignatureMap(rules []config.Rule) (map[string]string, error) {
	result := map[string]string{}
	for _, rule := range rules {
		if rule.Name == "" {
			continue
		}
		signature, err := config.RuleSignature(rule)
		if err != nil {
			return nil, err
		}
		result[rule.Name] = signature
	}
	return result, nil
}

func gitShowFile(gitRoot, ref, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", fmt.Sprintf("%s:%s", ref, path))
	cmd.Dir = gitRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return output, nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
