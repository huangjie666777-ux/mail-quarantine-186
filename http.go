package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/go-chi/chi/v5"
)

type HTTPServer struct {
	server *http.Server
}

func NewHTTPServer(config Config, store *Store) *HTTPServer {
	r := chi.NewRouter()
	h := &httpHandlers{store: store, config: config}

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.URL.RawQuery) != 0 {
				http.Error(w, "query parameters are not allowed", http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	r.Get("/api/rules", h.getRules)
	r.Put("/api/rules", h.replaceRules)
	r.Get("/api/quarantine", h.listAllQuarantine)
	r.Get("/api/recipients/{recipient}/messages", h.list)
	r.Get("/api/recipients/{recipient}/messages/{id}", h.get)
	r.Get("/api/recipients/{recipient}/messages/{id}/eml", h.eml)
	r.Get("/api/recipients/{recipient}/quarantine", h.listQuarantine)
	r.Post("/api/recipients/{recipient}/messages/{id}/release", h.release)
	r.Post("/api/recipients/{recipient}/messages/{id}/discard", h.discard)

	semaphore := make(chan struct{}, config.MaxConns)
	var rejected sync.Map
	server := &http.Server{
		Addr:              config.HTTPAddr,
		Handler:           r,
		ReadTimeout:       config.ReadTimeout,
		ReadHeaderTimeout: config.ReadTimeout,
		WriteTimeout:      config.ReadTimeout,
		IdleTimeout:       config.ReadTimeout,
		MaxHeaderBytes:    config.MaxLineBytes,
	}
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if _, rejectedConn := rejected.LoadAndDelete(conn); rejectedConn {
			return
		}
		switch state {
		case http.StateNew:
			select {
			case semaphore <- struct{}{}:
			default:
				rejected.Store(conn, struct{}{})
				fmt.Fprintf(conn, "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
				_ = conn.Close()
			}
		case http.StateClosed:
			<-semaphore
		}
	}
	return &HTTPServer{server: server}
}

func (h *HTTPServer) ListenAndServe() error {
	if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (h *HTTPServer) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

type httpHandlers struct {
	store  *Store
	config Config
}

type replaceRulesRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Rules           []Rule `json:"rules"`
}

func (h *httpHandlers) getRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.store.GetRules(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (h *httpHandlers) replaceRules(w http.ResponseWriter, r *http.Request) {
	var request replaceRulesRequest
	if err := decodeJSONBody(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rules, err := h.store.ReplaceRules(r.Context(), request.ExpectedVersion, request.Rules)
	if err == ErrStaleRules {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "stale rules version"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func decodeJSONBody(r *http.Request, value any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("request body too large")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("JSON body is required")
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("invalid JSON body")
	}
	return nil
}

func (h *httpHandlers) listAllQuarantine(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListQuarantine(r.Context(), "")
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *httpHandlers) listQuarantine(w http.ResponseWriter, r *http.Request) {
	recipient, ok := h.recipient(w, r)
	if !ok {
		return
	}
	items, err := h.store.ListQuarantine(r.Context(), recipient)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *httpHandlers) release(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, StatusReleased)
}

func (h *httpHandlers) discard(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, StatusDiscarded)
}

func (h *httpHandlers) decide(w http.ResponseWriter, r *http.Request, decision string) {
	recipient, ok := h.recipient(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !validMessageID(id) {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	item, found, err := h.store.DecideQuarantine(r.Context(), id, recipient, decision)
	if err == ErrConflictDecision {
		writeJSON(w, http.StatusConflict, item)
		return
	}
	if err == ErrNotQuarantined {
		writeJSON(w, http.StatusConflict, item)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *httpHandlers) recipient(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "recipient")
	if unescaped, err := url.PathUnescape(raw); err == nil {
		raw = unescaped
	}
	canonical, err := CanonicalRecipient(raw)
	if err != nil {
		http.Error(w, "invalid recipient", http.StatusBadRequest)
		return "", false
	}
	if _, allowed := h.config.Recipients[canonical]; !allowed {
		http.Error(w, "recipient not found", http.StatusNotFound)
		return "", false
	}
	return canonical, true
}

func (h *httpHandlers) list(w http.ResponseWriter, r *http.Request) {
	recipient, ok := h.recipient(w, r)
	if !ok {
		return
	}
	messages, err := h.store.ListByRecipient(r.Context(), recipient)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

func (h *httpHandlers) get(w http.ResponseWriter, r *http.Request) {
	recipient, ok := h.recipient(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !validMessageID(id) {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	message, found, err := h.store.GetForRecipient(r.Context(), id, recipient, false)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, message.Message)
}

func (h *httpHandlers) eml(w http.ResponseWriter, r *http.Request) {
	recipient, ok := h.recipient(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !validMessageID(id) {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	message, found, err := h.store.GetForRecipient(r.Context(), id, recipient, true)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.eml"`, id))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(message.Raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(message.Raw)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
