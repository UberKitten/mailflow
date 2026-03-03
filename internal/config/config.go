package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// knownConfigKeys are the valid top-level keys in config.yaml
var knownConfigKeys = map[string]bool{
	"include": true, "graph": true, "pushover": true, "process": true, "webhook": true,
	"folder_categories": true,
}

// knownGraphKeys are the valid keys in the graph section
var knownGraphKeys = map[string]bool{
	"token_script": true, "base_url": true, "max_concurrent_requests": true,
	"range_workers": true, "large_folder_threshold": true, "range_days": true,
	"client_id": true, "tenant_id": true, "token_file": true,
}

// knownWebhookKeys are the valid keys in the webhook section
var knownWebhookKeys = map[string]bool{
	"enabled": true, "port": true, "path": true, "external_url": true,
	"state_file": true, "watch_folder": true, "poll_interval_seconds": true,
	"retry_interval_seconds": true, "startup_resort": true,
	"sweep_folder": true, "sweep_interval_seconds": true,
}

// knownProcessKeys are the valid keys in the process section
var knownProcessKeys = map[string]bool{
	"poll_interval_seconds": true, "resort_workers": true,
}

// knownPushoverKeys are the valid keys in the pushover section
var knownPushoverKeys = map[string]bool{
	"token": true, "user": true,
}

// knownSenderListKeys are the valid keys in sender list files
var knownSenderListKeys = map[string]bool{
	"name": true, "domains": true, "addresses": true,
}

// knownRuleFileKeys are the valid top-level keys in rule files
var knownRuleFileKeys = map[string]bool{
	"version": true, "folders": true, "rules": true,
}

// knownFolderRulesKeys are the valid keys in folder definitions
var knownFolderRulesKeys = map[string]bool{
	"path": true, "rules": true,
}

// knownRuleKeys are the valid keys in rule definitions
var knownRuleKeys = map[string]bool{
	"name": true, "folder": true, "from": true, "to": true,
	"from_name": true, "from_name_contains": true,
	"from_domain": true, "to_domain": true,
	"subject_contains": true, "subject_contains_any": true,
	"body_contains": true, "body_contains_any": true,
	"body_prefix_contains": true, "body_prefix_length": true,
	"subject_not_contains": true, "body_not_contains": true,
	"header_contains": true,
	"case_insensitive": true, "catchall": true, "on_match": true,
	"notify_only": true,
}

// knownOnMatchKeys are the valid keys in on_match blocks
var knownOnMatchKeys = map[string]bool{
	"mark_read": true, "pushover": true, "flag": true, "categories": true, "exec": true,
}

// knownExecKeys are the valid keys in exec action blocks
var knownExecKeys = map[string]bool{
	"command": true, "args": true, "timeout": true,
}

// knownPushoverRuleKeys are the valid keys in pushover rule blocks
var knownPushoverRuleKeys = map[string]bool{
	"title": true, "message": true, "fallback": true, "extract": true,
	"url": true, "url_title": true, "html": true, "priority": true, "sound": true,
}

// knownExtractPatternKeys are the valid keys in extract pattern blocks
var knownExtractPatternKeys = map[string]bool{
	"pattern": true, "capture": true,
}

// warnUnknownKeys checks a YAML mapping node for keys not in the known set
// and logs warnings for any unknown keys found.
func warnUnknownKeys(node *yaml.Node, known map[string]bool, context string) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !known[key] {
			log.Printf("WARNING: unknown config key %q in %s (line %d)", key, context, node.Content[i].Line)
		}
	}
}

// checkUnknownKeysInData unmarshals YAML data into a node and checks for unknown keys
func checkUnknownKeysInData(data []byte, known map[string]bool, context string) error {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		warnUnknownKeys(node.Content[0], known, context)
	}
	return nil
}

// checkNestedUnknownKeys checks for unknown keys in nested sections of config.yaml
func checkNestedUnknownKeys(data []byte, configPath string) error {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return nil
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i].Value
		val := root.Content[i+1]

		switch key {
		case "graph":
			warnUnknownKeys(val, knownGraphKeys, configPath+" -> graph")
		case "webhook":
			warnUnknownKeys(val, knownWebhookKeys, configPath+" -> webhook")
		case "process":
			warnUnknownKeys(val, knownProcessKeys, configPath+" -> process")
		case "pushover":
			warnUnknownKeys(val, knownPushoverKeys, configPath+" -> pushover")
		}
	}
	return nil
}

// checkRuleFileNestedKeys checks folder definitions in rule files for unknown keys
func checkRuleFileNestedKeys(data []byte, filePath string) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return
	}
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return
	}

	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i].Value
		val := root.Content[i+1]

		if key == "folders" && val.Kind == yaml.MappingNode {
			// Check each folder definition
			for j := 0; j < len(val.Content); j += 2 {
				folderName := val.Content[j].Value
				folderDef := val.Content[j+1]
				warnUnknownKeys(folderDef, knownFolderRulesKeys,
					fmt.Sprintf("%s -> folders -> %s", filePath, folderName))
			}
		}
	}
}

// FolderCategory maps a folder prefix to categories that should be auto-applied.
// Any email moved to a folder matching the prefix (or its subfolders) gets these categories
// merged with any rule-level on_match categories.
type FolderCategory struct {
	Folder     string   `yaml:"folder"`
	Categories []string `yaml:"categories"`
}

// Config is the main config file (config.yaml)
type Config struct {
	Include          []string         `yaml:"include"`
	Graph            GraphConfig      `yaml:"graph"`
	Pushover         Pushover         `yaml:"pushover"`
	Process          ProcessConfig    `yaml:"process"`
	Webhook          WebhookConfig    `yaml:"webhook"`
	FolderCategories []FolderCategory `yaml:"folder_categories"`
}

type WebhookConfig struct {
	Enabled              bool   `yaml:"enabled"`
	Port                 int    `yaml:"port"`
	Path                 string `yaml:"path"`
	ExternalURL          string `yaml:"external_url"`
	StateFile            string `yaml:"state_file"`
	WatchFolder          string `yaml:"watch_folder"` // folder path to watch, default "Inbox"
	PollIntervalSeconds  int    `yaml:"poll_interval_seconds"`
	RetryIntervalSeconds int    `yaml:"retry_interval_seconds"`
	StartupResort        *bool  `yaml:"startup_resort"` // resort watch folder on startup, default true
	SweepFolder          string `yaml:"sweep_folder"`   // folder to periodically sweep, default "Unsorted"
	SweepIntervalSeconds int    `yaml:"sweep_interval_seconds"` // sweep interval, default 60
}

type GraphConfig struct {
	// Built-in OAuth2 token management (recommended)
	ClientID  string `yaml:"client_id"`
	TenantID  string `yaml:"tenant_id"`
	TokenFile string `yaml:"token_file"` // path to cached token JSON

	// External token script (legacy/custom auth)
	TokenScript string `yaml:"token_script"`

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

// FolderCategoriesFor returns categories that should be auto-applied for a given
// destination folder. A folder_categories entry with folder "Inbox/Posts" matches
// "Inbox/Posts" and all subfolders like "Inbox/Posts/Tech".
func (c *Config) FolderCategoriesFor(folder string) []string {
	var cats []string
	for _, fc := range c.FolderCategories {
		if folder == fc.Folder || strings.HasPrefix(folder, fc.Folder+"/") {
			cats = append(cats, fc.Categories...)
		}
	}
	return cats
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
	Name               string
	Source             string
	Folder             string
	From               []string
	FromName           []string
	FromNameContains   []string
	To                 []string
	FromDomain         []string
	ToDomain           []string
	SubjectContains    []string
	SubjectNotContains []string
	BodyContains       []string
	BodyNotContains    []string
	BodyPrefixContains []string
	BodyPrefixLength   int
	HeaderContains     map[string][]string // header name → list of match values (any match = pass)
	CaseInsensitive    bool
	Catchall           bool
	NotifyOnly         bool
	OnMatch            *OnMatch

	fromDomainRefs []string
	toDomainRefs   []string
}

type OnMatch struct {
	MarkRead   bool          `yaml:"mark_read"`
	Pushover   *PushoverRule `yaml:"pushover"`
	Flag       string        `yaml:"-"` // "flagged", "complete", "notFlagged" (parsed from bool or string)
	Categories []string      `yaml:"-"` // Outlook categories (colored labels)
	Exec       *ExecAction   `yaml:"exec"`
}

// ExecAction defines a shell command to execute on match
type ExecAction struct {
	Command string   `yaml:"command"` // shell command to run
	Args    []string `yaml:"args"`    // optional args
	Timeout int      `yaml:"timeout"` // seconds, default 30
}

// onMatchRaw is used for initial YAML unmarshaling
type onMatchRaw struct {
	MarkRead   bool          `yaml:"mark_read"`
	Pushover   *PushoverRule `yaml:"pushover"`
	Flag       interface{}   `yaml:"flag"`
	Categories interface{}   `yaml:"categories"`
	Exec       *ExecAction   `yaml:"exec"`
}

// UnmarshalYAML handles flag being either bool or string, and categories being string or array
func (o *OnMatch) UnmarshalYAML(value *yaml.Node) error {
	var raw onMatchRaw
	if err := value.Decode(&raw); err != nil {
		return err
	}

	o.MarkRead = raw.MarkRead
	o.Pushover = raw.Pushover
	o.Exec = raw.Exec

	// Handle flag (bool or string)
	switch v := raw.Flag.(type) {
	case bool:
		if v {
			o.Flag = "flagged"
		}
	case string:
		o.Flag = v
	}

	// Handle categories (string or []string)
	switch v := raw.Categories.(type) {
	case string:
		o.Categories = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				o.Categories = append(o.Categories, s)
			}
		}
	}

	return nil
}

type PushoverRule struct {
	Title    string           `yaml:"title"`
	Message  string           `yaml:"message"`
	Fallback string           `yaml:"fallback"`
	Extract  []ExtractPattern `yaml:"extract"`
	URL      string           `yaml:"url"`
	URLTitle string           `yaml:"url_title"`
	HTML     int              `yaml:"html"`
	Priority int              `yaml:"priority"`
	Sound    string           `yaml:"sound"`
}

type ExtractPattern struct {
	Pattern string `yaml:"pattern"`
	Capture string `yaml:"capture"`
}

// OnMatchCategories returns the categories from on_match, or nil if none.
func (r *Rule) OnMatchCategories() []string {
	if r.OnMatch == nil {
		return nil
	}
	return r.OnMatch.Categories
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
	cfg, err := LoadMainConfig(configDir)
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

// LoadMainConfig reads and parses config.yaml (without loading rules/senders).
func LoadMainConfig(configDir string) (*Config, error) {
	path := filepath.Join(configDir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config.yaml: %w", err)
	}

	// Check for unknown keys at top level and nested sections
	_ = checkUnknownKeysInData(data, knownConfigKeys, path)
	_ = checkNestedUnknownKeys(data, path)

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}

	if cfg.Graph.ClientID != "" && cfg.Graph.TenantID != "" {
		// Built-in OAuth2 mode — token_script not needed
		if cfg.Graph.TokenFile == "" {
			cfg.Graph.TokenFile = filepath.Join(filepath.Dir(path), ".ms-graph-token.json")
		}
	} else if cfg.Graph.TokenScript == "" {
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
	if cfg.Webhook.StateFile == "" {
		cfg.Webhook.StateFile = "/data/webhook-state.json"
	}
	if cfg.Webhook.PollIntervalSeconds == 0 {
		cfg.Webhook.PollIntervalSeconds = 60
	}
	if cfg.Webhook.RetryIntervalSeconds == 0 {
		cfg.Webhook.RetryIntervalSeconds = 300
	}
	if cfg.Webhook.SweepFolder == "" {
		cfg.Webhook.SweepFolder = "Unsorted"
	}
	if cfg.Webhook.SweepIntervalSeconds == 0 {
		cfg.Webhook.SweepIntervalSeconds = 60
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

		// Check for unknown keys in sender list
		_ = checkUnknownKeysInData(data, knownSenderListKeys, entry)

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

		// Check for unknown keys in rule file
		_ = checkUnknownKeysInData(data, knownRuleFileKeys, path)
		checkRuleFileNestedKeys(data, path)

		var rf RuleFile
		if err := yaml.Unmarshal(data, &rf); err != nil {
			return nil, fmt.Errorf("parse rules file %s: %w", path, err)
		}

		// Folder-based rules
		for key, folder := range rf.Folders {
			ruleset.Folders[key] = folder.Path
			for _, rule := range folder.Rules {
				rule.Folder = folder.Path
				rule.Source = filepath.Base(path)
				if err := resolveRefs(&rule, senders); err != nil {
					return nil, fmt.Errorf("rules file %s: %w", path, err)
				}
				ruleset.Rules = append(ruleset.Rules, rule)
			}
		}

		// Flat rules (must include folder, unless notify_only)
		for _, rule := range rf.Rules {
			if rule.Folder == "" && !rule.NotifyOnly {
				return nil, fmt.Errorf("rules file %s: rule missing folder", path)
			}
			rule.Source = filepath.Base(path)
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
	if node, ok := raw["from_name"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.FromName = list
	}
	if node, ok := raw["from_name_contains"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.FromNameContains = list
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
	if node, ok := raw["subject_not_contains"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.SubjectNotContains = append(r.SubjectNotContains, list...)
	}
	if node, ok := raw["body_not_contains"]; ok {
		list, err := decodeStringList(node)
		if err != nil {
			return err
		}
		r.BodyNotContains = append(r.BodyNotContains, list...)
	}
	if node, ok := raw["header_contains"]; ok {
		// header_contains is a map of header name to string-or-list-of-strings
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("header_contains must be a mapping")
		}
		r.HeaderContains = make(map[string][]string)
		for i := 0; i < len(node.Content); i += 2 {
			headerName := node.Content[i].Value
			valNode := node.Content[i+1]
			list, err := decodeStringList(valNode)
			if err != nil {
				return fmt.Errorf("header_contains[%s]: %w", headerName, err)
			}
			r.HeaderContains[headerName] = list
		}
	}
	if node, ok := raw["case_insensitive"]; ok {
		_ = node.Decode(&r.CaseInsensitive)
	}
	if node, ok := raw["catchall"]; ok {
		_ = node.Decode(&r.Catchall)
	}
	if node, ok := raw["notify_only"]; ok {
		_ = node.Decode(&r.NotifyOnly)
	}
	if node, ok := raw["on_match"]; ok {
		var onMatch OnMatch
		if err := node.Decode(&onMatch); err != nil {
			return err
		}
		r.OnMatch = &onMatch

		// Check for unknown keys in on_match
		checkOnMatchKeys(node)
	}

	// Warn about unknown keys in rule
	for key := range raw {
		if !knownRuleKeys[key] {
			ruleName := r.Name
			if ruleName == "" {
				ruleName = "(unnamed)"
			}
			log.Printf("WARNING: unknown rule key %q in rule %s", key, ruleName)
		}
	}

	return nil
}

// checkOnMatchKeys checks for unknown keys in on_match blocks
func checkOnMatchKeys(node *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !knownOnMatchKeys[key] {
			log.Printf("WARNING: unknown on_match key %q (line %d)", key, node.Content[i].Line)
		}
		// Check pushover sub-block
		if key == "pushover" {
			checkPushoverRuleKeys(node.Content[i+1])
		}
		// Check exec sub-block
		if key == "exec" {
			checkExecActionKeys(node.Content[i+1])
		}
	}
}

// checkExecActionKeys checks for unknown keys in exec action blocks
func checkExecActionKeys(node *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !knownExecKeys[key] {
			log.Printf("WARNING: unknown exec key %q (line %d)", key, node.Content[i].Line)
		}
	}
}

// checkPushoverRuleKeys checks for unknown keys in pushover rule blocks
func checkPushoverRuleKeys(node *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !knownPushoverRuleKeys[key] {
			log.Printf("WARNING: unknown pushover key %q (line %d)", key, node.Content[i].Line)
		}
		// Check extract array
		if key == "extract" {
			checkExtractPatterns(node.Content[i+1])
		}
	}
}

// checkExtractPatterns checks for unknown keys in extract pattern blocks
func checkExtractPatterns(node *yaml.Node) {
	if node.Kind != yaml.SequenceNode {
		return
	}
	for _, item := range node.Content {
		if item.Kind == yaml.MappingNode {
			for i := 0; i < len(item.Content); i += 2 {
				key := item.Content[i].Value
				if !knownExtractPatternKeys[key] {
					log.Printf("WARNING: unknown extract key %q (line %d)", key, item.Content[i].Line)
				}
			}
		}
	}
}
