package engine

import (
	"strings"

	"mailflow/internal/config"
	"mailflow/internal/graph"
)

// DebugCondition holds debug info for a single condition check.
type DebugCondition struct {
	Name          string   // e.g. "from_domain", "subject_contains"
	Matched       bool     // did this condition pass?
	Got           string   // actual value from email (single value)
	GotList       []string // actual values from email (list, e.g. To recipients)
	Want          []string // expected patterns/values
	MatchedValues []string // which patterns matched (if any)
	Note          string   // extra info (e.g. "skipped in fast mode")
}

// DebugRule holds debug info for a single rule.
type DebugRule struct {
	Rule       *config.Rule
	Matched    bool
	Conditions []DebugCondition
}

// DebugResult holds debug info for all rules.
type DebugResult struct {
	Rules       []DebugRule
	MatchedRule *config.Rule // first matching rule (nil if none)
}

// MatchWithDebug matches an email against all rules and returns detailed debug info.
func (e *Engine) MatchWithDebug(msg *graph.Message) (*DebugResult, error) {
	result := &DebugResult{}

	for i := range e.rules.Rules {
		rule := &e.rules.Rules[i]
		debugRule := debugRuleMatch(*rule, *msg, MatchOptions{})
		debugRule.Rule = rule
		result.Rules = append(result.Rules, debugRule)

		if debugRule.Matched && result.MatchedRule == nil {
			result.MatchedRule = rule
		}
	}

	return result, nil
}

func debugRuleMatch(rule config.Rule, msg graph.Message, opts MatchOptions) DebugRule {
	result := DebugRule{Matched: true}

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

	// from condition
	if len(rule.From) > 0 {
		cond := DebugCondition{
			Name: "from",
			Got:  from,
			Want: truncateList(rule.From, 5),
		}
		matched := false
		for _, pattern := range rule.From {
			if matchPattern(pattern, from, rule.CaseInsensitive) {
				matched = true
				cond.MatchedValues = append(cond.MatchedValues, pattern)
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// from_domain condition
	if len(rule.FromDomain) > 0 {
		cond := DebugCondition{
			Name: "from_domain",
			Got:  fromDomain,
			Want: truncateList(rule.FromDomain, 5),
		}
		matched := false
		for _, pattern := range rule.FromDomain {
			// Domains are always case-insensitive per RFC 1035
			if matchPattern(pattern, fromDomain, true) {
				matched = true
				cond.MatchedValues = append(cond.MatchedValues, pattern)
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// from_name condition
	if len(rule.FromName) > 0 {
		cond := DebugCondition{
			Name: "from_name",
			Got:  fromName,
			Want: truncateList(rule.FromName, 5),
		}
		matched := false
		for _, pattern := range rule.FromName {
			checkPattern := pattern
			if rule.CaseInsensitive {
				checkPattern = strings.ToLower(checkPattern)
			}
			if matchPattern(checkPattern, fromName, rule.CaseInsensitive) {
				matched = true
				cond.MatchedValues = append(cond.MatchedValues, pattern)
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// from_name_contains condition
	if len(rule.FromNameContains) > 0 {
		cond := DebugCondition{
			Name: "from_name_contains",
			Got:  fromName,
			Want: truncateList(rule.FromNameContains, 5),
		}
		matched := false
		checkName := fromName
		for _, needle := range rule.FromNameContains {
			checkNeedle := needle
			if rule.CaseInsensitive {
				checkNeedle = strings.ToLower(checkNeedle)
			}
			if strings.Contains(checkName, checkNeedle) {
				matched = true
				cond.MatchedValues = append(cond.MatchedValues, needle)
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// to condition
	if len(rule.To) > 0 {
		cond := DebugCondition{
			Name:    "to",
			GotList: toList,
			Want:    truncateList(rule.To, 5),
		}
		matched := false
		for _, t := range toList {
			for _, pattern := range rule.To {
				if matchPattern(pattern, t, rule.CaseInsensitive) {
					matched = true
					cond.MatchedValues = append(cond.MatchedValues, pattern)
					break
				}
			}
			if matched {
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// to_domain condition
	if len(rule.ToDomain) > 0 {
		var toDomains []string
		for _, t := range toList {
			toDomains = append(toDomains, domainFromEmail(t))
		}
		cond := DebugCondition{
			Name:    "to_domain",
			GotList: toDomains,
			Want:    truncateList(rule.ToDomain, 5),
		}
		matched := false
		for _, domain := range toDomains {
			for _, pattern := range rule.ToDomain {
				// Domains are always case-insensitive per RFC 1035
				if matchPattern(pattern, domain, true) {
					matched = true
					cond.MatchedValues = append(cond.MatchedValues, pattern)
					break
				}
			}
			if matched {
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// header_contains condition
	if len(rule.HeaderContains) > 0 {
		if opts.Fast {
			// In fast mode, headers aren't fetched
			for headerName, patterns := range rule.HeaderContains {
				cond := DebugCondition{
					Name:    "header_contains[" + headerName + "]",
					Got:     "(not fetched)",
					Want:    truncateList(patterns, 5),
					Matched: false,
					Note:    "skipped in fast mode",
				}
				result.Conditions = append(result.Conditions, cond)
			}
			result.Matched = false
		} else {
			for headerName, patterns := range rule.HeaderContains {
				headerValue := getHeaderCaseInsensitive(msg.Headers, headerName)
				got := headerValue
				if got == "" {
					got = "(not present)"
				}
				cond := DebugCondition{
					Name: "header_contains[" + headerName + "]",
					Got:  truncateString(got, 100),
					Want: truncateList(patterns, 5),
				}
				matched := false
				if headerValue != "" {
					for _, pattern := range patterns {
						cmpPattern := pattern
						cmpValue := headerValue
						if rule.CaseInsensitive {
							cmpPattern = strings.ToLower(cmpPattern)
							cmpValue = strings.ToLower(cmpValue)
						}
						if strings.Contains(cmpValue, cmpPattern) {
							matched = true
							cond.MatchedValues = append(cond.MatchedValues, pattern)
							break
						}
					}
				}
				cond.Matched = matched
				result.Conditions = append(result.Conditions, cond)
				if !matched {
					result.Matched = false
				}
			}
		}
	}

	// body_contains condition
	if len(rule.BodyContains) > 0 {
		cond := DebugCondition{
			Name: "body_contains",
			Got:  truncateString(body, 100),
			Want: truncateList(rule.BodyContains, 5),
		}
		if opts.Fast {
			cond.Matched = false
			cond.Note = "skipped in fast mode"
			result.Conditions = append(result.Conditions, cond)
			result.Matched = false
		} else {
			matched := false
			for _, s := range rule.BodyContains {
				cmp := s
				if rule.CaseInsensitive {
					cmp = strings.ToLower(cmp)
				}
				if strings.Contains(body, cmp) {
					matched = true
					cond.MatchedValues = append(cond.MatchedValues, s)
					break
				}
			}
			cond.Matched = matched
			result.Conditions = append(result.Conditions, cond)
			if !matched {
				result.Matched = false
			}
		}
	}

	// body_not_contains condition (exclusion)
	if len(rule.BodyNotContains) > 0 {
		cond := DebugCondition{
			Name: "body_not_contains",
			Got:  truncateString(body, 100),
			Want: truncateList(rule.BodyNotContains, 5),
		}
		matched := true // starts true, fails if any pattern found
		for _, s := range rule.BodyNotContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(body, cmp) {
				matched = false
				cond.MatchedValues = append(cond.MatchedValues, s)
				cond.Note = "exclusion pattern found"
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// body_prefix_contains condition
	if len(rule.BodyPrefixContains) > 0 {
		prefixLen := rule.BodyPrefixLength
		if prefixLen <= 0 {
			prefixLen = 1000
		}
		prefix := body
		if len(prefix) > prefixLen {
			prefix = prefix[:prefixLen]
		}
		cond := DebugCondition{
			Name: "body_prefix_contains",
			Got:  truncateString(prefix, 100),
			Want: truncateList(rule.BodyPrefixContains, 5),
		}
		if opts.Fast {
			cond.Matched = false
			cond.Note = "skipped in fast mode"
			result.Conditions = append(result.Conditions, cond)
			result.Matched = false
		} else {
			matched := false
			for _, s := range rule.BodyPrefixContains {
				cmp := s
				if rule.CaseInsensitive {
					cmp = strings.ToLower(cmp)
				}
				if strings.Contains(prefix, cmp) {
					matched = true
					cond.MatchedValues = append(cond.MatchedValues, s)
					break
				}
			}
			cond.Matched = matched
			result.Conditions = append(result.Conditions, cond)
			if !matched {
				result.Matched = false
			}
		}
	}

	// subject_contains condition
	if len(rule.SubjectContains) > 0 {
		cond := DebugCondition{
			Name: "subject_contains",
			Got:  subject,
			Want: truncateList(rule.SubjectContains, 5),
		}
		matched := false
		for _, s := range rule.SubjectContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(subject, cmp) {
				matched = true
				cond.MatchedValues = append(cond.MatchedValues, s)
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// subject_not_contains condition (exclusion)
	if len(rule.SubjectNotContains) > 0 {
		cond := DebugCondition{
			Name: "subject_not_contains",
			Got:  subject,
			Want: truncateList(rule.SubjectNotContains, 5),
		}
		matched := true // starts true, fails if any pattern found
		for _, s := range rule.SubjectNotContains {
			cmp := s
			if rule.CaseInsensitive {
				cmp = strings.ToLower(cmp)
			}
			if strings.Contains(subject, cmp) {
				matched = false
				cond.MatchedValues = append(cond.MatchedValues, s)
				cond.Note = "exclusion pattern found"
				break
			}
		}
		cond.Matched = matched
		result.Conditions = append(result.Conditions, cond)
		if !matched {
			result.Matched = false
		}
	}

	// catchall rule (no conditions = matches everything)
	if rule.Catchall {
		cond := DebugCondition{
			Name:    "catchall",
			Matched: true,
			Note:    "matches all emails",
		}
		result.Conditions = append(result.Conditions, cond)
	}

	return result
}

func truncateList(list []string, max int) []string {
	if len(list) <= max {
		return list
	}
	result := make([]string, max)
	copy(result, list[:max])
	result[max-1] = "..."
	return result
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
