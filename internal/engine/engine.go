package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
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
	cfg    *config.Config
	rules  *config.RuleSet
	client *graph.Client
}

func New(cfg *config.Config, rules *config.RuleSet, client *graph.Client) *Engine {
	return &Engine{cfg: cfg, rules: rules, client: client}
}

func (e *Engine) Reload(cfg *config.Config, rules *config.RuleSet) error {
	e.cfg = cfg
	e.rules = rules
	return nil
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
		rule := Match(e.rules, msg, MatchOptions{})
		if rule == nil {
			continue
		}

		destID, err := e.client.FindFolderIDByPath(ctx, rule.Folder)
		if err != nil {
			return err
		}
		if err := e.client.MoveMessage(ctx, msg.ID, destID); err != nil {
			return err
		}
		slog.Info("moved", "id", msg.ID, "from", msg.From, "subject", msg.Subject, "rule", rule.Name, "folder", rule.Folder)

		if rule.OnMatch != nil {
			if rule.OnMatch.MarkRead {
				if err := e.client.MarkRead(ctx, msg.ID); err != nil {
					slog.Warn("mark read failed", "id", msg.ID, "error", err)
				}
			}
			if rule.OnMatch.Pushover != nil {
				e.sendPushover(msg, rule)
			}
		}
	}

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

	if err := pushover.Send(payload); err != nil {
		slog.Warn("pushover send failed", "error", err)
	}
}

// MatchOptions controls rule matching behavior.
type MatchOptions struct {
	Fast bool
}

// Match returns the first rule that matches.
func Match(rules *config.RuleSet, msg graph.Message, opts MatchOptions) *config.Rule {
	for i := range rules.Rules {
		rule := &rules.Rules[i]
		if ruleMatches(*rule, msg, opts) {
			return rule
		}
	}
	return nil
}

func ruleMatches(rule config.Rule, msg graph.Message, opts MatchOptions) bool {
	subject := msg.Subject
	body := msg.Body
	from := msg.From
	fromDomain := domainFromEmail(from)
	toList := msg.To

	if rule.CaseInsensitive {
		subject = strings.ToLower(subject)
		body = strings.ToLower(body)
		from = strings.ToLower(from)
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

	if len(rule.FromDomain) > 0 {
		matched := false
		for _, pattern := range rule.FromDomain {
			if matchPattern(pattern, fromDomain, rule.CaseInsensitive) {
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
				if matchPattern(pattern, domain, rule.CaseInsensitive) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}

	if opts.Fast && len(rule.BodyContains) > 0 {
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

// BuildPushover extracts data and builds payload.
func BuildPushover(cfg *config.PushoverRule, msg graph.Message) pushover.Payload {
	vars := map[string]string{
		"subject":   msg.Subject,
		"from":      msg.From,
		"from_name": msg.FromName,
		"to":        strings.Join(msg.To, ","),
		"snippet":   msg.Snippet,
		"body":      msg.Body,
		"body_html": msg.BodyHTML,
	}

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
			if len(matches) > 1 {
				vars[extract.Capture] = matches[1]
			}
		} else {
			for i, name := range re.SubexpNames() {
				if i == 0 || name == "" {
					continue
				}
				vars[name] = matches[i]
			}
		}
		// Use first successful extraction
		break
	}

	message := cfg.Message
	if message == "" {
		message = cfg.Fallback
	}

	if message == "" {
		message = "${subject}"
	}

	return pushover.Payload{
		Title:   expandVars(cfg.Title, vars),
		Message: expandVars(message, vars),
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
	DryRun    bool
	Recursive bool
	Since     time.Duration
	Fast      bool
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
	fmt.Fprintln(os.Stderr, "DEBUG: building folder cache")
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

			slog.Info("starting stream", "folder", f.Path, "fast", opts.Fast)
			listOpts := graph.ListOptions{Since: opts.Since, Fast: opts.Fast}
			if opts.Fast {
				listOpts.Fields = []string{"id", "from", "subject", "toRecipients"}
			}
			err := e.client.StreamMessages(gctx, f.ID, listOpts, func(msg graph.Message) error {
				reportMu.Lock()
				report.Total++
				reportMu.Unlock()

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
					if err := e.client.MoveMessage(gctx, msg.ID, destID); err != nil {
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
	Fast bool
}

func (e *Engine) Gaps(ctx context.Context, folder string, opts GapsOptions) (*GapReport, error) {
	folderID, err := e.client.FindFolderIDByPath(ctx, folder)
	if err != nil {
		return nil, err
	}
	result := &GapReport{ByDomain: map[string]int{}}
	err = e.client.StreamMessages(ctx, folderID, graph.ListOptions{Fields: []string{"id", "from"}, Fast: opts.Fast}, func(msg graph.Message) error {
		if Match(e.rules, msg, MatchOptions{Fast: opts.Fast}) != nil {
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

	return result, nil
}

func (e *Engine) String() string {
	return fmt.Sprintf("rules=%d", len(e.rules.Rules))
}
