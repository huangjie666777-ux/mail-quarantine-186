package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Message struct {
	ID         string     `json:"id"`
	ReceivedAt time.Time  `json:"received_at"`
	Sender     string     `json:"sender"`
	Recipients []string   `json:"recipients"`
	Size       int        `json:"size"`
	Status     string     `json:"status,omitempty"`
	Action     string     `json:"action,omitempty"`
	RuleID     string     `json:"rule_id,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
}

type StoredMessage struct {
	Message
	Raw []byte `json:"-"`
}

type QuarantineItem struct {
	Message
	RuleVersion int64 `json:"rule_version"`
}

type Disposition struct {
	Status    string     `json:"status"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

var ErrConflict = errors.New("conflict")

type Store struct {
	db *sql.DB
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
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

CREATE TABLE IF NOT EXISTS rule_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS rules (
	id TEXT PRIMARY KEY,
	position INTEGER NOT NULL,
	recipient TEXT NOT NULL,
	sender_domain TEXT NOT NULL,
	action TEXT NOT NULL CHECK (action IN ('allow', 'quarantine'))
);
CREATE INDEX IF NOT EXISTS idx_rules_recipient ON rules(recipient, position);

CREATE TABLE IF NOT EXISTS recipient_messages (
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	recipient TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('deliverable','quarantined','released','discarded','deleted')),
	rule_id TEXT NOT NULL,
	rule_version INTEGER NOT NULL,
	action TEXT NOT NULL CHECK (action IN ('allow','quarantine')),
	decided_at TEXT,
	PRIMARY KEY(message_id, recipient)
);
CREATE INDEX IF NOT EXISTS idx_recipient_messages_recipient_status
	ON recipient_messages(recipient, status);

INSERT OR IGNORE INTO rule_meta(id, version) VALUES (1, 0);
`
	if _, err := s.db.ExecContext(ctx, statements); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO recipient_messages(message_id, recipient, status, rule_id, rule_version, action)
SELECT message_id, recipient, 'deliverable', '', 0, 'allow'
FROM message_recipients`)
	return err
}

func (s *Store) Save(ctx context.Context, sender string, recipients []string, raw []byte) (string, error) {
	rules, version, err := s.GetRules(ctx)
	if err != nil {
		return "", err
	}
	return s.SaveWithRules(ctx, sender, recipients, raw, rules, version)
}

func (s *Store) SaveWithRules(ctx context.Context, sender string, recipients []string, raw []byte, rules []Rule, ruleVersion int64) (string, error) {
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

	_, err = tx.ExecContext(ctx,
		`INSERT INTO messages(id, received_at, sender, raw) VALUES (?, ?, ?, ?)`,
		id, receivedAt, sender, raw)
	if err != nil {
		return "", err
	}
	for _, recipient := range recipients {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_recipients(message_id, recipient) VALUES (?, ?)`,
			id, recipient); err != nil {
			return "", err
		}
		matched := matchRule(rules, recipient, sender)
		ruleID := ""
		action := actionAllow
		if matched != nil {
			ruleID = matched.ID
			action = matched.Action
		}
		status := "deliverable"
		if action == actionQuarantine {
			status = "quarantined"
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recipient_messages(message_id, recipient, status, rule_id, rule_version, action)
VALUES (?, ?, ?, ?, ?, ?)`, id, recipient, status, ruleID, ruleVersion, action); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) GetRules(ctx context.Context) ([]Rule, int64, error) {
	var version int64
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM rule_meta WHERE id = 1`).Scan(&version); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, recipient, sender_domain, action FROM rules ORDER BY position`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	rules := make([]Rule, 0)
	for rows.Next() {
		var rule Rule
		if err := rows.Scan(&rule.ID, &rule.Recipient, &rule.SenderDomain, &rule.Action); err != nil {
			return nil, 0, err
		}
		rules = append(rules, rule)
	}
	return rules, version, rows.Err()
}

func (s *Store) ReplaceRules(ctx context.Context, expectedVersion int64, rules []Rule) (RuleSet, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RuleSet{}, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM rule_meta WHERE id = 1`).Scan(&current); err != nil {
		return RuleSet{}, err
	}
	if current != expectedVersion {
		return RuleSet{Version: current, Rules: []Rule{}}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rules`); err != nil {
		return RuleSet{}, err
	}
	for index, rule := range rules {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO rules(id, position, recipient, sender_domain, action) VALUES (?, ?, ?, ?, ?)`,
			rule.ID, index, rule.Recipient, rule.SenderDomain, rule.Action); err != nil {
			return RuleSet{}, err
		}
	}
	next := expectedVersion + 1
	if _, err := tx.ExecContext(ctx, `UPDATE rule_meta SET version = ? WHERE id = 1`, next); err != nil {
		return RuleSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return RuleSet{}, err
	}
	return RuleSet{Version: next, Rules: append([]Rule(nil), rules...)}, nil
}

func (s *Store) ListByRecipient(ctx context.Context, recipient string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.received_at, m.sender, length(m.raw), rm.status, rm.rule_id, rm.action, rm.decided_at
FROM messages m
JOIN recipient_messages rm ON rm.message_id = m.id
WHERE rm.recipient = ?
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
		var decidedAt sql.NullString
		if err := rows.Scan(&message.ID, &receivedAt, &message.Sender, &message.Size,
			&message.Status, &message.RuleID, &message.Action, &decidedAt); err != nil {
			return nil, err
		}
		message.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
		if err != nil {
			return nil, err
		}
		if decidedAt.Valid {
			decided, err := time.Parse(time.RFC3339Nano, decidedAt.String)
			if err != nil {
				return nil, err
			}
			message.DecidedAt = &decided
		}
		populateReason(&message)
		rowsData = append(rowsData, messageRow{message: message, receivedAt: receivedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	messages := make([]Message, 0, len(rowsData))
	for _, row := range rowsData {
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
	var decidedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT m.id, m.received_at, m.sender, `+rawColumn+`, length(m.raw), rm.status, rm.rule_id, rm.action, rm.decided_at
FROM messages m
JOIN recipient_messages rm ON rm.message_id = m.id
WHERE rm.recipient = ? AND m.id = ?`, recipient, id).
		Scan(&stored.ID, &receivedAt, &stored.Sender, &stored.Raw, &stored.Size,
			&stored.Status, &stored.RuleID, &stored.Action, &decidedAt)
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
	if decidedAt.Valid {
		decided, err := time.Parse(time.RFC3339Nano, decidedAt.String)
		if err != nil {
			return StoredMessage{}, false, err
		}
		stored.DecidedAt = &decided
	}
	populateReason(&stored.Message)
	stored.Recipients, err = s.recipientsFor(ctx, id)
	if err != nil {
		return StoredMessage{}, false, err
	}
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
JOIN recipient_messages r ON r.message_id = m.id
WHERE r.recipient = ? AND r.status IN ('deliverable', 'released')
ORDER BY m.received_at ASC, m.id ASC`, recipient)
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
			`UPDATE recipient_messages SET status = 'deleted', decided_at = ?
			 WHERE message_id = ? AND recipient = ? AND status IN ('deliverable', 'released')`,
			time.Now().UTC().Format(time.RFC3339Nano), id, recipient); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListQuarantine(ctx context.Context, recipient string) ([]QuarantineItem, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.received_at, m.sender, length(m.raw), rm.status, rm.rule_id, rm.action, rm.rule_version, rm.decided_at
FROM messages m JOIN recipient_messages rm ON rm.message_id = m.id
WHERE rm.recipient = ? AND rm.status = 'quarantined'
ORDER BY m.received_at DESC, m.id DESC`, recipient)
	if err != nil {
		return nil, err
	}
	items := make([]QuarantineItem, 0)
	for rows.Next() {
		var item QuarantineItem
		var receivedAt, decidedAt sql.NullString
		if err := rows.Scan(&item.ID, &receivedAt, &item.Sender, &item.Size,
			&item.Status, &item.RuleID, &item.Action, &item.RuleVersion, &decidedAt); err != nil {
			return nil, err
		}
		if item.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt.String); err != nil {
			return nil, err
		}
		populateReason(&item.Message)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range items {
		recipients, err := s.recipientsFor(ctx, items[index].ID)
		if err != nil {
			return nil, err
		}
		items[index].Recipients = recipients
	}
	return items, nil
}

func (s *Store) SetQuarantineDisposition(ctx context.Context, id, recipient, target string) (Disposition, bool, error) {
	if target != "released" && target != "discarded" {
		return Disposition{}, false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Disposition{}, false, err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `
SELECT status FROM recipient_messages WHERE message_id = ? AND recipient = ?`, id, recipient).Scan(&current)
	if err == sql.ErrNoRows {
		return Disposition{}, false, nil
	}
	if err != nil {
		return Disposition{}, false, err
	}
	if current != "quarantined" && current != target {
		return Disposition{}, false, ErrConflict
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if current == "quarantined" {
		if _, err := tx.ExecContext(ctx, `
UPDATE recipient_messages SET status = ?, decided_at = ?
WHERE message_id = ? AND recipient = ? AND status = 'quarantined'`, target, now, id, recipient); err != nil {
			return Disposition{}, false, err
		}
	}
	var decidedText sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT status, decided_at FROM recipient_messages WHERE message_id = ? AND recipient = ?`,
		id, recipient).Scan(&current, &decidedText); err != nil {
		return Disposition{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Disposition{}, false, err
	}
	result := Disposition{Status: current}
	if decidedText.Valid {
		decidedAt, err := time.Parse(time.RFC3339Nano, decidedText.String)
		if err != nil {
			return Disposition{}, false, err
		}
		result.DecidedAt = &decidedAt
	}
	return result, true, nil
}

func populateReason(message *Message) {
	switch message.Status {
	case "quarantined":
		message.Reason = "matched quarantine rule"
		if message.RuleID != "" {
			message.Reason += " " + message.RuleID
		}
	case "released", "discarded":
		message.Reason = "manual " + message.Status
	}
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
