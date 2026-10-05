package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	ActionAllow       = "allow"
	ActionQuarantine  = "quarantine"
	StatusDelivered   = "delivered"
	StatusQuarantined = "quarantined"
	StatusReleased    = "released"
	StatusDiscarded   = "discarded"
	StatusPOP3Deleted = "pop3_deleted"
)

var (
	ErrStaleRules       = errors.New("stale rules version")
	ErrConflictDecision = errors.New("conflicting quarantine decision")
	ErrNotQuarantined   = errors.New("message is not quarantined for recipient")
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

type RuleMatch struct {
	RuleID string
	Action string
}

func (rules RuleSet) Match(recipient, sender string) RuleMatch {
	senderDomain := senderDomain(sender)
	for _, rule := range rules.Rules {
		if rule.Recipient != recipient {
			continue
		}
		if rule.SenderDomain == "*" || strings.EqualFold(rule.SenderDomain, senderDomain) {
			return RuleMatch{RuleID: rule.ID, Action: rule.Action}
		}
	}
	return RuleMatch{Action: ActionAllow}
}

func senderDomain(sender string) string {
	at := strings.LastIndexByte(sender, '@')
	if at < 0 {
		return ""
	}
	return sender[at+1:]
}

func (s *Store) GetRules(ctx context.Context) (RuleSet, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RuleSet{}, err
	}
	defer tx.Rollback()
	return loadRules(ctx, tx)
}

func loadRules(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (RuleSet, error) {
	var rules RuleSet
	if err := query.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'rules_version'`).Scan(&rules.Version); err != nil {
		return RuleSet{}, err
	}
	rows, err := query.QueryContext(ctx, `SELECT id, recipient, sender_domain, action FROM rules ORDER BY position`)
	if err != nil {
		return RuleSet{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule Rule
		if err := rows.Scan(&rule.ID, &rule.Recipient, &rule.SenderDomain, &rule.Action); err != nil {
			return RuleSet{}, err
		}
		rules.Rules = append(rules.Rules, rule)
	}
	return rules, rows.Err()
}

func (s *Store) ReplaceRules(ctx context.Context, expectedVersion int64, incoming []Rule) (RuleSet, error) {
	normalized := make([]Rule, 0, len(incoming))
	seen := make(map[string]struct{}, len(incoming))
	for _, rule := range incoming {
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			return RuleSet{}, fmt.Errorf("rule id is required")
		}
		if _, duplicate := seen[rule.ID]; duplicate {
			return RuleSet{}, fmt.Errorf("duplicate rule id %q", rule.ID)
		}
		seen[rule.ID] = struct{}{}
		canonical, err := CanonicalRecipient(rule.Recipient)
		if err != nil {
			return RuleSet{}, fmt.Errorf("rule %q recipient: %w", rule.ID, err)
		}
		if _, allowed := s.config.Recipients[canonical]; !allowed {
			return RuleSet{}, fmt.Errorf("rule %q recipient is not allowed", rule.ID)
		}
		domain := strings.TrimSpace(rule.SenderDomain)
		if domain != "" && domain != "*" {
			domain = strings.ToLower(domain)
			if !validDomain(domain) {
				return RuleSet{}, fmt.Errorf("rule %q sender domain is invalid", rule.ID)
			}
		}
		if rule.Action != ActionAllow && rule.Action != ActionQuarantine {
			return RuleSet{}, fmt.Errorf("rule %q action must be allow or quarantine", rule.ID)
		}
		normalized = append(normalized, Rule{ID: rule.ID, Recipient: canonical, SenderDomain: domain, Action: rule.Action})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RuleSet{}, err
	}
	defer tx.Rollback()
	current, err := loadRules(ctx, tx)
	if err != nil {
		return RuleSet{}, err
	}
	if expectedVersion != current.Version {
		return RuleSet{}, ErrStaleRules
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rules`); err != nil {
		return RuleSet{}, err
	}
	for position, rule := range normalized {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rules(id, position, recipient, sender_domain, action) VALUES (?, ?, ?, ?, ?)`,
			rule.ID, position, rule.Recipient, rule.SenderDomain, rule.Action); err != nil {
			return RuleSet{}, err
		}
	}
	next := current.Version + 1
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'rules_version'`, next); err != nil {
		return RuleSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return RuleSet{}, err
	}
	return RuleSet{Version: next, Rules: normalized}, nil
}
