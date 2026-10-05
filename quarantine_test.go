package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func quarantineTestConfig(t *testing.T) Config {
	t.Helper()
	config := pop3TestConfig(t)
	config.Recipients["other@example.com"] = struct{}{}
	return config
}

func sendQuarantineMessage(t *testing.T, config Config, store *Store, sender string) string {
	t.Helper()
	_, conn, reader := dialSMTPServer(t, config, store)
	payload := "EHLO client.example\r\n" +
		"MAIL FROM:<" + sender + ">\r\n" +
		"RCPT TO:<user.Name@Example.COM>\r\n" +
		"RCPT TO:<other@example.com>\r\n" +
		"DATA\r\n" +
		"Subject: quarantine\r\n\r\nbody\r\n.\r\n"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		reply, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if len(reply) > 14 && reply[:14] == "250 queued as " {
			return reply[14 : len(reply)-2]
		}
	}
	t.Fatal("missing queued reply")
	return ""
}

func decodeJSONBody(response *http.Response, target any) error {
	defer response.Body.Close()
	return json.NewDecoder(response.Body).Decode(target)
}

func newJSONRequest(method, url string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func TestQuarantineReleaseAndRecipientIndependence(t *testing.T) {
	config := quarantineTestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	rules := []Rule{{ID: "evil", Recipient: "user.Name@example.com", SenderDomain: "evil.org", Action: "quarantine"}}
	if _, err := store.ReplaceRules(context.Background(), 0, rules); err != nil {
		t.Fatal(err)
	}
	id := sendQuarantineMessage(t, config, store, "sender@EVIL.ORG")

	quarantined, err := store.ListQuarantine(context.Background(), "user.Name@example.com")
	if err != nil || len(quarantined) != 1 || quarantined[0].ID != id || quarantined[0].RuleID != "evil" {
		t.Fatalf("quarantine=%v err=%v", quarantined, err)
	}
	if visible, err := store.ListIDsForRecipient(context.Background(), "user.Name@example.com"); err != nil || len(visible) != 0 {
		t.Fatalf("quarantined visible to POP3=%v err=%v", visible, err)
	}
	if visible, err := store.ListIDsForRecipient(context.Background(), "other@example.com"); err != nil || len(visible) != 1 {
		t.Fatalf("default allowed recipient=%v err=%v", visible, err)
	}

	httpServer := NewHTTPServer(config, store)
	testHTTP := httptest.NewServer(httpServer.server.Handler)
	defer testHTTP.Close()
	base := testHTTP.URL + "/api/recipients/user.Name@example.com/messages/" + id
	response, err := testHTTP.Client().Post(base+"/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var disposition Disposition
	if err := decodeJSONBody(response, &disposition); err != nil || response.StatusCode != 200 || disposition.Status != "released" || disposition.DecidedAt == nil {
		t.Fatalf("release status=%d disposition=%v err=%v", response.StatusCode, disposition, err)
	}
	first := *disposition.DecidedAt
	response.Body.Close()

	response, err = testHTTP.Client().Post(base+"/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeJSONBody(response, &disposition); err != nil || response.StatusCode != 200 || disposition.Status != "released" || !disposition.DecidedAt.Equal(first) {
		t.Fatalf("idempotent release status=%d disposition=%v err=%v", response.StatusCode, disposition, err)
	}
	response.Body.Close()
	response, err = testHTTP.Client().Post(base+"/discard", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatalf("reverse decision status=%d", response.StatusCode)
	}

	if visible, err := store.ListIDsForRecipient(context.Background(), "user.Name@example.com"); err != nil || len(visible) != 1 || visible[0].ID != id {
		t.Fatalf("released message=%v err=%v", visible, err)
	}
	if still, err := store.ListQuarantine(context.Background(), "user.Name@example.com"); err != nil || len(still) != 0 {
		t.Fatalf("released remains quarantined=%v err=%v", still, err)
	}
	if archived, err := store.ListByRecipient(context.Background(), "user.Name@example.com"); err != nil || len(archived) != 1 || archived[0].Status != "released" {
		t.Fatalf("archive=%v err=%v", archived, err)
	}
	if quarantined, err := store.ListQuarantine(context.Background(), "other@example.com"); err != nil || len(quarantined) != 0 {
		t.Fatalf("other recipient affected=%v err=%v", quarantined, err)
	}
}

func TestRuleReplacementVersioningAndMatching(t *testing.T) {
	if matched := matchRule([]Rule{{Recipient: "a@example.com", SenderDomain: "", Action: "quarantine"}}, "a@example.com", "mail@x.test"); matched != nil {
		t.Fatalf("empty domain matched non-empty sender: %#v", matched)
	}
	if matched := matchRule([]Rule{{Recipient: "a@example.com", SenderDomain: "", Action: "quarantine"}}, "a@example.com", ""); matched == nil || matched.Action != "quarantine" {
		t.Fatalf("empty sender did not match empty domain: %#v", matched)
	}
	if matched := matchRule([]Rule{{Recipient: "a@example.com", SenderDomain: "*", Action: "quarantine"}}, "a@example.com", ""); matched == nil {
		t.Fatal("star must match empty sender")
	}

	config := quarantineTestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	httpServer := NewHTTPServer(config, store)
	testHTTP := httptest.NewServer(httpServer.server.Handler)
	defer testHTTP.Close()

	body := bytes.NewBufferString(`{"expected_version":0,"rules":[{"id":"q","recipient":"other@Example.COM","sender_domain":"X.Test","action":"QUARANTINE"}]}`)
	request, err := newJSONRequest(http.MethodPut, testHTTP.URL+"/api/rules", body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := testHTTP.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var ruleset RuleSet
	if err := decodeJSONBody(response, &ruleset); err != nil || response.StatusCode != 200 || ruleset.Version != 1 || ruleset.Rules[0].SenderDomain != "x.test" {
		t.Fatalf("put=%#v status=%d err=%v", ruleset, response.StatusCode, err)
	}
	response.Body.Close()

	body = bytes.NewBufferString(`{"expected_version":0,"rules":[]}`)
	request, _ = newJSONRequest(http.MethodPut, testHTTP.URL+"/api/rules", body)
	response, err = testHTTP.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeJSONBody(response, &ruleset); err != nil || response.StatusCode != 409 || ruleset.Version != 1 {
		t.Fatalf("stale status=%d set=%#v err=%v", response.StatusCode, ruleset, err)
	}
	response.Body.Close()
}

func TestConcurrentQuarantineDispositionPersistsOneDecision(t *testing.T) {
	config := quarantineTestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rules := []Rule{{ID: "q", Recipient: "user.Name@example.com", SenderDomain: "*", Action: "quarantine"}}
	if _, err := store.ReplaceRules(context.Background(), 0, rules); err != nil {
		t.Fatal(err)
	}
	id := sendQuarantineMessage(t, config, store, "sender@any.test")

	results := make(chan Disposition, 24)
	var wg sync.WaitGroup
	for index := 0; index < 24; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			disposition, _, err := store.SetQuarantineDisposition(context.Background(), id, "user.Name@example.com", "released")
			if err != nil {
				t.Error(err)
				return
			}
			results <- disposition
		}()
	}
	wg.Wait()
	close(results)
	first := true
	var decided string
	for disposition := range results {
		if disposition.Status != "released" || disposition.DecidedAt == nil {
			t.Fatalf("bad disposition=%#v", disposition)
		}
		if first {
			decided = disposition.DecidedAt.String()
			first = false
		} else if disposition.DecidedAt.String() != decided {
			t.Fatalf("decision times differ: %q %q", decided, disposition.DecidedAt.String())
		}
	}
}

func TestDiscardKeepsArchiveButNotPOP3(t *testing.T) {
	config := quarantineTestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rules := []Rule{{ID: "q", Recipient: "user.Name@example.com", SenderDomain: "*", Action: "quarantine"}}
	if _, err := store.ReplaceRules(context.Background(), 0, rules); err != nil {
		t.Fatal(err)
	}
	id := sendQuarantineMessage(t, config, store, "sender@any.test")
	httpServer := NewHTTPServer(config, store)
	testHTTP := httptest.NewServer(httpServer.server.Handler)
	defer testHTTP.Close()
	url := testHTTP.URL + "/api/recipients/user.Name@example.com/messages/" + id + "/discard"
	for index, expected := range []int{http.StatusOK, http.StatusOK} {
		response, err := testHTTP.Client().Post(url, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != expected {
			t.Fatalf("discard #%d status=%d", index+1, response.StatusCode)
		}
		response.Body.Close()
	}
	release, err := testHTTP.Client().Post(testHTTP.URL+"/api/recipients/user.Name@example.com/messages/"+id+"/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	release.Body.Close()
	if release.StatusCode != http.StatusConflict {
		t.Fatalf("release after discard status=%d", release.StatusCode)
	}
	if visible, err := store.ListIDsForRecipient(context.Background(), "user.Name@example.com"); err != nil || len(visible) != 0 {
		t.Fatalf("discarded POP3 visible=%v err=%v", visible, err)
	}
	archive, err := store.ListByRecipient(context.Background(), "user.Name@example.com")
	if err != nil || len(archive) != 1 || archive[0].Status != "discarded" {
		t.Fatalf("archive=%v err=%v", archive, err)
	}
}

func TestMigratesLegacyDatabase(t *testing.T) {
	config := quarantineTestConfig(t)
	db, err := sql.Open("sqlite3", config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE messages(id TEXT PRIMARY KEY, received_at TEXT NOT NULL, sender TEXT NOT NULL, raw BLOB NOT NULL);
CREATE TABLE message_recipients(message_id TEXT NOT NULL, recipient TEXT NOT NULL, PRIMARY KEY(message_id, recipient));
INSERT INTO messages VALUES('0123456789abcdef0123456789abcdef','2026-01-01T00:00:00Z','old@example.org',X'78');
INSERT INTO message_recipients VALUES('0123456789abcdef0123456789abcdef','user.Name@example.com');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	visible, err := store.ListIDsForRecipient(context.Background(), "user.Name@example.com")
	if err != nil || len(visible) != 1 || visible[0].ID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("legacy visible=%v err=%v", visible, err)
	}
}
