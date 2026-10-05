package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newTestHTTP(t *testing.T, config Config, store *Store) *httptest.Server {
	t.Helper()
	httpServer := NewHTTPServer(config, store)
	server := httptest.NewServer(httpServer.server.Handler)
	t.Cleanup(server.Close)
	return server
}

func putJSON(t *testing.T, server *httptest.Server, path string, body any) (int, RuleSet) {
	t.Helper()
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPut, server.URL+path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var rules RuleSet
	if err := json.Unmarshal(responseBody, &rules); err != nil {
		t.Fatalf("status=%d body=%q: %v", response.StatusCode, responseBody, err)
	}
	return response.StatusCode, rules
}

func TestRuleEvaluationOrderAndDomains(t *testing.T) {
	rules := RuleSet{Version: 7, Rules: []Rule{
		{ID: "empty-only", Recipient: "qa@example.com", SenderDomain: "", Action: ActionQuarantine},
		{ID: "wildcard", Recipient: "qa@example.com", SenderDomain: "*", Action: ActionAllow},
		{ID: "specific", Recipient: "other@example.com", SenderDomain: "Bad.Example", Action: ActionQuarantine},
	}}
	if match := rules.Match("qa@example.com", ""); match.RuleID != "empty-only" || match.Action != ActionQuarantine {
		t.Fatalf("empty sender match=%#v", match)
	}
	if match := rules.Match("qa@example.com", "x@bad.example"); match.RuleID != "wildcard" {
		t.Fatalf("wildcard match=%#v", match)
	}
	if match := rules.Match("other@example.com", "x@BAD.example"); match.RuleID != "specific" {
		t.Fatalf("case-insensitive match=%#v", match)
	}
	if match := rules.Match("other@example.com", "x@good.example"); match.RuleID != "" || match.Action != ActionAllow {
		t.Fatalf("default match=%#v", match)
	}
}

func TestQuarantineReleaseDiscardAndRulesVersion(t *testing.T) {
	config := testConfig(t)
	store, err := NewStore(config.DBPath, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	httpServer := newTestHTTP(t, config, store)

	rules := []Rule{
		{ID: "qa-bad-domain", Recipient: "qa@example.com", SenderDomain: "bad.org", Action: ActionQuarantine},
		{ID: "qa-empty", Recipient: "qa@example.com", SenderDomain: "", Action: ActionQuarantine},
		{ID: "qa-allow", Recipient: "qa@example.com", SenderDomain: "example.net", Action: ActionAllow},
		{ID: "user-empty", Recipient: "user.Name@example.com", SenderDomain: "", Action: ActionQuarantine},
		{ID: "user-allow-all", Recipient: "user.Name@example.com", SenderDomain: "*", Action: ActionAllow},
	}
	status, saved := putJSON(t, httpServer, "/api/rules", map[string]any{"expected_version": 0, "rules": rules})
	if status != http.StatusOK || saved.Version != 1 || len(saved.Rules) != len(rules) {
		t.Fatalf("replace status=%d rules=%#v", status, saved)
	}
	if status, _ = putJSON(t, httpServer, "/api/rules", map[string]any{"expected_version": 0, "rules": rules}); status != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d", status)
	}

	id := sendQuarantineTestMail(t, config, store, "sender@bad.org", []string{"qa@Example.COM", "user.Name@example.com"})
	emptyID := sendQuarantineTestMail(t, config, store, "", []string{"qa@example.com"})

	qaMessages, err := store.ListIDsForRecipient(context.Background(), "qa@example.com")
	if err != nil || len(qaMessages) != 0 {
		t.Fatalf("quarantine must not be in POP3: %v err=%v", qaMessages, err)
	}
	userMessages, err := store.ListIDsForRecipient(context.Background(), "user.Name@example.com")
	if err != nil || len(userMessages) != 1 || userMessages[0].ID != id {
		t.Fatalf("delivered message=%v err=%v", userMessages, err)
	}

	response, err := httpServer.Client().Get(httpServer.URL + "/api/recipients/qa@example.com/quarantine")
	if err != nil {
		t.Fatal(err)
	}
	var items []QuarantineItem
	if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || len(items) != 2 {
		t.Fatalf("items status=%d len=%d", response.StatusCode, len(items))
	}
	if items[1].ID != id || items[1].RuleID != "qa-bad-domain" || items[1].RulesVersion != 1 || !strings.Contains(items[1].Reason, "qa-bad-domain") {
		t.Fatalf("unexpected item=%#v", items[1])
	}

	released := postDecision(t, httpServer, id, "qa@example.com", "release", http.StatusOK)
	repeated := postDecision(t, httpServer, id, "qa@example.com", "release", http.StatusOK)
	if repeated.DecidedAt == nil || released.DecidedAt == nil || !repeated.DecidedAt.Equal(*released.DecidedAt) {
		t.Fatalf("idempotent times=%v %v", released.DecidedAt, repeated.DecidedAt)
	}
	postDecision(t, httpServer, id, "qa@example.com", "discard", http.StatusConflict)
	qaMessages, _ = store.ListIDsForRecipient(context.Background(), "qa@example.com")
	if len(qaMessages) != 1 || qaMessages[0].ID != id {
		t.Fatalf("released mail must be visible next POP3 session: %v", qaMessages)
	}

	discarded := postDecision(t, httpServer, emptyID, "qa@example.com", "discard", http.StatusOK)
	if discarded.DecidedAt == nil {
		t.Fatal("discarded time missing")
	}
	postDecision(t, httpServer, emptyID, "qa@example.com", "release", http.StatusConflict)
	qaMessages, _ = store.ListIDsForRecipient(context.Background(), "qa@example.com")
	if len(qaMessages) != 1 {
		t.Fatalf("discarded mail must remain unavailable: %v", qaMessages)
	}
	archive, found, err := store.GetForRecipient(context.Background(), emptyID, "qa@example.com", false)
	if err != nil || !found || archive.Status != StatusDiscarded {
		t.Fatalf("discarded archive found=%v err=%v status=%s", found, err, archive.Status)
	}

	status, _ = putJSON(t, httpServer, "/api/rules", map[string]any{
		"expected_version": 1,
		"rules":            []Rule{{ID: "discard-new", Recipient: "qa@example.com", SenderDomain: "*", Action: ActionQuarantine}},
	})
	if status != http.StatusOK {
		t.Fatalf("second rule version status=%d", status)
	}
	qaMessages, _ = store.ListIDsForRecipient(context.Background(), "qa@example.com")
	if len(qaMessages) != 1 {
		t.Fatalf("rule update must not retroactively reclassify: %v", qaMessages)
	}
}

func TestConcurrentReleasePersistsOneDecision(t *testing.T) {
	config := testConfig(t)
	store, err := NewStore(config.DBPath, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	_, err = store.ReplaceRules(context.Background(), 0, []Rule{{ID: "q", Recipient: "qa@example.com", SenderDomain: "*", Action: ActionQuarantine}})
	if err != nil {
		t.Fatal(err)
	}
	id := sendQuarantineTestMail(t, config, store, "sender@example.org", []string{"qa@example.com"})
	httpServer := newTestHTTP(t, config, store)
	var wg sync.WaitGroup
	statuses := make(chan int, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/recipients/qa@example.com/messages/%s/release", httpServer.URL, id), nil)
			response, err := httpServer.Client().Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			response.Body.Close()
			statuses <- response.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}
	item, found, err := store.GetQuarantineDecision(context.Background(), id, "qa@example.com")
	if err != nil || !found || item.Status != StatusReleased || item.DecidedAt == nil {
		t.Fatalf("item=%#v found=%v err=%v", item, found, err)
	}
}

func sendQuarantineTestMail(t *testing.T, config Config, store *Store, sender string, recipients []string) string {
	t.Helper()
	_, conn, reader := dialSMTPServer(t, config, store)
	defer conn.Close()
	readLine := func(want string) {
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, want) {
			t.Fatalf("line=%q want=%s err=%v", line, want, err)
		}
	}
	fmt.Fprint(conn, "EHLO client.example\r\n")
	readLine("250")
	readLine("250")
	if sender == "" {
		fmt.Fprint(conn, "MAIL FROM:<>\r\n")
	} else {
		fmt.Fprintf(conn, "MAIL FROM:<%s>\r\n", sender)
	}
	readLine("250")
	for _, recipient := range recipients {
		fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", recipient)
		readLine("250")
	}
	fmt.Fprint(conn, "DATA\r\nSubject: quarantine test\r\n\r\nbody\r\n.\r\n")
	readLine("354")
	queued, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(queued, "250 queued as ") {
		t.Fatalf("queued=%q err=%v", queued, err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(queued, "250 queued as "), "\r\n")
}

func postDecision(t *testing.T, server *httptest.Server, id, recipient, action string, wantStatus int) QuarantineItem {
	t.Helper()
	request, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/recipients/%s/messages/%s/%s", server.URL, recipient, id, action), nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var item QuarantineItem
	body, _ := io.ReadAll(response.Body)
	if err := json.Unmarshal(body, &item); err != nil {
		t.Fatalf("body=%s: %v", body, err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("status=%d want=%d item=%#v body=%s", response.StatusCode, wantStatus, item, body)
	}
	return item
}
