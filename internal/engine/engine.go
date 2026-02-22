package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mailflow/internal/config"

	"golang.org/x/sync/errgroup"
	"mailflow/internal/graph"
	"mailflow/internal/pushover"
)

var (
	regexCache   = make(map[string]*regexp.Regexp)
	regexCacheMu sync.RWMutex
)

type Engine struct {
	cfg           *config.Config
	rules         *config.RuleSet
	client        *graph.Client
	watchFolder   string
	watchFolderID string
}

func New(cfg *config.Config, rules *config.RuleSet, client *graph.Client) *Engine {
	return &Engine{cfg: cfg, rules: rules, client: client}
}

// SetWatchFolder sets the folder to watch for webhook processing.
func (e *Engine) SetWatchFolder(folder, folderID string) {
	e.watchFolder = folder
	e.watchFolderID = folderID
}

func (e *Engine) Reload(cfg *config.Config, rules *config.RuleSet) error {
	e.cfg = cfg
	e.rules = rules
	return nil
}

// Rules returns the current ruleset
func (e *Engine) Rules() *config.RuleSet {
	return e.rules
}

func (e *Engine) ProcessOnce(ctx context.Context, since time.Duration) error {
	inboxID, err := e.client.FindFolderIDByPath(ctx, "Inbox")
	if err != nil {
		return err
	}

	msgs, err := e.client.ListMessages(ctx, inboxID, graph.ListOptions{
		OnlyUnread: true,
		Since:      since,
	})
	if err != nil {
		return err
	}

	for _, msg := range msgs {
		// Collect notify_only rules before moving (match against original message)
		notifyRules := MatchNotifyOnly(e.rules, msg)

		// Find the sorting rule
		rule := Match(e.rules, msg, MatchOptions{})
		if rule == nil {
			// No sorting rule — fire notify_only with original ID (message stays put)
			for _, notifyRule := range notifyRules {
				slog.Info("notify_only matched", "id", msg.ID, "from", msg.From, "subject", msg.Subject, "rule", notifyRule.Name)
				e.executeOnMatch(ctx, msg.ID, msg, notifyRule, OnMatchOptions{AllowPushover: true})
			}
			continue
		}

		destID, err := e.client.FindFolderIDByPath(ctx, rule.Folder)
		if err != nil {
			return err
		}
		newMsgID, err := e.client.MoveMessage(ctx, msg.ID, destID)
		if err != nil {
			if errors.Is(err, graph.ErrMessageGone) {
				slog.Warn("message gone before move", "id", msg.ID, "subject", msg.Subject)
				return nil
			}
			return err
		}
		slog.Info("moved", "id", newMsgID, "from", msg.From, "subject", msg.Subject, "rule", rule.Name, "folder", rule.Folder)

		msg.ID = newMsgID

		// Fire notify_only rules AFTER move so ${message_id} reflects the new ID
		for _, notifyRule := range notifyRules {
			slog.Info("notify_only matched", "id", newMsgID, "from", msg.From, "subject", msg.Subject, "rule", notifyRule.Name)
			e.executeOnMatch(ctx, newMsgID, msg, notifyRule, OnMatchOptions{AllowPushover: true})
		}

		e.executeOnMatch(ctx, newMsgID, msg, rule, OnMatchOptions{AllowPushover: true})
	}

	return nil
}

// ProcessSingle processes a single message by ID (used by webhook)
func (e *Engine) ProcessSingle(ctx context.Context, messageID string) error {
	// Check if message is in the watch folder
	folderID, err := e.client.GetMessageFolder(ctx, messageID)
	if err != nil {
		return fmt.Errorf("get message folder: %w", err)
	}

	// Determine expected folder - use watch folder if set, otherwise Inbox
	expectedFolderID := e.watchFolderID
	expectedFolderName := e.watchFolder
	if expectedFolderID == "" {
		expectedFolderID, err = e.client.FindFolderIDByPath(ctx, "Inbox")
		if err != nil {
			return fmt.Errorf("find inbox: %w", err)
		}
		expectedFolderName = "Inbox"
	}

	// Only process if in expected folder
	if folderID != expectedFolderID {
		slog.Debug("message not in watch folder, skipping", "messageId", messageID, "folder", folderID, "expected", expectedFolderName)
		return nil
	}

	// Get full message
	msg, err := e.client.GetMessage(ctx, messageID)
	if err != nil {
		return fmt.Errorf("get message: %w", err)
	}

	// Collect notify_only rules before moving (match against original message)
	notifyRules := MatchNotifyOnly(e.rules, *msg)

	// Match against sorting rules
	rule := Match(e.rules, *msg, MatchOptions{})
	if rule == nil {
		// No sorting rule — fire notify_only with original ID (message stays put)
		for _, notifyRule := range notifyRules {
			slog.Info("notify_only matched", "id", messageID, "from", msg.From, "subject", msg.Subject, "rule", notifyRule.Name)
			e.executeOnMatch(ctx, messageID, *msg, notifyRule, OnMatchOptions{AllowPushover: true})
		}
		slog.Debug("no rule matched", "messageId", messageID, "from", msg.From, "subject", msg.Subject)
		return nil
	}

	// Move to destination folder
	destID, err := e.client.FindFolderIDByPath(ctx, rule.Folder)
	if err != nil {
		return fmt.Errorf("find dest folder: %w", err)
	}

	newMsgID, err := e.client.MoveMessage(ctx, messageID, destID)
	if err != nil {
		if errors.Is(err, graph.ErrMessageGone) {
			slog.Warn("message gone before move", "id", messageID, "subject", msg.Subject)
			return nil
		}
		return fmt.Errorf("move message: %w", err)
	}

	slog.Info("moved", "id", newMsgID, "from", msg.From, "subject", msg.Subject, "rule", rule.Name, "folder", rule.Folder)

	// Execute on_match actions with new message ID (ID changes on move in Graph API)
	msg.ID = newMsgID

	// Fire notify_only rules AFTER move so ${message_id} reflects the new ID
	for _, notifyRule := range notifyRules {
		slog.Info("notify_only matched", "id", newMsgID, "from", msg.From, "subject", msg.Subject, "rule", notifyRule.Name)
		e.executeOnMatch(ctx, newMsgID, *msg, notifyRule, OnMatchOptions{AllowPushover: true})
	}

	e.executeOnMatch(ctx, newMsgID, *msg, rule, OnMatchOptions{AllowPushover: true})

	return nil
}

func (e *Engine) sendPushover(msg graph.Message, rule *config.Rule) {
	if e.cfg.Pushover.Token == "" || e.cfg.Pushover.User == "" {
		slog.Warn("pushover config missing")
		return
	}

	payload := BuildPushover(rule.OnMatch.Pushover, msg)
	payload.Token = e.cfg.Pushover.Token
	payload.User = e.cfg.Pushover.User

	slog.Info("pushover sending", "title", payload.Title, "message", payload.Message, "url", payload.URL)

	if err := pushover.Send(payload); err != nil {
		slog.Warn("pushover send failed", "error", err)
	}
}

// OnMatchOptions controls behavior for on_match actions.
type OnMatchOptions struct {
	AllowPushover bool
}

// ApplyOnMatch runs on_match actions for a rule with options.
func (e *Engine) ApplyOnMatch(ctx context.Context, msgID string, msg graph.Message, rule *config.Rule, opts OnMatchOptions) {
	e.executeOnMatch(ctx, msgID, msg, rule, opts)
}

// executeOnMatch runs all on_match actions for a rule
func (e *Engine) executeOnMatch(ctx context.Context, msgID string, msg graph.Message, rule *config.Rule, opts OnMatchOptions) {
	if rule.OnMatch == nil {
		return
	}

	if rule.OnMatch.MarkRead {
		if err := e.client.MarkRead(ctx, msgID); err != nil {
			slog.Warn("mark read failed", "id", msgID, "error", err)
		}
	}

	if rule.OnMatch.Flag != "" {
		if err := e.client.FlagMessage(ctx, msgID, rule.OnMatch.Flag); err != nil {
			slog.Warn("flag message failed", "id", msgID, "flag", rule.OnMatch.Flag, "error", err)
		} else {
			slog.Debug("flagged message", "id", msgID, "flag", rule.OnMatch.Flag)
		}
	}

	if len(rule.OnMatch.Categories) > 0 {
		if err := e.client.SetCategories(ctx, msgID, rule.OnMatch.Categories); err != nil {
			slog.Warn("set categories failed", "id", msgID, "categories", rule.OnMatch.Categories, "error", err)
		} else {
			slog.Debug("set categories", "id", msgID, "categories", rule.OnMatch.Categories)
		}
	}

	if opts.AllowPushover && rule.OnMatch.Pushover != nil {
		e.sendPushover(msg, rule)
	}

	if rule.OnMatch.Exec != nil {
		e.executeExec(ctx, msg, rule.OnMatch.Exec)
	}
}

// execPayload is the JSON payload passed to exec commands via stdin
type execPayload struct {
	ID             string   `json:"id"`
	From           string   `json:"from"`
	FromName       string   `json:"from_name"`
	To             []string `json:"to"`
	Subject        string   `json:"subject"`
	Body           string   `json:"body"`
	BodyHTML       string   `json:"body_html"`
	Received       string   `json:"received"`
	HasAttachments bool     `json:"has_attachments"`
	MessageID      string   `json:"message_id"`
}

// executeExec runs a shell command with email metadata as JSON on stdin
func (e *Engine) executeExec(ctx context.Context, msg graph.Message, execAction *config.ExecAction) {
	timeout := execAction.Timeout
	if timeout <= 0 {
		timeout = 30
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Build the command
	args := execAction.Args
	cmd := exec.CommandContext(execCtx, execAction.Command, args...)

	// Build JSON payload
	payload := execPayload{
		ID:             msg.ID,
		From:           msg.From,
		FromName:       msg.FromName,
		To:             msg.To,
		Subject:        msg.Subject,
		Body:           msg.Body,
		BodyHTML:       msg.BodyHTML,
		Received:       msg.Received.Format(time.RFC3339),
		HasAttachments: false, // TODO: add attachment detection if needed
		MessageID:      msg.ID,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("exec marshal payload failed", "command", execAction.Command, "error", err)
		return
	}

	cmd.Stdin = bytes.NewReader(jsonData)

	// Capture stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Info("exec running", "command", execAction.Command, "args", args, "message_id", msg.ID)

	err = cmd.Run()
	if err != nil {
		slog.Warn("exec failed",
			"command", execAction.Command,
			"error", err,
			"stdout", stdout.String(),
			"stderr", stderr.String(),
		)
		return
	}

	slog.Info("exec completed",
		"command", execAction.Command,
		"message_id", msg.ID,
		"stdout", strings.TrimSpace(stdout.String()),
	)
}

// MatchOptions controls rule matching behavior.
type MatchOptions struct {
	Fast           bool
	IgnoreCatchall bool
}

// Match returns the first rule that matches.
func Match(rules *config.RuleSet, msg graph.Message, opts MatchOptions) *config.Rule {
	for i := range rules.Rules {
		rule := &rules.Rules[i]
		if opts.IgnoreCatchall && rule.Catchall {
			continue
		}
		// Skip notify_only rules - they don't move messages
		if rule.NotifyOnly {
			continue
		}
		if ruleMatches(*rule, msg, opts) {
			return rule
		}
	}
	return nil
}

// MatchNotifyOnly returns all matching notify_only rules for a message.
// These rules trigger on_match actions but don't prevent other rules from matching.
func MatchNotifyOnly(rules *config.RuleSet, msg graph.Message) []*config.Rule {
	var matches []*config.Rule
	for i := range rules.Rules {
		rule := &rules.Rules[i]
		if !rule.NotifyOnly {
			continue
		}
		if ruleMatches(*rule, msg, MatchOptions{}) {
			matches = append(matches, rule)
		}
	}
	return matches
}

func ruleMatches(rule config.Rule, msg graph.Message, opts MatchOptions) bool {
	subject := msg.Subject
	body := msg.Body
	from := msg.From
	fromName := msg.FromName
	fromDomain := domainFromEmail(from)
	toList := msg.To

	if rule.CaseInsensitive {
		subject = strings.ToLower(subject)
		body = strings.ToLower(body)
		from = strings.ToLower(from)
		fromName = strings.ToLower(fromName)
		fromDomain = strings.ToLower(fromDomain)
		for i, t := range toList {
			toList[i] = strings.ToLower(t)
		}
	}

	if len(rule.From) > 0 {
		matched := false
		for _, pattern := range rule.From {
			if matchPattern(pattern, from, rule.CaseInsensitive) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(rule.FromName) > 0 {
		matched := false
		for _, pattern := range rule.FromName {
			if matchPattern(pattern, fromName, rule.CaseInsensitive) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(rule.FromNameContains) > 0 {
		matched := false
		checkName := fromName
		if rule.CaseInsensitive {
			checkName = strings.ToLower(fromName)
		}
		for _, needle := range rule.FromNameContains {
			checkNeedle := needle
			if rule.CaseInsensitive {
				checkNeedle = strings.ToLower(needle)
			}
			if strings.Contains(checkName, checkNeedle) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(rule.FromDomain) > 0 {
		matched := false
		for _, pattern := range rule.FromDomain {
			// Domains are always case-insensitive per RFC 1035
			if matchPattern(pattern, fromDomain, true) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(rule.To) > 0 {
		matched := false
		for _, t := range toList {
			for _, pattern := range rule.To {
				if matchPattern(pattern, t, rule.CaseInsensitive) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}

	if len(rule.ToDomain) > 0 {
		matched := false
		for _, t := range toList {
			domain := domainFromEmail(t)
			for _, pattern := range rule.ToDomain {
				// Domains are always case-insensitive per RFC 1035
				if matchPattern(pattern, domain, true) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}

	// header_contains: skip in fast mode (headers not fetched)
	if opts.Fast && len(rule.HeaderContains) > 0 {
		return false
	}

	// header_contains: for each header in rule, check if message header contains any pattern
	if len(rule.HeaderContains) > 0 {
		for headerName, patterns := range rule.HeaderContains {
			// Look up header value case-insensitively (per RFC 2822)
			headerValue := getHeaderCaseInsensitive(msg.Headers, headerName)
			if headerValue == "" {
				// Header not present in message
				return false
			}
			matched := false
			for _, pattern := range patterns {
				cmpPattern := pattern
				cmpValue := headerValue
				if rule.CaseInsensitive {
					cmpPattern = strings.ToLower(cmpPattern)
					cmpValue = strings.ToLower(cmpValue)
				}
				if strings.Contains(cmpValue, cmpPattern) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}

	if opts.Fast && len(rule.BodyContains) > 0 {
		return false
	}

	if opts.Fast && len(rule.BodyPrefixContains) > 0 {
		return false
	}

	if len(rule.SubjectContains) > 0 {
		matched := false
		for _, s := range rule.SubjectContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(subject, cmp) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// subject_not_contains: if ANY pattern matches, rule fails (exclusion logic)
	if len(rule.SubjectNotContains) > 0 {
		for _, s := range rule.SubjectNotContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(subject, cmp) {
				return false
			}
		}
	}

	if len(rule.BodyContains) > 0 {
		matched := false
		for _, s := range rule.BodyContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(body, cmp) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// body_not_contains: if ANY pattern matches, rule fails (exclusion logic)
	if len(rule.BodyNotContains) > 0 {
		for _, s := range rule.BodyNotContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(body, cmp) {
				return false
			}
		}
	}

	// body_prefix_contains: check only the first N characters of the body
	if len(rule.BodyPrefixContains) > 0 {
		prefixLen := rule.BodyPrefixLength
		if prefixLen <= 0 {
			prefixLen = 1000 // default to 1000 chars
		}
		prefix := body
		if len(prefix) > prefixLen {
			prefix = prefix[:prefixLen]
		}
		matched := false
		for _, s := range rule.BodyPrefixContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(prefix, cmp) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

func getRegex(pattern string) *regexp.Regexp {
	regexCacheMu.RLock()
	re, ok := regexCache[pattern]
	regexCacheMu.RUnlock()
	if ok {
		return re
	}

	regexCacheMu.Lock()
	defer regexCacheMu.Unlock()
	if re, ok = regexCache[pattern]; ok {
		return re
	}
	re = regexp.MustCompile(pattern)
	regexCache[pattern] = re
	return re
}

func matchPattern(pattern, value string, caseInsensitive bool) bool {
	if caseInsensitive {
		pattern = strings.ToLower(pattern)
		value = strings.ToLower(value)
	}

	// Convert glob to regex
	re := getRegex("^" + globToRegex(pattern) + "$")
	return re.MatchString(value)
}

func globToRegex(glob string) string {
	escaped := regexp.QuoteMeta(glob)
	escaped = strings.ReplaceAll(escaped, "\\*", ".*")
	escaped = strings.ReplaceAll(escaped, "\\?", ".")
	return escaped
}

func domainFromEmail(addr string) string {
	parts := strings.Split(addr, "@")
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// getHeaderCaseInsensitive looks up a header value case-insensitively.
// Per RFC 2822, header field names are case-insensitive.
func getHeaderCaseInsensitive(headers map[string]string, name string) string {
	// Try exact match first
	if val, ok := headers[name]; ok {
		return val
	}
	// Fall back to case-insensitive search
	nameLower := strings.ToLower(name)
	for k, v := range headers {
		if strings.ToLower(k) == nameLower {
			return v
		}
	}
	return ""
}

// MatchSender checks if an email address matches a pattern (supports * wildcards).
// Pattern matching is case-insensitive.
func MatchSender(pattern, email string) bool {
	return matchPattern(pattern, email, true)
}

// BuildPushover extracts data and builds payload.
func BuildPushover(cfg *config.PushoverRule, msg graph.Message) pushover.Payload {
	vars := map[string]string{
		"subject":    msg.Subject,
		"from":       msg.From,
		"from_name":  msg.FromName,
		"to":         strings.Join(msg.To, ","),
		"snippet":    msg.Snippet,
		"body":       msg.Body,
		"body_html":  msg.BodyHTML,
		"message_id": msg.ID,
	}

	// Try all extract patterns, but only set each capture name once (first match wins)
	for _, extract := range cfg.Extract {
		re, err := regexp.Compile(extract.Pattern)
		if err != nil {
			continue
		}
		matches := re.FindStringSubmatch(msg.Body)
		if len(matches) == 0 {
			matches = re.FindStringSubmatch(msg.BodyHTML)
		}
		if len(matches) == 0 {
			continue
		}

		if extract.Capture != "" {
			// Only set if not already captured (first match wins)
			if _, exists := vars[extract.Capture]; !exists && len(matches) > 1 {
				vars[extract.Capture] = matches[1]
			}
		} else {
			for i, name := range re.SubexpNames() {
				if i == 0 || name == "" {
					continue
				}
				// Only set if not already captured (first match wins)
				if _, exists := vars[name]; !exists {
					vars[name] = matches[i]
				}
			}
		}
	}

	message := cfg.Message
	if message == "" {
		message = cfg.Fallback
	}

	if message == "" {
		message = "${subject}"
	}

	// Expand variables in the message. If any ${...} remain unexpanded,
	// fall back to the fallback template (or subject).
	expanded := expandVars(message, vars)
	if strings.Contains(expanded, "${") && cfg.Fallback != "" && message != cfg.Fallback {
		expanded = expandVars(cfg.Fallback, vars)
	}

	return pushover.Payload{
		Title:    expandVars(cfg.Title, vars),
		Message:  expanded,
		URL:      expandVars(cfg.URL, vars),
		URLTitle: expandVars(cfg.URLTitle, vars),
		HTML:     cfg.HTML,
		Priority: cfg.Priority,
		Sound:    cfg.Sound,
	}
}

func expandVars(template string, vars map[string]string) string {
	out := template
	for k, v := range vars {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	return out
}

// ResortOptions controls resort behavior.
type ResortOptions struct {
	DryRun         bool
	Recursive      bool
	Since          time.Duration
	Before         time.Time
	Fast           bool
	CheckpointPath string
	ConfigDir      string
	Resume         bool
}

// ResortCheckpoint stores resume state for resort.
type ResortCheckpoint struct {
	Folder      string               `json:"folder"`
	Recursive   bool                 `json:"recursive"`
	Processed   int                  `json:"processed"`
	StartedAt   time.Time            `json:"startedAt"`
	FolderTimes map[string]time.Time `json:"folderTimes"`
}

// DefaultResortCheckpointPath returns the default checkpoint path.
func DefaultResortCheckpointPath(configDir string) string {
	if configDir != "" {
		return filepath.Join(configDir, "resort-checkpoint.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "resort-checkpoint.json"
	}
	return filepath.Join(home, ".config", "appdata", "mailflow", "resort-checkpoint.json")
}

func expandCheckpointPath(path string) string {
	if path == "" {
		return path
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func LoadResortCheckpoint(path string) (*ResortCheckpoint, error) {
	path = expandCheckpointPath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var checkpoint ResortCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func writeResortCheckpoint(path string, checkpoint ResortCheckpoint) error {
	path = expandCheckpointPath(path)
	if path == "" {
		return nil
	}
	if len(checkpoint.FolderTimes) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func deleteResortCheckpoint(path string) error {
	path = expandCheckpointPath(path)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type MoveRecord struct {
	FromFolder string
	ToFolder   string
	From       string
	Subject    string
}

type ResortReport struct {
	StartedAt time.Time
	Total     int
	Moved     int
	Unmatched int
	Moves     []MoveRecord
}

func (e *Engine) Resort(ctx context.Context, folder string, opts ResortOptions) (*ResortReport, error) {
	start := time.Now()
	checkpointPath := opts.CheckpointPath
	if checkpointPath == "" {
		checkpointPath = DefaultResortCheckpointPath(opts.ConfigDir)
	}
	checkpointPath = expandCheckpointPath(checkpointPath)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	folderID, err := e.client.FindFolderIDByPath(ctx, folder)
	if err != nil {
		return nil, err
	}

	var folderIDs []graph.FolderInfo
	if opts.Recursive {
		folderIDs, err = e.client.ListFolderTree(ctx, folderID, folder)
		if err != nil {
			return nil, err
		}
	} else {
		folderIDs = []graph.FolderInfo{{ID: folderID, Path: folder}}
	}

	// Build folder ID cache from rules
	folderCache := make(map[string]string)
	for _, rule := range e.rules.Rules {
		if rule.Folder == "" {
			continue
		}
		if _, ok := folderCache[rule.Folder]; ok {
			continue
		}
		slog.Info("resolving folder", "folder", rule.Folder)
		id, err := e.client.FindFolderIDByPath(ctx, rule.Folder)
		if err != nil {
			return nil, fmt.Errorf("resolve folder %s: %w", rule.Folder, err)
		}
		folderCache[rule.Folder] = id
	}
	slog.Info("folder cache built", "count", len(folderCache))

	report := &ResortReport{StartedAt: start}
	var reportMu sync.Mutex
	folderTimes := make(map[string]time.Time)

	var resumeTimes map[string]time.Time
	if opts.Resume {
		checkpoint, err := LoadResortCheckpoint(checkpointPath)
		if err != nil {
			return nil, fmt.Errorf("load checkpoint: %w", err)
		}
		resumeTimes = checkpoint.FolderTimes
	}

	writeCheckpoint := func() {
		reportMu.Lock()
		checkpoint := ResortCheckpoint{
			Folder:      folder,
			Recursive:   opts.Recursive,
			Processed:   report.Total,
			StartedAt:   start,
			FolderTimes: make(map[string]time.Time, len(folderTimes)),
		}
		for key, value := range folderTimes {
			checkpoint.FolderTimes[key] = value
		}
		reportMu.Unlock()
		if err := writeResortCheckpoint(checkpointPath, checkpoint); err != nil {
			slog.Warn("write checkpoint failed", "error", err)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			slog.Warn("resort interrupted, writing checkpoint")
			writeCheckpoint()
			cancel()
		case <-ctx.Done():
		}
	}()

	workerLimit := e.cfg.Process.ResortWorkers
	if workerLimit <= 0 {
		workerLimit = 6
	}

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, workerLimit)
	for _, f := range folderIDs {
		f := f
		before := opts.Before
		if opts.Resume {
			resumeTime, ok := resumeTimes[f.Path]
			if !ok {
				slog.Info("skipping completed folder", "folder", f.Path)
				continue
			}
			before = resumeTime
		}

		g.Go(func() error {
			reportMu.Lock()
			if _, ok := folderTimes[f.Path]; !ok {
				folderTimes[f.Path] = time.Time{}
			}
			reportMu.Unlock()

			sem <- struct{}{}
			defer func() { <-sem }()

			slog.Info("starting stream", "folder", f.Path, "fast", opts.Fast)
			slog.Info("fetching messages from Graph API...")

			// Heartbeat goroutine to show we're still alive during slow API calls
			heartbeatCtx, heartbeatCancel := context.WithCancel(gctx)
			defer heartbeatCancel()
			var lastProgress atomic.Int64
			lastProgress.Store(time.Now().UnixNano())
			go func() {
				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-heartbeatCtx.Done():
						return
					case <-ticker.C:
						lastTime := time.Unix(0, lastProgress.Load())
						if time.Since(lastTime) > 10*time.Second {
							reportMu.Lock()
							total := report.Total
							moved := report.Moved
							reportMu.Unlock()
							elapsed := time.Since(start)
							requests, retries, _, reqPerMin := e.client.Metrics()
							slog.Info("still working...",
								"processed", total,
								"moved", moved,
								"requests", requests,
								"retries", retries,
								"req/min", fmt.Sprintf("%.1f", reqPerMin),
								"elapsed", elapsed.Round(time.Second))
						}
					}
				}
			}()

			listOpts := graph.ListOptions{Since: opts.Since, Before: before, Fast: opts.Fast}
			if opts.Fast {
				listOpts.Fields = []string{"id", "from", "subject", "toRecipients"}
			}
			err := e.client.StreamMessages(gctx, f.ID, listOpts, func(msg graph.Message) error {
				lastProgress.Store(time.Now().UnixNano())

				reportMu.Lock()
				report.Total++
				if !msg.Received.IsZero() {
					lastTime := folderTimes[f.Path]
					if lastTime.IsZero() || msg.Received.Before(lastTime) {
						folderTimes[f.Path] = msg.Received
					}
				}
				total := report.Total
				moved := report.Moved
				reportMu.Unlock()

				if total%50 == 0 {
					elapsed := time.Since(start)
					emailsPerSec := float64(total) / elapsed.Seconds()
					requests, retries, _, reqPerMin := e.client.Metrics()
					slog.Info("resort progress",
						"processed", total,
						"moved", moved,
						"emails/sec", fmt.Sprintf("%.1f", emailsPerSec),
						"requests", requests,
						"retries", retries,
						"req/min", fmt.Sprintf("%.1f", reqPerMin),
						"elapsed", elapsed.Round(time.Second))
					writeCheckpoint()
				}

				rule := Match(e.rules, msg, MatchOptions{Fast: opts.Fast})
				if rule == nil {
					reportMu.Lock()
					report.Unmatched++
					reportMu.Unlock()
					return nil
				}
				if rule.Folder == f.Path {
					return nil
				}

				destID := folderCache[rule.Folder]
				if !opts.DryRun {
					if _, err := e.client.MoveMessage(gctx, msg.ID, destID); err != nil {
						if errors.Is(err, graph.ErrMessageGone) {
							slog.Warn("message gone, skipping", "subject", msg.Subject, "from", msg.From)
							return nil
						}
						return err
					}
				}
				reportMu.Lock()
				report.Moved++
				report.Moves = append(report.Moves, MoveRecord{FromFolder: f.Path, ToFolder: rule.Folder, From: msg.From, Subject: msg.Subject})
				reportMu.Unlock()
				return nil
			})
			if err != nil {
				return err
			}
			reportMu.Lock()
			delete(folderTimes, f.Path)
			reportMu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := deleteResortCheckpoint(checkpointPath); err != nil {
		slog.Warn("delete checkpoint failed", "error", err)
	}
	return report, nil
}

// ResortSenderOptions controls resort-sender behavior.
type ResortSenderOptions struct {
	DryRun    bool
	Recursive bool
	Since     time.Duration
	Fast      bool
	ConfigDir string
}

// ResortSenderReport contains results of a sender-specific resort.
type ResortSenderReport struct {
	StartedAt time.Time
	Scanned   int // total messages scanned
	Matched   int // messages matching sender pattern
	Moved     int
	Unmatched int // matched sender but no rule matched
	Moves     []MoveRecord
}

// ResortSender re-sorts messages matching a sender pattern.
func (e *Engine) ResortSender(ctx context.Context, folder, senderPattern string, opts ResortSenderOptions) (*ResortSenderReport, error) {
	start := time.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	folderID, err := e.client.FindFolderIDByPath(ctx, folder)
	if err != nil {
		return nil, err
	}

	var folderIDs []graph.FolderInfo
	if opts.Recursive {
		folderIDs, err = e.client.ListFolderTree(ctx, folderID, folder)
		if err != nil {
			return nil, err
		}
	} else {
		folderIDs = []graph.FolderInfo{{ID: folderID, Path: folder}}
	}

	// Build folder ID cache from rules
	folderCache := make(map[string]string)
	for _, rule := range e.rules.Rules {
		if rule.Folder == "" {
			continue
		}
		if _, ok := folderCache[rule.Folder]; ok {
			continue
		}
		slog.Info("resolving folder", "folder", rule.Folder)
		id, err := e.client.FindFolderIDByPath(ctx, rule.Folder)
		if err != nil {
			return nil, fmt.Errorf("resolve folder %s: %w", rule.Folder, err)
		}
		folderCache[rule.Folder] = id
	}
	slog.Info("folder cache built", "count", len(folderCache))

	report := &ResortSenderReport{StartedAt: start}
	var reportMu sync.Mutex

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			slog.Warn("resort-sender interrupted")
			cancel()
		case <-ctx.Done():
		}
	}()

	workerLimit := e.cfg.Process.ResortWorkers
	if workerLimit <= 0 {
		workerLimit = 6
	}

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, workerLimit)
	for _, f := range folderIDs {
		f := f

		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			slog.Info("starting stream", "folder", f.Path, "pattern", senderPattern, "fast", opts.Fast)
			slog.Info("fetching messages from Graph API...")

			// Heartbeat goroutine to show we're still alive during slow API calls
			heartbeatCtx, heartbeatCancel := context.WithCancel(gctx)
			defer heartbeatCancel()
			var lastProgress atomic.Int64
			lastProgress.Store(time.Now().UnixNano())
			go func() {
				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-heartbeatCtx.Done():
						return
					case <-ticker.C:
						lastTime := time.Unix(0, lastProgress.Load())
						if time.Since(lastTime) > 10*time.Second {
							reportMu.Lock()
							scanned := report.Scanned
							moved := report.Moved
							reportMu.Unlock()
							elapsed := time.Since(start)
							requests, retries, _, reqPerMin := e.client.Metrics()
							slog.Info("still working...",
								"scanned", scanned,
								"moved", moved,
								"requests", requests,
								"retries", retries,
								"req/min", fmt.Sprintf("%.1f", reqPerMin),
								"elapsed", elapsed.Round(time.Second))
						}
					}
				}
			}()

			listOpts := graph.ListOptions{Since: opts.Since, Fast: opts.Fast, SenderFilter: senderPattern}
			if opts.Fast {
				listOpts.Fields = []string{"id", "from", "subject", "toRecipients", "receivedDateTime"}
			}
			err := e.client.StreamMessages(gctx, f.ID, listOpts, func(msg graph.Message) error {
				lastProgress.Store(time.Now().UnixNano())

				reportMu.Lock()
				report.Scanned++
				scanned := report.Scanned
				moved := report.Moved
				reportMu.Unlock()

				// Check if sender matches pattern (still needed for complex patterns
				// that can't be filtered server-side, and as a safety check)
				if !MatchSender(senderPattern, msg.From) {
					return nil
				}

				reportMu.Lock()
				report.Matched++
				reportMu.Unlock()

				if scanned%50 == 0 {
					elapsed := time.Since(start)
					emailsPerSec := float64(scanned) / elapsed.Seconds()
					requests, retries, _, reqPerMin := e.client.Metrics()
					slog.Info("resort-sender progress",
						"scanned", scanned,
						"matched", report.Matched,
						"moved", moved,
						"emails/sec", fmt.Sprintf("%.1f", emailsPerSec),
						"requests", requests,
						"retries", retries,
						"req/min", fmt.Sprintf("%.1f", reqPerMin),
						"elapsed", elapsed.Round(time.Second))
				}

				rule := Match(e.rules, msg, MatchOptions{Fast: opts.Fast})
				if rule == nil {
					reportMu.Lock()
					report.Unmatched++
					reportMu.Unlock()
					return nil
				}
				if rule.Folder == f.Path {
					return nil
				}

				destID := folderCache[rule.Folder]
				if !opts.DryRun {
					if _, err := e.client.MoveMessage(gctx, msg.ID, destID); err != nil {
						if errors.Is(err, graph.ErrMessageGone) {
							slog.Warn("message gone, skipping", "subject", msg.Subject, "from", msg.From)
							return nil
						}
						return err
					}
				}
				reportMu.Lock()
				report.Moved++
				report.Moves = append(report.Moves, MoveRecord{FromFolder: f.Path, ToFolder: rule.Folder, From: msg.From, Subject: msg.Subject})
				reportMu.Unlock()
				return nil
			})
			if err != nil {
				return err
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return report, nil
}

type GapReport struct {
	ByDomain map[string]int
}

// GapsOptions controls gaps behavior.
type GapsOptions struct {
	Fast      bool
	Recursive bool
}

func (e *Engine) Gaps(ctx context.Context, folder string, opts GapsOptions) (*GapReport, error) {
	folderID, err := e.client.FindFolderIDByPath(ctx, folder)
	if err != nil {
		return nil, err
	}

	var folderIDs []graph.FolderInfo
	if opts.Recursive {
		folderIDs, err = e.client.ListFolderTree(ctx, folderID, folder)
		if err != nil {
			return nil, err
		}
	} else {
		folderIDs = []graph.FolderInfo{{ID: folderID, Path: folder}}
	}

	result := &GapReport{ByDomain: map[string]int{}}
	for _, f := range folderIDs {
		err = e.client.StreamMessages(ctx, f.ID, graph.ListOptions{Fields: []string{"id", "from"}, Fast: opts.Fast}, func(msg graph.Message) error {
			if Match(e.rules, msg, MatchOptions{Fast: opts.Fast, IgnoreCatchall: true}) != nil {
				return nil
			}
			domain := domainFromEmail(msg.From)
			if domain == "" {
				domain = "(unknown)"
			}
			result.ByDomain[domain]++
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (e *Engine) String() string {
	return fmt.Sprintf("rules=%d", len(e.rules.Rules))
}
