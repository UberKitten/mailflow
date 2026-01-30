package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Config is the main config file (config.yaml)
type Config struct {
	Include  []string      `yaml:"include"`
	Graph    GraphConfig   `yaml:"graph"`
	Pushover Pushover      `yaml:"pushover"`
	Process  ProcessConfig `yaml:"process"`
}

type GraphConfig struct {
	TokenScript           string `yaml:"token_script"`
	BaseURL               string `yaml:"base_url"`
	MaxConcurrentRequests int    `yaml:"max_concurrent_requests"`
	RangeWorkers          int    `yaml:"range_workers"`
	LargeFolderThreshold  int    `yaml:"large_folder_threshold"`
	RangeDays             int    `yaml:"range_days"`
}

type Pushover struct {
	Token string `yaml:"token"`
	User  string `yaml:"user"`
}

type ProcessConfig struct {
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`
	ResortWorkers       int `yaml:"resort_workers"`
}

// SenderList defines a reusable sender list.
type SenderList struct {
	Name      string   `yaml:"name"`
	Domains   []string `yaml:"domains"`
	Addresses []string `yaml:"addresses"`
}

// RuleSet is the final compiled config used for matching.
type RuleSet struct {
	Rules   []Rule
	Folders map[string]string // folder key -> path
}

// Rule definition.
type Rule struct {
	Name            string
	Folder          string
	From            []string
	To              []string
	FromDomain      []string
	ToDomain        []string
	SubjectContains []string
	BodyContains    []string
	CaseInsensitive bool
	OnMatch         *OnMatch

	fromDomainRefs []string
	toDomainRefs   []string
}

type OnMatch struct {
	MarkRead bool          `yaml:"mark_read"`
	Pushover *PushoverRule `yaml:"pushover"`
}

type PushoverRule struct {
	Title    string           `yaml:"title"`
	Message  string           `yaml:"message"`
	Fallback string           `yaml:"fallback"`
	Extract  []ExtractPattern `yaml:"extract"`
}

type ExtractPattern struct {
	Pattern string `yaml:"pattern"`
	Capture string `yaml:"capture"`
}

// RuleFile represents a rules file.
type RuleFile struct {
	Version int                    `yaml:"version"`
	Folders map[string]FolderRules `yaml:"folders"`
	Rules   []Rule                 `yaml:"rules"`
}

type FolderRules struct {
	Path  string `yaml:"path"`
	Rules []Rule `yaml:"rules"`
}

// Load reads config.yaml, sender lists, and rules.
func Load(configDir string) (*Config, *RuleSet, error) {
	cfg, err := loadMainConfig(configDir)
	if err != nil {
		return nil, nil, err
	}

	senders, err := loadSenders(configDir)
	if err != nil {
		return nil, nil, err
	}

	ruleset, err := loadRules(configDir, cfg, senders)
	if err != nil {
		return nil, nil, err
	}

	return cfg, ruleset, nil
}

func loadMainConfig(configDir string) (*Config, error) {
	path := filepath.Join(configDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config.yaml: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}

	if cfg.Graph.TokenScript == "" {
		home, _ := os.UserHomeDir()
		cfg.Graph.TokenScript = filepath.Join(home, "bin", "ms-graph-token.sh")
	}
	if cfg.Graph.BaseURL == "" {
		cfg.Graph.BaseURL = "https://graph.microsoft.com/v1.0"
	}
	if cfg.Graph.MaxConcurrentRequests == 0 {
		cfg.Graph.MaxConcurrentRequests = 12
	}
	if cfg.Graph.RangeWorkers == 0 {
		cfg.Graph.RangeWorkers = 6
	}
	if cfg.Graph.LargeFolderThreshold == 0 {
		cfg.Graph.LargeFolderThreshold = 5000
	}
	if cfg.Graph.RangeDays == 0 {
		cfg.Graph.RangeDays = 7
	}
	if cfg.Process.ResortWorkers == 0 {
		cfg.Process.ResortWorkers = 6
	}

	return &cfg, nil
}

func loadSenders(configDir string) (map[string]SenderList, error) {
	path := filepath.Join(configDir, "senders.d")
	senders := map[string]SenderList{}

	err := filepath.WalkDir(path, func(entry string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(d.Name())
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(entry)
		if err != nil {
			return err
		}
		var list SenderList
		if err := yaml.Unmarshal(data, &list); err != nil {
			return fmt.Errorf("parse sender list %s: %w", entry, err)
		}
		key := list.Name
		if key == "" {
			base := filepath.Base(entry)
			key = base[:len(base)-len(filepath.Ext(base))]
		}
		senders[key] = list
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	return senders, nil
}

func loadRules(configDir string, cfg *Config, senders map[string]SenderList) (*RuleSet, error) {
	includes := cfg.Include
	if len(includes) == 0 {
		includes = []string{"rules.d/*.yaml", "rules.d/*.yml"}
	}

	var files []string
	for _, pattern := range includes {
		matches, err := filepath.Glob(filepath.Join(configDir, pattern))
		if err != nil {
			return nil, fmt.Errorf("bad include glob %s: %w", pattern, err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no rules files found in %s", configDir)
	}
	sort.Strings(files)

	ruleset := &RuleSet{Folders: map[string]string{}}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var rf RuleFile
		if err := yaml.Unmarshal(data, &rf); err != nil {
			return nil, fmt.Errorf("parse rules file %s: %w", path, err)
		}

		// Folder-based rules
		for key, folder := range rf.Folders {
			ruleset.Folders[key] = folder.Path
			for _, rule := range folder.Rules {
				rule.Folder = folder.Path
				if err := resolveRefs(&rule, senders); err != nil {
					return nil, fmt.Errorf("rules file %s: %w", path, err)
				}
				ruleset.Rules = append(ruleset.Rules, rule)
			}
		}

		// Flat rules (must include folder)
		for _, rule := range rf.Rules {
			if rule.Folder == "" {
				return nil, fmt.Errorf("rules file %s: rule missing folder", path)
			}
			if err := resolveRefs(&rule, senders); err != nil {
				return nil, fmt.Errorf("rules file %s: %w", path, err)
			}
			ruleset.Rules = append(ruleset.Rules, rule)
		}
	}

	if len(ruleset.Rules) == 0 {
		return nil, fmt.Errorf("no rules loaded")
	}

	return ruleset, nil
}

func resolveRefs(rule *Rule, senders map[string]SenderList) error {
	if len(rule.fromDomainRefs) > 0 {
		for _, ref := range rule.fromDomainRefs {
			list, ok := senders[ref]
			if !ok {
				return fmt.Errorf("unknown sender ref: %s", ref)
			}
			rule.FromDomain = append(rule.FromDomain, list.Domains...)
			rule.From = append(rule.From, list.Addresses...)
		}
	}
	if len(rule.toDomainRefs) > 0 {
		for _, ref := range rule.toDomainRefs {
			list, ok := senders[ref]
			if !ok {
				return fmt.Errorf("unknown sender ref: %s", ref)
			}
			rule.ToDomain = append(rule.ToDomain, list.Domains...)
			rule.To = append(rule.To, list.Addresses...)
		}
	}
	return nil
}

// UnmarshalYAML custom logic to allow aliases like subject_contains_any.
func (r *Rule) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("invalid rule format")
	}
	raw := map[string]*yaml.Node{}
	for i := 0; i < len(value.Content); i += 2 {
		key := value.Content[i]
		val := value.Content[i+1]
		raw[key.Value] = val
	}

	decodeStringList := func(node *yaml.Node) ([]string, error) {
		if node == nil {
			return nil, nil
		}
		switch node.Kind {
		case yaml.ScalarNode:
			var s string
			if err := node.Decode(&s); err != nil {
				return nil, err
			}
			return []string{s}, nil
		case yaml.SequenceNode:
			var s []string
			if err := node.Decode(&s); err != nil {
				return nil, err
			}
			return s, nil
		default:
			return nil, fmt.Errorf("invalid string list")
		}
	}

	if node, ok := raw["name"]; ok {
		_ = node.Decode(&r.Name)
	}
	if node, ok := raw["folder"]; ok {
		_ = node.Decode(&r.Folder)
	}

	if node, ok := raw["from"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.From = list
	}
	if node, ok := raw["to"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.To = list
	}

	// from_domain and to_domain can be list or !ref
	if node, ok := raw["from_domain"]; ok {
		if node.Tag == "!ref" {
			var ref string
			if err := node.Decode(&ref); err != nil {
				return err
			}
			r.fromDomainRefs = append(r.fromDomainRefs, ref)
		} else {
			list, err := decodeStringList(node)
			if err != nil {
				return err
			}
			r.FromDomain = list
		}
	}
	if node, ok := raw["to_domain"]; ok {
		if node.Tag == "!ref" {
			var ref string
			if err := node.Decode(&ref); err != nil {
				return err
			}
			r.toDomainRefs = append(r.toDomainRefs, ref)
		} else {
			list, err := decodeStringList(node)
			if err != nil {
				return err
			}
			r.ToDomain = list
		}
	}

	if node, ok := raw["subject_contains"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.SubjectContains = append(r.SubjectContains, list...)
	}
	if node, ok := raw["subject_contains_any"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.SubjectContains = append(r.SubjectContains, list...)
	}
	if node, ok := raw["body_contains"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.BodyContains = append(r.BodyContains, list...)
	}
	if node, ok := raw["body_contains_any"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.BodyContains = append(r.BodyContains, list...)
	}
	if node, ok := raw["case_insensitive"]; ok {
		_ = node.Decode(&r.CaseInsensitive)
	}
	if node, ok := raw["on_match"]; ok {
		var onMatch OnMatch
		if err := node.Decode(&onMatch); err != nil {
			return err
		}
		r.OnMatch = &onMatch
	}

	return nil
}
