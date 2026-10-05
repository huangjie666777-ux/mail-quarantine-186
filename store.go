package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Message struct {
	ID           string    `json:"id"`
	ReceivedAt   time.Time `json:"received_at"`
	Sender       string    `json:"sender"`
	Recipients   []string  `json:"recipients"`
	Size         int       `json:"size"`
	Status       string    `json:"status,omitempty"`
	RuleID       string    `json:"rule_id,omitempty"`
	RulesVersion int64     `json:"rules_version,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

type QuarantineItem struct {
	Message
	Decision  string     `json:"decision"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

type RecipientState struct {
	Recipient    string
	Status       string
	RuleID       string
	RulesVersion int64
	Decision     string
	DecidedAt    string
}

type StoredMessage struct {
	Message
	Raw []byte `json:"-"`
}

type Store struct {
	db     *sql.DB
	config Config
}

func NewStore(path string, config ...Config) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	store := &Store{db: db, config: cfg}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	statements := `
CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	received_at TEXT NOT NULL,
	sender TEXT NOT NULL,
	raw BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS message_recipients (
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	recipient TEXT NOT NULL,
	PRIMARY KEY(message_id, recipient)
);
CREATE INDEX IF NOT EXISTS idx_message_recipients_recipient ON message_recipients(recipient);
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rules (
	id TEXT NOT NULL,
	position INTEGER NOT NULL,
	recipient TEXT NOT NULL,
	sender_domain TEXT NOT NULL,
	action TEXT NOT NULL CHECK(action IN ('allow', 'quarantine')),
	PRIMARY KEY(id)
);
CREATE INDEX IF NOT EXISTS idx_rules_match ON rules(recipient);
`
	if _, err := s.db.ExecContext(ctx, statements); err != nil {
		return err
	}
	columns := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(message_recipients)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, addition := range []struct {
		name string
		ddl  string
	}{
		{"status", "ALTER TABLE message_recipients ADD COLUMN status TEXT NOT NULL DEFAULT 'delivered'"},
		{"rule_id", "ALTER TABLE message_recipients ADD COLUMN rule_id TEXT NOT NULL DEFAULT ''"},
		{"rules_version", "ALTER TABLE message_recipients ADD COLUMN rules_version INTEGER NOT NULL DEFAULT 0"},
		{"decision", "ALTER TABLE message_recipients ADD COLUMN decision TEXT NOT NULL DEFAULT ''"},
		{"decided_at", "ALTER TABLE message_recipients ADD COLUMN decided_at TEXT"},
	} {
		if !columns[addition.name] {
			if _, err := s.db.ExecContext(ctx, addition.ddl); err != nil {
				return err
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES('rules_version', '0')`); err != nil {
		return err
	}
	return nil
}

func (s *Store) Save(ctx context.Context, sender string, recipients []string, raw []byte) (string, error) {
	states := make([]RecipientState, 0, len(recipients))
	for _, recipient := range recipients {
		states = append(states, RecipientState{Recipient: recipient, Status: StatusDelivered})
	}
	return s.SaveWithStates(ctx, sender, states, raw, nil)
}

func (s *Store) SaveWithRules(ctx context.Context, sender string, recipients []string, raw []byte) (string, error) {
	id, err := newMessageID()
	if err != nil {
		return "", err
	}
	receivedAt := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	rules, err := loadRules(ctx, tx)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages(id, received_at, sender, raw) VALUES (?, ?, ?, ?)`,
		id, receivedAt, sender, raw); err != nil {
		return "", err
	}
	for _, recipient := range recipients {
		match := rules.Match(recipient, sender)
		status := StatusDelivered
		if match.Action == ActionQuarantine {
			status = StatusQuarantined
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_recipients(message_id, recipient, status, rule_id, rules_version, decision, decided_at) VALUES (?, ?, ?, ?, ?, '', NULL)`,
			id, recipient, status, match.RuleID, rules.Version); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) SaveWithStates(ctx context.Context, sender string, states []RecipientState, raw []byte, rules *RuleSet) (string, error) {
	id, err := newMessageID()
	if err != nil {
		return "", err
	}
	receivedAt := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages(id, received_at, sender, raw) VALUES (?, ?, ?, ?)`,
		id, receivedAt, sender, raw); err != nil {
		return "", err
	}
	for _, state := range states {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_recipients(message_id, recipient, status, rule_id, rules_version, decision, decided_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, state.Recipient, state.Status, state.RuleID, state.RulesVersion, state.Decision, state.DecidedAt); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) ListByRecipient(ctx context.Context, recipient string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.received_at, m.sender, length(m.raw), r.status, r.rule_id, r.rules_version
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE r.recipient = ?
ORDER BY m.received_at DESC, m.id DESC`, recipient)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type messageRow struct {
		message    Message
		receivedAt string
	}
	rowsData := make([]messageRow, 0)
	for rows.Next() {
		var message Message
		var receivedAt string
		if err := rows.Scan(&message.ID, &receivedAt, &message.Sender, &message.Size, &message.Status, &message.RuleID, &message.RulesVersion); err != nil {
			return nil, err
		}
		message.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
		if err != nil {
			return nil, err
		}
		rowsData = append(rowsData, messageRow{message: message, receivedAt: receivedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	messages := make([]Message, 0, len(rowsData))
	for index := range rowsData {
		row := rowsData[index]
		message := row.message
		var err error
		message.ReceivedAt, err = time.Parse(time.RFC3339Nano, row.receivedAt)
		if err != nil {
			return nil, err
		}
		message.Recipients, err = s.recipientsFor(ctx, message.ID)
		if err != nil {
			return nil, err
		}
		message.Reason = stateReason(message.Status, message.RuleID, message.RulesVersion)
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) GetForRecipient(ctx context.Context, id, recipient string, includeRaw bool) (StoredMessage, bool, error) {
	if !validMessageID(id) {
		return StoredMessage{}, false, nil
	}
	var stored StoredMessage
	var receivedAt string
	rawColumn := "x''"
	if includeRaw {
		rawColumn = "m.raw"
	}
	err := s.db.QueryRowContext(ctx, `
SELECT m.id, m.received_at, m.sender, `+rawColumn+`, length(m.raw), requested.status, requested.rule_id, requested.rules_version
FROM messages m
JOIN message_recipients requested ON requested.message_id = m.id AND requested.recipient = ?
WHERE m.id = ?`, recipient, id).
		Scan(&stored.ID, &receivedAt, &stored.Sender, &stored.Raw, &stored.Size, &stored.Status, &stored.RuleID, &stored.RulesVersion)
	if err == sql.ErrNoRows {
		return StoredMessage{}, false, nil
	}
	if err != nil {
		return StoredMessage{}, false, err
	}
	stored.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
	if err != nil {
		return StoredMessage{}, false, err
	}
	stored.Recipients, err = s.recipientsFor(ctx, id)
	if err != nil {
		return StoredMessage{}, false, err
	}
	stored.Reason = stateReason(stored.Status, stored.RuleID, stored.RulesVersion)
	return stored, true, nil
}

func (s *Store) recipientsFor(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT recipient FROM message_recipients WHERE message_id = ? ORDER BY recipient`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recipients := make([]string, 0)
	for rows.Next() {
		var recipient string
		if err := rows.Scan(&recipient); err != nil {
			return nil, err
		}
		recipients = append(recipients, recipient)
	}
	return recipients, rows.Err()
}

type MessageRef struct {
	ID   string
	Size int
}

func (s *Store) ListIDsForRecipient(ctx context.Context, recipient string) ([]MessageRef, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, length(m.raw)
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE r.recipient = ?
  AND r.status IN (?, ?)
ORDER BY m.received_at ASC, m.id ASC`, recipient, StatusDelivered, StatusReleased)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := make([]MessageRef, 0)
	for rows.Next() {
		var ref MessageRef
		if err := rows.Scan(&ref.ID, &ref.Size); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (s *Store) DeleteForRecipient(ctx context.Context, recipient string, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE message_recipients SET status = ? WHERE message_id = ? AND recipient = ?`,
			StatusPOP3Deleted, id, recipient); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListQuarantine(ctx context.Context, recipient string) ([]QuarantineItem, error) {
	query := `
SELECT m.id, m.received_at, m.sender, length(m.raw), r.status, r.rule_id, r.rules_version, r.decision, COALESCE(r.decided_at, ''), r.recipient
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE r.status = ?`
	args := []any{StatusQuarantined}
	if recipient != "" {
		query += ` AND r.recipient = ?`
		args = append(args, recipient)
	}
	query += ` ORDER BY m.received_at DESC, m.id DESC, r.recipient`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]QuarantineItem, 0)
	for rows.Next() {
		var item QuarantineItem
		var receivedAt, decidedAt string
		var recipient string
		if err := rows.Scan(&item.ID, &receivedAt, &item.Sender, &item.Size, &item.Status, &item.RuleID, &item.RulesVersion, &item.Decision, &decidedAt, &recipient); err != nil {
			return nil, err
		}
		item.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
		if err != nil {
			return nil, err
		}
		item.Recipients = []string{recipient}
		item.Reason = quarantineReason(item.RuleID, item.RulesVersion)
		if decidedAt != "" {
			decided, err := time.Parse(time.RFC3339Nano, decidedAt)
			if err != nil {
				return nil, err
			}
			item.DecidedAt = &decided
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func quarantineReason(ruleID string, version int64) string {
	if ruleID == "" {
		return fmt.Sprintf("quarantined by rules version %d", version)
	}
	return fmt.Sprintf("quarantined by rule %s in rules version %d", ruleID, version)
}

func stateReason(status, ruleID string, version int64) string {
	switch status {
	case StatusQuarantined, StatusReleased, StatusDiscarded:
		return quarantineReason(ruleID, version)
	default:
		return ""
	}
}

func (s *Store) DecideQuarantine(ctx context.Context, id, recipient, decision string) (QuarantineItem, bool, error) {
	if decision != StatusReleased && decision != StatusDiscarded {
		return QuarantineItem{}, false, errors.New("invalid decision")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QuarantineItem{}, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE message_recipients
SET status = ?, decision = ?, decided_at = ?
WHERE message_id = ? AND recipient = ? AND status = ?`,
		decision, decision, now, id, recipient, StatusQuarantined)
	if err != nil {
		return QuarantineItem{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return QuarantineItem{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return QuarantineItem{}, false, err
	}
	item, found, err := s.GetQuarantineDecision(ctx, id, recipient)
	if err != nil || !found {
		return QuarantineItem{}, false, err
	}
	if affected == 0 {
		if item.Decision == decision {
			return item, true, nil
		}
		return item, true, ErrNotQuarantined
	}
	return item, true, nil
}

func (s *Store) GetQuarantineDecision(ctx context.Context, id, recipient string) (QuarantineItem, bool, error) {
	if !validMessageID(id) {
		return QuarantineItem{}, false, nil
	}
	var item QuarantineItem
	var receivedAt, decidedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT m.id, m.received_at, m.sender, length(m.raw), r.status, r.rule_id, r.rules_version, r.decision, COALESCE(r.decided_at, '')
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE m.id = ? AND r.recipient = ?`, id, recipient).
		Scan(&item.ID, &receivedAt, &item.Sender, &item.Size, &item.Status, &item.RuleID, &item.RulesVersion, &item.Decision, &decidedAt)
	if err == sql.ErrNoRows {
		return QuarantineItem{}, false, nil
	}
	if err != nil {
		return QuarantineItem{}, false, err
	}
	item.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
	if err != nil {
		return QuarantineItem{}, false, err
	}
	item.Recipients = []string{recipient}
	item.Reason = quarantineReason(item.RuleID, item.RulesVersion)
	if decidedAt != "" {
		decided, err := time.Parse(time.RFC3339Nano, decidedAt)
		if err != nil {
			return QuarantineItem{}, false, err
		}
		item.DecidedAt = &decided
	}
	return item, true, nil
}

func newMessageID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validMessageID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, char := range id {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
