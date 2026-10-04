package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	SMTPAddr      string
	HTTPAddr      string
	DBPath        string
	Recipients    map[string]struct{}
	POP3Enable    bool
	POP3Addr      string
	POP3Password  string
	MaxConns      int
	ReadTimeout   time.Duration
	MaxLineBytes  int
	MaxRecipients int
	MaxMailBytes  int64
}

func LoadConfig() (Config, error) {
	smtpAddr := getenv("SMTP_ADDR", "127.0.0.1:2525")
	httpAddr := getenv("HTTP_ADDR", "127.0.0.1:8080")
	if err := validateLoopbackAddr(smtpAddr); err != nil {
		return Config{}, fmt.Errorf("SMTP_ADDR: %w", err)
	}
	if err := validateLoopbackAddr(httpAddr); err != nil {
		return Config{}, fmt.Errorf("HTTP_ADDR: %w", err)
	}

	allowedText := os.Getenv("ALLOWED_RECIPIENTS")
	recipientList := splitAddresses(allowedText)
	if len(recipientList) == 0 {
		return Config{}, fmt.Errorf("ALLOWED_RECIPIENTS must contain at least one address")
	}
	recipients := make(map[string]struct{}, len(recipientList))
	for _, recipient := range recipientList {
		canonical, err := CanonicalRecipient(recipient)
		if err != nil {
			return Config{}, fmt.Errorf("allowed recipient %q: %w", recipient, err)
		}
		recipients[canonical] = struct{}{}
	}

	maxConns, err := getenvInt("MAX_CONNECTIONS", 100)
	if err != nil {
		return Config{}, err
	}
	readTimeoutSeconds, err := getenvInt("READ_TIMEOUT_SECONDS", 60)
	if err != nil {
		return Config{}, err
	}
	maxLineBytes, err := getenvInt("MAX_LINE_BYTES", 4096)
	if err != nil {
		return Config{}, err
	}
	maxRecipients, err := getenvInt("MAX_RECIPIENTS", 100)
	if err != nil {
		return Config{}, err
	}
	maxMailBytes, err := getenvInt64("MAX_MAIL_BYTES", 10*1024*1024)
	if err != nil {
		return Config{}, err
	}
	if maxConns < 1 || readTimeoutSeconds < 1 || maxLineBytes < 3 || maxRecipients < 1 || maxMailBytes < 1 {
		return Config{}, fmt.Errorf("limits must be positive")
	}

	pop3Enable := false
	if value := os.Getenv("POP3_ENABLE"); value != "" {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			pop3Enable = true
		case "0", "false", "no", "off":
		default:
			return Config{}, fmt.Errorf("POP3_ENABLE: invalid boolean %q", value)
		}
	}
	pop3Addr := getenv("POP3_ADDR", "127.0.0.1:1100")
	pop3Password := os.Getenv("POP3_PASSWORD")
	if pop3Enable {
		if err := validateLoopbackAddr(pop3Addr); err != nil {
			return Config{}, fmt.Errorf("POP3_ADDR: %w", err)
		}
		if pop3Password == "" {
			return Config{}, fmt.Errorf("POP3_PASSWORD must be set when POP3_ENABLE is true")
		}
	}

	return Config{
		SMTPAddr:      smtpAddr,
		POP3Enable:    pop3Enable,
		POP3Addr:      pop3Addr,
		POP3Password:  pop3Password,
		HTTPAddr:      httpAddr,
		DBPath:        getenv("DB_PATH", "mail.db"),
		Recipients:    recipients,
		MaxConns:      maxConns,
		ReadTimeout:   time.Duration(readTimeoutSeconds) * time.Second,
		MaxLineBytes:  maxLineBytes,
		MaxRecipients: maxRecipients,
		MaxMailBytes:  maxMailBytes,
	}, nil
}

func validateLoopbackAddr(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback()) {
		return fmt.Errorf("must listen on 127.0.0.1 or ::1")
	}
	if port == "" {
		return fmt.Errorf("empty port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("invalid port")
	}
	return nil
}

func splitAddresses(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return number, nil
}

func getenvInt64(key string, fallback int64) (int64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return number, nil
}
