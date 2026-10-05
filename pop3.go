package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type POP3Server struct {
	addr      string
	store     *Store
	config    Config
	listener  net.Listener
	semaphore chan struct{}
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	locks     map[string]struct{}
	wg        sync.WaitGroup
}

func NewPOP3Server(config Config, store *Store) *POP3Server {
	return &POP3Server{
		addr:      config.POP3Addr,
		store:     store,
		config:    config,
		semaphore: make(chan struct{}, config.MaxConns),
		conns:     make(map[net.Conn]struct{}),
		locks:     make(map[string]struct{}),
	}
}

func (s *POP3Server) Start() error {
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
				_ = conn.SetWriteDeadline(time.Now().Add(s.config.ReadTimeout))
				fmt.Fprintf(conn, "-ERR connection limit exceeded\r\n")
				conn.Close()
			}
		}
	}()
	return nil
}

func (s *POP3Server) Shutdown(ctx context.Context) error {
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

func (s *POP3Server) addConn(conn net.Conn) {
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *POP3Server) removeConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *POP3Server) lockMailbox(recipient string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.locks[recipient]; held {
		return false
	}
	s.locks[recipient] = struct{}{}
	return true
}

func (s *POP3Server) unlockMailbox(recipient string) {
	s.mu.Lock()
	delete(s.locks, recipient)
	s.mu.Unlock()
}

type pop3Message struct {
	id      string
	size    int
	deleted bool
}

type pop3Session struct {
	user       string
	userSet    bool
	authed     bool
	lockedUser string
	messages   []pop3Message
}

func (s *POP3Server) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	session := &pop3Session{}
	defer func() {
		if session.lockedUser != "" {
			s.unlockMailbox(session.lockedUser)
		}
	}()
	if !s.reply(conn, "+OK POP3 service ready") {
		return
	}

	for {
		line, err := readStrictLine(conn, reader, s.config.ReadTimeout, s.config.MaxLineBytes)
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		if command == "" {
			if !s.reply(conn, "-ERR empty command") {
				return
			}
			continue
		}
		verbEnd := strings.IndexByte(command, ' ')
		verb := command
		argument := ""
		if verbEnd >= 0 {
			verb = command[:verbEnd]
			argument = strings.TrimSpace(command[verbEnd+1:])
		}
		verb = strings.ToUpper(verb)

		switch verb {
		case "USER":
			if session.authed {
				s.reply(conn, "-ERR already authenticated")
			} else if argument == "" {
				s.reply(conn, "-ERR USER requires an argument")
			} else {
				session.user = argument
				session.userSet = true
				s.reply(conn, "+OK user accepted")
			}
		case "PASS":
			if session.authed {
				s.reply(conn, "-ERR already authenticated")
			} else if !session.userSet {
				s.reply(conn, "-ERR send USER first")
			} else if argument == "" {
				s.reply(conn, "-ERR PASS requires an argument")
			} else {
				s.handlePass(conn, session, argument)
			}
		case "STAT":
			if !s.requireAuth(conn, session) {
				break
			}
			count := 0
			size := 0
			for _, message := range session.messages {
				if !message.deleted {
					count++
					size += message.size
				}
			}
			s.reply(conn, "+OK "+strconv.Itoa(count)+" "+strconv.Itoa(size))
		case "LIST":
			if !s.requireAuth(conn, session) {
				break
			}
			s.handleList(conn, session, argument)
		case "UIDL":
			if !s.requireAuth(conn, session) {
				break
			}
			s.handleUIDL(conn, session, argument)
		case "RETR":
			if !s.requireAuth(conn, session) {
				break
			}
			if !s.handleRetr(conn, session, argument) {
				return
			}
		case "DELE":
			if !s.requireAuth(conn, session) {
				break
			}
			index, ok := s.messageIndex(conn, session, argument)
			if !ok {
				break
			}
			session.messages[index].deleted = true
			s.reply(conn, "+OK message deleted")
		case "RSET":
			if argument != "" {
				s.reply(conn, "-ERR RSET takes no arguments")
			} else if !s.requireAuth(conn, session) {
				break
			} else {
				for index := range session.messages {
					session.messages[index].deleted = false
				}
				s.reply(conn, "+OK deletion marks cleared")
			}
		case "NOOP":
			if !s.requireAuth(conn, session) {
				break
			}
			s.reply(conn, "+OK")
		case "QUIT":
			if argument != "" {
				s.reply(conn, "-ERR QUIT takes no arguments")
			} else if !session.authed {
				s.reply(conn, "+OK goodbye")
				return
			} else {
				marked := make([]string, 0)
				for _, message := range session.messages {
					if message.deleted {
						marked = append(marked, message.id)
					}
				}
				if len(marked) > 0 {
					if err := s.store.DeleteForRecipient(context.Background(), session.lockedUser, marked); err != nil {
						s.reply(conn, "-ERR could not commit deletions")
						return
					}
				}
				s.reply(conn, "+OK goodbye")
				return
			}
		default:
			s.reply(conn, "-ERR command not recognized")
		}
	}
}

func (s *POP3Server) handlePass(conn net.Conn, session *pop3Session, password string) {
	canonical, err := CanonicalRecipient(session.user)
	if err != nil {
		s.reply(conn, "-ERR invalid user")
		return
	}
	if _, allowed := s.config.Recipients[canonical]; !allowed {
		s.reply(conn, "-ERR no such user")
		return
	}
	if password != s.config.POP3Password {
		s.reply(conn, "-ERR authentication failed")
		return
	}
	if !s.lockMailbox(canonical) {
		s.reply(conn, "-ERR mailbox already locked by another session")
		return
	}
	entries, err := s.store.ListIDsForRecipient(context.Background(), canonical)
	if err != nil {
		s.unlockMailbox(canonical)
		s.reply(conn, "-ERR storage error")
		return
	}
	session.authed = true
	session.lockedUser = canonical
	session.messages = make([]pop3Message, 0, len(entries))
	for _, entry := range entries {
		session.messages = append(session.messages, pop3Message{id: entry.ID, size: entry.Size})
	}
	s.reply(conn, "+OK mailbox locked and ready")
}

func (s *POP3Server) requireAuth(conn net.Conn, session *pop3Session) bool {
	if !session.authed {
		s.reply(conn, "-ERR authenticate first")
		return false
	}
	return true
}

func (s *POP3Server) messageIndex(conn net.Conn, session *pop3Session, argument string) (int, bool) {
	number, err := strconv.Atoi(argument)
	if err != nil || number < 1 || number > len(session.messages) {
		s.reply(conn, "-ERR no such message")
		return 0, false
	}
	index := number - 1
	if session.messages[index].deleted {
		s.reply(conn, "-ERR message already deleted")
		return 0, false
	}
	return index, true
}

func (s *POP3Server) handleList(conn net.Conn, session *pop3Session, argument string) {
	if argument != "" {
		index, ok := s.messageIndex(conn, session, argument)
		if !ok {
			return
		}
		s.reply(conn, "+OK "+strconv.Itoa(index+1)+" "+strconv.Itoa(session.messages[index].size))
		return
	}
	count := 0
	size := 0
	for _, message := range session.messages {
		if !message.deleted {
			count++
			size += message.size
		}
	}
	var builder strings.Builder
	builder.WriteString("+OK " + strconv.Itoa(count) + " messages (" + strconv.Itoa(size) + " octets)\r\n")
	for index, message := range session.messages {
		if message.deleted {
			continue
		}
		builder.WriteString(strconv.Itoa(index+1) + " " + strconv.Itoa(message.size) + "\r\n")
	}
	builder.WriteString(".")
	s.reply(conn, builder.String())
}

func (s *POP3Server) handleUIDL(conn net.Conn, session *pop3Session, argument string) {
	if argument != "" {
		index, ok := s.messageIndex(conn, session, argument)
		if !ok {
			return
		}
		s.reply(conn, "+OK "+strconv.Itoa(index+1)+" "+session.messages[index].id)
		return
	}
	var builder strings.Builder
	builder.WriteString("+OK unique-id listing follows\r\n")
	for index, message := range session.messages {
		if message.deleted {
			continue
		}
		builder.WriteString(strconv.Itoa(index+1) + " " + message.id + "\r\n")
	}
	builder.WriteString(".")
	s.reply(conn, builder.String())
}

func (s *POP3Server) handleRetr(conn net.Conn, session *pop3Session, argument string) bool {
	index, ok := s.messageIndex(conn, session, argument)
	if !ok {
		return true
	}
	stored, found, err := s.store.GetForRecipient(context.Background(), session.messages[index].id, session.lockedUser, true)
	if err != nil || !found {
		return s.reply(conn, "-ERR could not retrieve message")
	}
	var builder strings.Builder
	builder.WriteString("+OK " + strconv.Itoa(len(stored.Raw)) + " octets\r\n")
	lines := strings.SplitAfter(string(stored.Raw), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, ".") {
			builder.WriteByte('.')
		}
		builder.WriteString(line)
	}
	if !strings.HasSuffix(string(stored.Raw), "\r\n") {
		builder.WriteString("\r\n")
	}
	builder.WriteString(".")
	return s.reply(conn, builder.String())
}

func (s *POP3Server) reply(conn net.Conn, text string) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(s.config.ReadTimeout))
	if !strings.HasSuffix(text, "\r\n") {
		text += "\r\n"
	}
	_, err := conn.Write([]byte(text))
	return err == nil
}
