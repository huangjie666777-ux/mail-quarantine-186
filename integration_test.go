package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		SMTPAddr:      "127.0.0.1:0",
		HTTPAddr:      "127.0.0.1:0",
		DBPath:        filepath.Join(t.TempDir(), "mail.db"),
		Recipients:    map[string]struct{}{"user.Name@example.com": {}},
		MaxConns:      4,
		ReadTimeout:   2 * time.Second,
		MaxLineBytes:  200,
		MaxRecipients: 3,
		MaxMailBytes:  512,
	}
}

func dialSMTPServer(t *testing.T, config Config, store *Store) (*SMTPServer, net.Conn, *bufio.Reader) {
	t.Helper()
	server := NewSMTPServer(config, store)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	conn, err := net.Dial("tcp", server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil || greeting != "220 127.0.0.1 ESMTP service ready\r\n" {
		t.Fatalf("greeting=%q err=%v", greeting, err)
	}
	return server, conn, reader
}

func sendSMTP(t *testing.T, conn net.Conn, reader *bufio.Reader, wantPrefix, payload string) string {
	t.Helper()
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	reply, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, wantPrefix) {
		t.Fatalf("reply=%q, want prefix %q; payload=%q", reply, wantPrefix, payload)
	}
	return reply
}

func TestSMTPReceiptAndHTTPRetrieval(t *testing.T) {
	config := testConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	_, conn, reader := dialSMTPServer(t, config, store)
	if _, err := io.WriteString(conn, "EHLO client.example\r\n"); err != nil {
		t.Fatal(err)
	}
	if first, err := reader.ReadString('\n'); err != nil || first != "250-127.0.0.1\r\n" {
		t.Fatalf("first EHLO line=%q err=%v", first, err)
	}
	if second, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(second, "250 SIZE ") {
		t.Fatalf("second EHLO line=%q err=%v", second, err)
	}

	sendSMTP(t, conn, reader, "503", "RCPT TO:<user.Name@Example.COM>\r\n")
	sendSMTP(t, conn, reader, "250", "MAIL FROM:<>\r\n")
	sendSMTP(t, conn, reader, "550", "RCPT TO:<other@example.com>\r\n")
	sendSMTP(t, conn, reader, "250", "RCPT TO:<user.Name@Example.COM>\r\n")
	sendSMTP(t, conn, reader, "250", "RCPT TO:<user.Name@example.com>\r\n")
	sendSMTP(t, conn, reader, "250", "RSET\r\nEHLO client.example\r\nMAIL FROM:<sender@example.org>\r\nRCPT TO:<user.Name@Example.COM>\r\nDATA\r\n")
	for _, expected := range []string{"250", "250", "250", "250", "354"} {
		reply, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(reply, expected) {
			t.Fatalf("pipelined reply=%q err=%v", reply, err)
		}
	}

	raw := "Subject: test\r\n\r\n..dot line\r\nsecond\r\n.\r\n"
	for _, chunk := range []string{raw[:8], raw[8:20], raw[20:]} {
		if _, err := io.WriteString(conn, chunk); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	queued, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(queued, "250 queued as ") {
		t.Fatalf("queued=%q err=%v", queued, err)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(queued, "250 queued as "), "\r\n")

	httpServer := NewHTTPServer(config, store)
	testHTTP := httptest.NewServer(httpServer.server.Handler)
	defer testHTTP.Close()
	response, err := testHTTP.Client().Get(testHTTP.URL + "/api/recipients/user.Name@Example.COM/messages")
	if err != nil {
		t.Fatal(err)
	}
	var messages []Message
	if err := json.NewDecoder(response.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || len(messages) != 1 || messages[0].ID != id {
		t.Fatalf("status=%d messages=%#v", response.StatusCode, messages)
	}
	if messages[0].Sender != "sender@example.org" || len(messages[0].Recipients) != 1 || messages[0].Recipients[0] != "user.Name@example.com" {
		t.Fatalf("unexpected envelope: %#v", messages[0])
	}

	emlResponse, err := testHTTP.Client().Get(testHTTP.URL + "/api/recipients/user.Name@Example.COM/messages/" + id + "/eml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(emlResponse.Body)
	emlResponse.Body.Close()
	wanted := "Subject: test\r\n\r\n.dot line\r\nsecond\r\n"
	if emlResponse.StatusCode != 200 || string(body) != wanted {
		t.Fatalf("eml status=%d body=%q", emlResponse.StatusCode, body)
	}
}

func TestIncompleteDataIsDiscardedAndRSETClearsEnvelope(t *testing.T) {
	config := testConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server, conn, reader := dialSMTPServer(t, config, store)
	if _, err := io.WriteString(conn, "EHLO client.example\r\nMAIL FROM:<sender@example.org>\r\nRCPT TO:<user.Name@example.com>\r\nDATA\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"250-", "250 ", "250", "250", "354"} {
		reply, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(reply, expected) {
			t.Fatalf("reply=%q err=%v", reply, err)
		}
	}
	if _, err := io.WriteString(conn, "incomplete without terminator"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	messages, err := store.ListByRecipient(context.Background(), "user.Name@example.com")
	if err != nil || len(messages) != 0 {
		t.Fatalf("messages=%v err=%v", messages, err)
	}

	_, conn2, reader2 := dialSMTPServer(t, config, store)
	sendSMTP(t, conn2, reader2, "250", "EHLO client.example\r\nRSET extra parameters accepted\r\n")
	ehloSecond, err := reader2.ReadString('\n')
	if err != nil || !strings.HasPrefix(ehloSecond, "250 SIZE ") {
		t.Fatalf("EHLO second=%q err=%v", ehloSecond, err)
	}
	rset, err := reader2.ReadString('\n')
	if err != nil || rset != "250 OK\r\n" {
		t.Fatalf("RSET=%q err=%v", rset, err)
	}
	sendSMTP(t, conn2, reader2, "250", "MAIL FROM:<sender@example.org>\r\n")
	sendSMTP(t, conn2, reader2, "503", "DATA\r\n")
}

func TestStrictCRLFAndInvalidHTTPQueries(t *testing.T) {
	config := testConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	_, conn, reader := dialSMTPServer(t, config, store)
	if _, err := io.WriteString(conn, "EHLO client.example\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("connection should close after bare LF")
	}

	httpServer := NewHTTPServer(config, store)
	testHTTP := httptest.NewServer(httpServer.server.Handler)
	defer testHTTP.Close()
	response, err := testHTTP.Client().Get(testHTTP.URL + "/api/recipients/user.Name@example.com/messages?bad=1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	invalidID, err := testHTTP.Client().Get(testHTTP.URL + "/api/recipients/user.Name@example.com/messages/not-hex/eml")
	if err != nil {
		t.Fatal(err)
	}
	invalidID.Body.Close()
	if invalidID.StatusCode != 400 {
		t.Fatalf("invalid ID status=%d", invalidID.StatusCode)
	}
}

func TestRecipientAddressRules(t *testing.T) {
	canonical, err := CanonicalRecipient("User.Name@Example.COM")
	if err != nil || canonical != "User.Name@example.com" {
		t.Fatalf("canonical=%q err=%v", canonical, err)
	}
	for _, invalid := range []string{".a@example.com", "a.@example.com", "a..b@example.com", "a@example..com", "a@-example.com", "a@"} {
		if _, err := CanonicalRecipient(invalid); err == nil {
			t.Fatalf("expected %q invalid", invalid)
		}
	}
}

func TestSMTPOptionalMailParameters(t *testing.T) {
	sender, err := parseReversePath("FROM:<sender@example.org> BODY=8BITMIME SIZE=123")
	if err != nil || sender != "sender@example.org" {
		t.Fatalf("sender=%q err=%v", sender, err)
	}
	empty, err := parseReversePath("FROM:<>")
	if err != nil || empty != "" {
		t.Fatalf("empty sender=%q err=%v", empty, err)
	}
}
