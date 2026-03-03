package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateOptions controls validation behavior.
type ValidateOptions struct {
	OnlyRules map[string]bool
	Detailed  bool
}

// ValidationIssue represents a validation warning or error.
type ValidationIssue struct {
	RuleName   string
	RuleSource string
	Message    string
}

// ValidationReport contains validation errors and warnings.
type ValidationReport struct {
	Errors   []ValidationIssue
	Warnings []ValidationIssue
}

// ApplyStrict converts warnings into errors.
func (r *ValidationReport) ApplyStrict() {
	if len(r.Warnings) == 0 {
		return
	}
	r.Errors = append(r.Errors, r.Warnings...)
	r.Warnings = nil
}

// ValidateRuleSet runs static validation checks on a ruleset.
func ValidateRuleSet(rules *RuleSet, opts ValidateOptions) ValidationReport {
	report := ValidationReport{}
	report.Errors = append(report.Errors, duplicateRuleNameIssues(rules)...)

	overlaps := findBroadFromOverlaps(rules, opts.OnlyRules)
	for _, overlap := range overlaps {
		report.Warnings = append(report.Warnings, ValidationIssue{
			RuleName:   overlap.RuleName,
			RuleSource: overlap.RuleSource,
			Message:    formatBroadFromOverlap(overlap, opts.Detailed),
		})
	}

	return report
}

type broadFromOverlap struct {
	RuleName   string
	RuleSource string
	Domain     string
	FromList   []string
	Matches    []overlapMatch
}

type overlapMatch struct {
	RuleName      string
	RuleSource    string
	Patterns      []string
	Catchall      bool
	LowerPriority bool
}

func duplicateRuleNameIssues(rules *RuleSet) []ValidationIssue {
	seen := map[string][]string{}
	for _, rule := range rules.Rules {
		if rule.Name == "" {
			continue
		}
		seen[rule.Name] = append(seen[rule.Name], rule.Source)
	}

	var issues []ValidationIssue
	for name, sources := range seen {
		if len(sources) < 2 {
			continue
		}
		uniqueSources := uniqueStrings(sources)
		sort.Strings(uniqueSources)
		issues = append(issues, ValidationIssue{
			RuleName: name,
			Message:  fmt.Sprintf("duplicate rule name %q found in %s", name, strings.Join(uniqueSources, ", ")),
		})
	}

	sort.Slice(issues, func(i, j int) bool {
		return issues[i].RuleName < issues[j].RuleName
	})
	return issues
}

func findBroadFromOverlaps(rules *RuleSet, onlyRules map[string]bool) []broadFromOverlap {
	var warnings []broadFromOverlap

	for i, rule := range rules.Rules {
		if len(rule.From) == 0 {
			continue
		}
		if onlyRules != nil && !onlyRules[rule.Name] {
			continue
		}

		domains := map[string][]string{}
		for _, from := range rule.From {
			domain := domainFromAddress(from)
			if domain == "" {
				continue
			}
			domains[domain] = append(domains[domain], from)
		}
		if len(domains) == 0 {
			continue
		}

		for domain, fromList := range domains {
			var matches []overlapMatch
			for j, other := range rules.Rules {
				if j == i {
					continue
				}
				if !other.Catchall && j <= i {
					continue
				}
				if len(other.FromDomain) == 0 {
					continue
				}

				matchedPatterns := matchDomainPatterns(domain, other.FromDomain)
				if len(matchedPatterns) == 0 {
					continue
				}

				matches = append(matches, overlapMatch{
					RuleName:      other.Name,
					RuleSource:    other.Source,
					Patterns:      matchedPatterns,
					Catchall:      other.Catchall,
					LowerPriority: j > i,
				})
			}

			if len(matches) == 0 {
				continue
			}

			sort.Strings(fromList)
			warnings = append(warnings, broadFromOverlap{
				RuleName:   rule.Name,
				RuleSource: rule.Source,
				Domain:     domain,
				FromList:   fromList,
				Matches:    matches,
			})
		}
	}

	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].RuleName == warnings[j].RuleName {
			return warnings[i].Domain < warnings[j].Domain
		}
		return warnings[i].RuleName < warnings[j].RuleName
	})

	return warnings
}

func formatBroadFromOverlap(overlap broadFromOverlap, detailed bool) string {
	fromText := strings.Join(overlap.FromList, ", ")

	var matchTexts []string
	for _, match := range overlap.Matches {
		priority := "lower-priority"
		if match.Catchall {
			priority = "catch-all"
		}
		if match.Catchall && match.LowerPriority {
			priority = "lower-priority catch-all"
		}
		patternText := strings.Join(match.Patterns, ", ")
		if detailed {
			matchTexts = append(matchTexts, fmt.Sprintf("%s rule %q (%s, from_domain: %s)",
				priority, match.RuleName, match.RuleSource, patternText))
		} else {
			matchTexts = append(matchTexts, fmt.Sprintf("%s rule %q (from_domain: %s)",
				priority, match.RuleName, patternText))
		}
	}

	sort.Strings(matchTexts)

	if detailed {
		return fmt.Sprintf("rule %q (%s) from %s uses domain %q that matches %s", overlap.RuleName, overlap.RuleSource, fromText, overlap.Domain, strings.Join(matchTexts, "; "))
	}
	return fmt.Sprintf("rule %q from %s uses domain %q that matches %s", overlap.RuleName, fromText, overlap.Domain, strings.Join(matchTexts, "; "))
}

func matchDomainPatterns(domain string, patterns []string) []string {
	var matched []string
	for _, pattern := range patterns {
		if matchPattern(pattern, domain) {
			matched = append(matched, pattern)
		}
	}
	return matched
}

func domainFromAddress(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at == -1 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(addr[at+1:])
}

var regexCache = map[string]*regexp.Regexp{}

func matchPattern(pattern, value string) bool {
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)
	re := cachedRegex(pattern)
	return re.MatchString(value)
}

func cachedRegex(pattern string) *regexp.Regexp {
	if re, ok := regexCache[pattern]; ok {
		return re
	}
	re := regexp.MustCompile("^" + globToRegex(pattern) + "$")
	regexCache[pattern] = re
	return re
}

func globToRegex(glob string) string {
	escaped := regexp.QuoteMeta(glob)
	escaped = strings.ReplaceAll(escaped, "\\*", ".*")
	escaped = strings.ReplaceAll(escaped, "\\?", ".")
	return escaped
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// ParseRuleFile parses a rules file into a flat list of rules.
func ParseRuleFile(data []byte, source string) ([]Rule, error) {
	var rf RuleFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, err
	}

	var rules []Rule
	for _, folder := range rf.Folders {
		for _, rule := range folder.Rules {
			rule.Folder = folder.Path
			rule.Source = filepath.Base(source)
			rules = append(rules, rule)
		}
	}

	for _, rule := range rf.Rules {
		if rule.Folder == "" && !rule.NotifyOnly {
			return nil, fmt.Errorf("rule missing folder")
		}
		rule.Source = filepath.Base(source)
		rules = append(rules, rule)
	}

	return rules, nil
}

type ruleSnapshot struct {
	Rule
	FromDomainRefs []string `json:"from_domain_refs,omitempty"`
	ToDomainRefs   []string `json:"to_domain_refs,omitempty"`
}

// RuleSignature returns a stable JSON representation for diffing.
func RuleSignature(rule Rule) (string, error) {
	snapshot := ruleSnapshot{
		Rule:           rule,
		FromDomainRefs: rule.fromDomainRefs,
		ToDomainRefs:   rule.toDomainRefs,
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
