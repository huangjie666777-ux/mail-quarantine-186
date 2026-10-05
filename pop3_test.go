package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func pop3TestConfig(t *testing.T) Config {
	t.Helper()
	config := testConfig(t)
	config.POP3Enable = true
	config.POP3Addr = "127.0.0.1:0"
	config.POP3Password = "s3cret"
	return config
}

func dialPOP3Server(t *testing.T, config Config, store *Store) (*POP3Server, net.Conn, *bufio.Reader) {
	t.Helper()
	server := NewPOP3Server(config, store)
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
	if err != nil || !strings.HasPrefix(greeting, "+OK") {
		t.Fatalf("greeting=%q err=%v", greeting, err)
	}
	return server, conn, reader
}

func pop3Cmd(t *testing.T, conn net.Conn, reader *bufio.Reader, wantPrefix, payload string) string {
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

func pop3Login(t *testing.T, conn net.Conn, reader *bufio.Reader, user string) {
	t.Helper()
	pop3Cmd(t, conn, reader, "+OK", "USER "+user+"\r\n")
	pop3Cmd(t, conn, reader, "+OK", "PASS s3cret\r\n")
}

func seedMessages(t *testing.T, store *Store, recipient string, bodies []string) []string {
	t.Helper()
	ids := make([]string, 0, len(bodies))
	for _, body := range bodies {
		id, err := store.Save(context.Background(), "sender@example.org", []string{recipient}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond)
	}
	return ids
}

func TestPOP3Flow(t *testing.T) {
	config := pop3TestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	recipient := "user.Name@example.com"
	raw1 := "Subject: one\r\n\r\nfirst\r\n"
	raw2 := "Subject: two\r\n\r\n..dotted\r\nlast\r\n"
	ids := seedMessages(t, store, recipient, []string{raw1, raw2})

	_, conn, reader := dialPOP3Server(t, config, store)

	pop3Cmd(t, conn, reader, "-ERR", "STAT\r\n")
	pop3Cmd(t, conn, reader, "-ERR", "PASS s3cret\r\n")
	pop3Cmd(t, conn, reader, "+OK", "USER not-allowed@example.com\r\n")
	pop3Cmd(t, conn, reader, "-ERR", "PASS s3cret\r\n")
	pop3Cmd(t, conn, reader, "+OK", "USER user.Name@Example.COM\r\n")
	pop3Cmd(t, conn, reader, "-ERR", "PASS wrong\r\n")
	pop3Cmd(t, conn, reader, "+OK", "PASS s3cret\r\n")

	size1 := len(raw1)
	size2 := len(raw2)
	stat := pop3Cmd(t, conn, reader, "+OK", "STAT\r\n")
	if stat != fmt.Sprintf("+OK 2 %d\r\n", size1+size2) {
		t.Fatalf("STAT=%q", stat)
	}

	pop3Cmd(t, conn, reader, "+OK", "LIST\r\n")
	listLine1, _ := reader.ReadString('\n')
	listLine2, _ := reader.ReadString('\n')
	listEnd, _ := reader.ReadString('\n')
	if listLine1 != fmt.Sprintf("1 %d\r\n", size1) || listLine2 != fmt.Sprintf("2 %d\r\n", size2) || listEnd != ".\r\n" {
		t.Fatalf("LIST lines=%q %q %q", listLine1, listLine2, listEnd)
	}
	if single := pop3Cmd(t, conn, reader, "+OK", "LIST 2\r\n"); single != fmt.Sprintf("+OK 2 %d\r\n", size2) {
		t.Fatalf("LIST 2=%q", single)
	}
	pop3Cmd(t, conn, reader, "-ERR", "LIST 3\r\n")

	uidlLine := pop3Cmd(t, conn, reader, "+OK", "UIDL 1\r\n")
	if !strings.HasSuffix(uidlLine, " "+ids[0]+"\r\n") {
		t.Fatalf("UIDL 1=%q want id %s", uidlLine, ids[0])
	}
	pop3Cmd(t, conn, reader, "+OK", "UIDL\r\n")
	uidl1, _ := reader.ReadString('\n')
	uidl2, _ := reader.ReadString('\n')
	uidlEnd, _ := reader.ReadString('\n')
	if uidl1 != "1 "+ids[0]+"\r\n" || uidl2 != "2 "+ids[1]+"\r\n" || uidlEnd != ".\r\n" {
		t.Fatalf("UIDL lines=%q %q %q", uidl1, uidl2, uidlEnd)
	}

	retrReply := pop3Cmd(t, conn, reader, "+OK", "RETR 2\r\n")
	if retrReply != fmt.Sprintf("+OK %d octets\r\n", size2) {
		t.Fatalf("RETR reply=%q", retrReply)
	}
	var body strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ".\r\n" {
			break
		}
		body.WriteString(line)
	}
	if body.String() != "Subject: two\r\n\r\n...dotted\r\nlast\r\n" {
		t.Fatalf("RETR body=%q", body.String())
	}

	pop3Cmd(t, conn, reader, "+OK", "DELE 1\r\n")
	pop3Cmd(t, conn, reader, "-ERR", "RETR 1\r\n")
	pop3Cmd(t, conn, reader, "-ERR", "DELE 1\r\n")
	if stat := pop3Cmd(t, conn, reader, "+OK", "STAT\r\n"); stat != fmt.Sprintf("+OK 1 %d\r\n", size2) {
		t.Fatalf("STAT after DELE=%q", stat)
	}
	pop3Cmd(t, conn, reader, "+OK", "RSET extra parameters accepted\r\n")
	if stat := pop3Cmd(t, conn, reader, "+OK", "STAT\r\n"); stat != fmt.Sprintf("+OK 2 %d\r\n", size1+size2) {
		t.Fatalf("STAT after RSET=%q", stat)
	}
	pop3Cmd(t, conn, reader, "+OK", "NOOP\r\n")
	pop3Cmd(t, conn, reader, "+OK", "DELE 1\r\n")
	pop3Cmd(t, conn, reader, "+OK", "QUIT extra parameters accepted\r\n")

	remaining, err := store.ListIDsForRecipient(context.Background(), recipient)
	if err != nil || len(remaining) != 1 || remaining[0].ID != ids[1] {
		t.Fatalf("remaining=%v err=%v", remaining, err)
	}

	archive, err := store.ListByRecipient(context.Background(), recipient)
	statuses := map[string]string{}
	for _, message := range archive {
		statuses[message.ID] = message.Status
	}
	if err != nil || len(archive) != 2 || statuses[ids[0]] != "deleted" || statuses[ids[1]] != "deliverable" {
		t.Fatalf("archive=%v err=%v", archive, err)
	}
}

func TestPOP3ExclusiveLockAndSnapshot(t *testing.T) {
	config := pop3TestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	recipient := "user.Name@example.com"
	seedMessages(t, store, recipient, []string{"Subject: a\r\n\r\na\r\n"})

	server, conn1, reader1 := dialPOP3Server(t, config, store)
	pop3Login(t, conn1, reader1, recipient)

	conn2, err := net.Dial("tcp", server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	reader2 := bufio.NewReader(conn2)
	if _, err := reader2.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	pop3Cmd(t, conn2, reader2, "+OK", "USER "+recipient+"\r\n")
	pop3Cmd(t, conn2, reader2, "-ERR", "PASS s3cret\r\n")
	conn2.Close()

	if _, err := store.Save(context.Background(), "sender@example.org", []string{recipient}, []byte("Subject: b\r\n\r\nb\r\n")); err != nil {
		t.Fatal(err)
	}
	if stat := pop3Cmd(t, conn1, reader1, "+OK", "STAT\r\n"); stat != "+OK 1 17\r\n" {
		t.Fatalf("snapshot STAT=%q", stat)
	}
	pop3Cmd(t, conn1, reader1, "+OK", "QUIT\r\n")
	conn1.Close()
	time.Sleep(50 * time.Millisecond)

	_, conn3, reader3 := dialPOP3Server(t, config, store)
	pop3Login(t, conn3, reader3, recipient)
	if stat := pop3Cmd(t, conn3, reader3, "+OK", "STAT\r\n"); stat != "+OK 2 34\r\n" {
		t.Fatalf("new session STAT=%q", stat)
	}
	pop3Cmd(t, conn3, reader3, "+OK", "QUIT\r\n")
}

func TestPOP3DeletionPersistsAndDisconnectKeepsMarks(t *testing.T) {
	config := pop3TestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	recipient := "user.Name@example.com"
	ids := seedMessages(t, store, recipient, []string{"Subject: a\r\n\r\na\r\n", "Subject: b\r\n\r\nb\r\n"})
	store.Close()

	store, err = NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	_, conn, reader := dialPOP3Server(t, config, store)
	pop3Login(t, conn, reader, recipient)
	pop3Cmd(t, conn, reader, "+OK", "DELE 1\r\n")
	conn.Close()
	time.Sleep(50 * time.Millisecond)

	remaining, err := store.ListIDsForRecipient(context.Background(), recipient)
	if err != nil || len(remaining) != 2 {
		t.Fatalf("disconnect must not delete: %v err=%v", remaining, err)
	}

	_, conn2, reader2 := dialPOP3Server(t, config, store)
	pop3Login(t, conn2, reader2, recipient)
	pop3Cmd(t, conn2, reader2, "+OK", "DELE 1\r\n")
	pop3Cmd(t, conn2, reader2, "+OK", "QUIT\r\n")

	remaining, err = store.ListIDsForRecipient(context.Background(), recipient)
	if err != nil || len(remaining) != 1 || remaining[0].ID != ids[1] {
		t.Fatalf("after QUIT: %v err=%v", remaining, err)
	}
	store.Close()

	reopened, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	remaining, err = reopened.ListIDsForRecipient(context.Background(), recipient)
	if err != nil || len(remaining) != 1 || remaining[0].ID != ids[1] {
		t.Fatalf("after restart: %v err=%v", remaining, err)
	}
}

func TestPOP3StrictCRLF(t *testing.T) {
	config := pop3TestConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	_, conn, reader := dialPOP3Server(t, config, store)
	if _, err := io.WriteString(conn, "NOOP\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("connection should close after bare LF")
	}
}

func TestDuplicateRecipientAllowedWhenFull(t *testing.T) {
	config := testConfig(t)
	config.MaxRecipients = 1
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	_, conn, reader := dialSMTPServer(t, config, store)
	sendSMTP(t, conn, reader, "250", "EHLO client.example\r\n")
	reader.ReadString('\n')
	sendSMTP(t, conn, reader, "250", "MAIL FROM:<sender@example.org>\r\n")
	sendSMTP(t, conn, reader, "250", "RCPT TO:<user.Name@example.com>\r\n")
	sendSMTP(t, conn, reader, "250", "RCPT TO:<user.Name@example.com>\r\n")
	sendSMTP(t, conn, reader, "354", "DATA\r\n")
	sendSMTP(t, conn, reader, "250", "Subject: x\r\n\r\nbody\r\n.\r\n")
}

func TestHTTPEncodedRecipientQuery(t *testing.T) {
	config := testConfig(t)
	store, err := NewStore(config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.Save(context.Background(), "sender@example.org", []string{"user.Name@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	httpServer := NewHTTPServer(config, store)
	testHTTP := httptest.NewServer(httpServer.server.Handler)
	defer testHTTP.Close()
	response, err := testHTTP.Client().Get(testHTTP.URL + "/api/recipients/user.Name%40example.com/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status=%d", response.StatusCode)
	}
}
