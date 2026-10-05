package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	config, err := LoadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	store, err := NewStore(config.DBPath, config)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	smtpServer := NewSMTPServer(config, store)
	if err := smtpServer.Start(); err != nil {
		log.Fatalf("SMTP listen: %v", err)
	}
	var pop3Server *POP3Server
	if config.POP3Enable {
		pop3Server = NewPOP3Server(config, store)
		if err := pop3Server.Start(); err != nil {
			log.Fatalf("POP3 listen: %v", err)
		}
	}
	httpServer := NewHTTPServer(config, store)
	httpErr := make(chan error, 1)
	go func() { httpErr <- httpServer.ListenAndServe() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-httpErr:
		if err != nil {
			log.Fatalf("HTTP listen: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	if err := smtpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("SMTP shutdown: %v", err)
	}
	if pop3Server != nil {
		if err := pop3Server.Shutdown(shutdownCtx); err != nil {
			log.Printf("POP3 shutdown: %v", err)
		}
	}
}
