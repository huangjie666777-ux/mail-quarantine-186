package main

import (
	"errors"
	"fmt"
	"strings"
)

const (
	actionAllow      = "allow"
	actionQuarantine = "quarantine"
)

type Rule struct {
	ID           string `json:"id"`
	Recipient    string `json:"recipient"`
	SenderDomain string `json:"sender_domain"`
	Action       string `json:"action"`
}

type RuleSet struct {
	Version int64  `json:"version"`
	Rules   []Rule `json:"rules"`
}

type ruleMatch struct {
	rule    *Rule
	version int64
}

func normalizeRule(rule Rule, allowed map[string]struct{}) (Rule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	if rule.ID == "" || strings.ContainsAny(rule.ID, " \t\r\n") {
		return Rule{}, errors.New("rule id is required")
	}
	recipient, err := CanonicalRecipient(rule.Recipient)
	if err != nil {
		return Rule{}, fmt.Errorf("rule %q recipient: %w", rule.ID, err)
	}
	if allowed != nil {
		if _, ok := allowed[recipient]; !ok {
			return Rule{}, fmt.Errorf("rule %q recipient is not allowed", rule.ID)
		}
	}
	domain := strings.ToLower(strings.TrimSpace(rule.SenderDomain))
	if domain != "" && domain != "*" && !validDomain(domain) {
		return Rule{}, fmt.Errorf("rule %q sender domain is invalid", rule.ID)
	}
	action := strings.ToLower(strings.TrimSpace(rule.Action))
	if action != actionAllow && action != actionQuarantine {
		return Rule{}, fmt.Errorf("rule %q action must be allow or quarantine", rule.ID)
	}
	return Rule{ID: rule.ID, Recipient: recipient, SenderDomain: domain, Action: action}, nil
}

func normalizeRules(rules []Rule, allowed map[string]struct{}) ([]Rule, error) {
	seen := make(map[string]struct{}, len(rules))
	normalized := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		item, err := normalizeRule(rule, allowed)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[item.ID]; exists {
			return nil, fmt.Errorf("duplicate rule id %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized, nil
}

func matchRule(rules []Rule, recipient, sender string) *Rule {
	domain := ""
	if at := strings.LastIndexByte(sender, '@'); at >= 0 && at < len(sender)-1 {
		domain = strings.ToLower(sender[at+1:])
	}
	for index := range rules {
		rule := &rules[index]
		if rule.Recipient != recipient {
			continue
		}
		if rule.SenderDomain == "*" || rule.SenderDomain == domain {
			return rule
		}
	}
	return nil
}
