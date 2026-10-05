package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

type SMTPServer struct {
	addr      string
	store     *Store
	config    Config
	listener  net.Listener
	semaphore chan struct{}
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

func NewSMTPServer(config Config, store *Store) *SMTPServer {
	return &SMTPServer{
		addr:      config.SMTPAddr,
		store:     store,
		config:    config,
		semaphore: make(chan struct{}, config.MaxConns),
		conns:     make(map[net.Conn]struct{}),
	}
}

func (s *SMTPServer) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = listener
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			select {
			case s.semaphore <- struct{}{}:
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					defer func() { <-s.semaphore }()
					s.addConn(conn)
					s.handle(conn)
					s.removeConn(conn)
				}()
			default:
				fmt.Fprintf(conn, "421 127.0.0.1 connection limit exceeded\r\n")
				conn.Close()
			}
		}
	}()
	return nil
}

func (s *SMTPServer) Shutdown(ctx context.Context) error {
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			return err
		}
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (s *SMTPServer) addConn(conn net.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *SMTPServer) removeConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

type smtpSession struct {
	greeted    bool
	sender     string
	senderSet  bool
	recipients []string
}

func (s *SMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	session := &smtpSession{}
	if _, err := fmt.Fprintf(conn, "220 127.0.0.1 ESMTP service ready\r\n"); err != nil {
		return
	}

	for {
		line, err := readStrictLine(conn, reader, s.config.ReadTimeout, s.config.MaxLineBytes)
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		if command == "" {
			if writeReply(conn, s.config.ReadTimeout, "500 empty command") {
				return
			}
			continue
		}
		verbEnd := strings.IndexByte(command, ' ')
		verb := command
		arguments := ""
		if verbEnd >= 0 {
			verb = command[:verbEnd]
			arguments = strings.TrimSpace(command[verbEnd+1:])
		}
		verb = strings.ToUpper(verb)

		var reply string
		closeAfter := false
		var fatal bool
		switch verb {
		case "HELO", "EHLO":
			if arguments == "" || strings.ContainsAny(arguments, " \t") {
				reply = "501 invalid greeting domain"
			} else {
				session.resetEnvelope()
				session.greeted = true
				if verb == "EHLO" {
					reply = "250-127.0.0.1\r\n250 SIZE " + fmt.Sprintf("%d", s.config.MaxMailBytes)
				} else {
					reply = "250 127.0.0.1"
				}
			}
		case "MAIL":
			{
				if !session.greeted {
					reply = "503 send HELO first"
					break
				}
				sender, err := parseReversePath(arguments)
				if err != nil {
					reply = "501 invalid sender"
				} else {
					session.resetEnvelope()
					session.greeted = true
					session.sender = sender
					session.senderSet = true
					reply = "250 sender OK"
				}
			}
		case "RCPT":
			{
				if !session.greeted {
					reply = "503 send HELO first"
				} else if !session.senderSet {
					reply = "503 send MAIL first"
				} else {
					address, err := parseForwardPath(arguments)
					if err != nil {
						reply = "501 invalid recipient"
						break
					}
					canonical, err := CanonicalRecipient(address)
					if err != nil {
						reply = "501 invalid recipient"
					} else if _, allowed := s.config.Recipients[canonical]; !allowed {
						reply = "550 recipient not allowed"
					} else if containsString(session.recipients, canonical) {
						reply = "250 duplicate recipient ignored"
					} else if len(session.recipients) >= s.config.MaxRecipients {
						reply = "452 too many recipients"
						fatal = true
					} else {
						session.recipients = append(session.recipients, canonical)
						reply = "250 recipient OK"
					}
				}
			}
		case "DATA":
			if arguments != "" {
				reply = "501 DATA takes no arguments"
			} else if !session.greeted {
				reply = "503 send HELO first"
			} else if !session.senderSet || len(session.recipients) == 0 {
				reply = "503 complete envelope first"
			} else {
				if writeReply(conn, s.config.ReadTimeout, "354 send mail content; end with .") {
					return
				}
				raw, err := readData(conn, reader, s.config)
				if err != nil {
					return
				}
				id, err := s.store.SaveWithRules(context.Background(), session.sender, append([]string(nil), session.recipients...), raw)
				if err != nil {
					writeReply(conn, s.config.ReadTimeout, "451 could not store message")
					return
				}
				reply = "250 queued as " + id
				session.resetEnvelope()
				session.greeted = true
			}
		case "RSET":
			if arguments != "" {
				reply = "501 RSET takes no arguments"
			} else {
				session.resetEnvelope()
				reply = "250 OK"
			}
		case "NOOP":
			if arguments != "" {
				reply = "501 NOOP takes no arguments"
			} else {
				reply = "250 OK"
			}
		case "QUIT":
			if arguments != "" {
				reply = "501 QUIT takes no arguments"
			} else {
				reply = "221 bye"
				closeAfter = true
			}
		default:
			reply = "500 command not recognized"
		}

		if writeReply(conn, s.config.ReadTimeout, reply) {
			return
		}
		if fatal || closeAfter {
			return
		}
	}
}

func (s *smtpSession) resetEnvelope() {
	s.sender = ""
	s.senderSet = false
	s.recipients = nil
}

func readStrictLine(conn net.Conn, reader *bufio.Reader, timeout time.Duration, maxBytes int) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var builder strings.Builder
	for {
		chunk, err := reader.ReadSlice('\n')
		builder.Write(chunk)
		if builder.Len() > maxBytes {
			return "", errLineTooLong
		}
		if err == nil {
			line := builder.String()
			if len(line) < 2 || line[len(line)-2] != '\r' {
				return "", errBadLineEnding
			}
			return line[:len(line)-2], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
}

func readData(conn net.Conn, reader *bufio.Reader, config Config) ([]byte, error) {
	var raw []byte
	for {
		line, err := readStrictLine(conn, reader, config.ReadTimeout, config.MaxLineBytes)
		if err != nil {
			return nil, err
		}
		if line == "." {
			return raw, nil
		}
		if strings.HasPrefix(line, ".") {
			line = line[1:]
		}
		addition := len(line) + 2
		if int64(len(raw)+addition) > config.MaxMailBytes {
			writeReply(conn, config.ReadTimeout, "552 message too large")
			return nil, errMessageTooLarge
		}
		raw = append(raw, line...)
		raw = append(raw, '\r', '\n')
	}
}

func parseReversePath(arguments string) (string, error) {
	if strings.EqualFold(arguments, "FROM:<>") {
		return "", nil
	}
	address, err := parsePathCommand(arguments, "FROM:")
	if err != nil {
		return "", err
	}
	if _, err := CanonicalRecipient(address); err != nil {
		return "", err
	}
	return address, nil
}

func parseForwardPath(arguments string) (string, error) {
	return parsePathCommand(arguments, "TO:")
}

func parsePathCommand(arguments, prefix string) (string, error) {
	if len(arguments) < len(prefix)+2 || !strings.EqualFold(arguments[:len(prefix)], prefix) {
		return "", errInvalidAddress
	}
	value := strings.TrimSpace(arguments[len(prefix):])
	if !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, ">") {
		closing := strings.IndexByte(value, '>')
		if closing < 1 {
			return "", errInvalidAddress
		}
		rest := value[closing+1:]
		if strings.TrimSpace(rest) != "" && rest[0] != ' ' {
			return "", errInvalidAddress
		}
		value = value[:closing+1]
		if !strings.HasSuffix(value, ">") {
			return "", errInvalidAddress
		}
	}
	address := value[1 : len(value)-1]
	if address == "" || strings.ContainsAny(address, "<> \t") {
		return "", errInvalidAddress
	}
	return address, nil
}

func writeReply(conn net.Conn, timeout time.Duration, reply string) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if !strings.HasSuffix(reply, "\r\n") {
		reply += "\r\n"
	}
	_, err := io.WriteString(conn, reply)
	return err != nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

var (
	errLineTooLong     = errors.New("line too long")
	errBadLineEnding   = errors.New("line must use CRLF")
	errMessageTooLarge = errors.New("message too large")
)
