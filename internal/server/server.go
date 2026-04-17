// Package server exposes a local banya-core sidecar over HTTP + SSE so that
// remote `banya --mode remote` clients can talk to it. It does NOT modify
// banya-core; it spawns the existing sidecar binary and forwards traffic.
//
// Endpoints (mirror banya-cli/internal/client/http.go expectations):
//
//	POST /api/v1/chat      ChatRequest JSON → SSE stream of ServerEvents
//	POST /api/v1/approval  ApprovalResponse JSON → 200 OK
//	GET  /api/v1/health    200 OK on sidecar ping
//
// Auth: `Authorization: Bearer <token>` is required when the server was
// started with a non-empty token. Unauthenticated requests get 401.
//
// Concurrency: a single ProcessClient is shared across requests; events
// flowing through the sidecar are filtered per-call by session_id.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// Options configures a Server instance.
type Options struct {
	Addr        string // ":8080"
	BearerToken string // empty disables auth
}

// Server runs the HTTP+SSE adapter.
type Server struct {
	opts Options
	pc   *client.ProcessClient

	mu      sync.Mutex
	subs    map[string]chan protocol.ServerEvent // session_id → subscriber
	fanOnce sync.Once
}

// New constructs a Server bound to a ProcessClient. The ProcessClient
// must already have an LLMBackend registered if the sidecar is expected
// to call back for `llm.chat`.
func New(pc *client.ProcessClient, opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = ":8080"
	}
	return &Server{
		opts: opts,
		pc:   pc,
		subs: make(map[string]chan protocol.ServerEvent),
	}
}

// Run starts the HTTP listener. Blocks until the underlying server exits.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.handleHealth)
	mux.HandleFunc("/api/v1/chat", s.requireAuth(s.handleChat))
	mux.HandleFunc("/api/v1/approval", s.requireAuth(s.handleApproval))

	srv := &http.Server{
		Addr:    s.opts.Addr,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.BearerToken == "" {
			next(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		want := "Bearer " + s.opts.BearerToken
		if h != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if err := s.pc.HealthCheck(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	s.fanOnce.Do(s.startFanout)
	sub := s.subscribe(req.SessionID)
	defer s.unsubscribe(req.SessionID)

	if _, err := s.pc.SendMessage(req); err != nil {
		writeSSE(w, "error", protocol.ErrorData{Code: "send_failed", Message: err.Error()})
		flusher.Flush()
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-sub:
			if !ok {
				return
			}
			writeSSE(w, string(evt.Type), evt)
			flusher.Flush()
			if evt.Type == protocol.EventDone || evt.Type == protocol.EventError {
				return
			}
		}
	}
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var resp protocol.ApprovalResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.pc.SendApproval(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// startFanout reads the underlying ProcessClient event stream once and
// distributes each event to subscribers keyed by session_id. A subscriber
// with empty session_id receives every event.
func (s *Server) startFanout() {
	go func() {
		// Trigger SendMessage once with an empty probe to materialize the
		// shared event channel? No — events channel is created on first
		// start. Instead, we subscribe lazily and rely on ProcessClient
		// having opened its events channel by the time chat.start fires.
		// Pull events through a tiny goroutine that watches the client.
		for evt := range pcEvents(s.pc) {
			s.broadcast(evt)
		}
	}()
}

func (s *Server) broadcast(evt protocol.ServerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.subs[evt.SessionID]; ok {
		select {
		case ch <- evt:
		default:
		}
	}
	if ch, ok := s.subs[""]; ok && evt.SessionID != "" {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (s *Server) subscribe(sessionID string) chan protocol.ServerEvent {
	ch := make(chan protocol.ServerEvent, 64)
	s.mu.Lock()
	s.subs[sessionID] = ch
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(sessionID string) {
	s.mu.Lock()
	if ch, ok := s.subs[sessionID]; ok {
		delete(s.subs, sessionID)
		close(ch)
	}
	s.mu.Unlock()
}

// pcEvents is a tiny shim that exposes ProcessClient's event channel for fanout.
func pcEvents(pc *client.ProcessClient) <-chan protocol.ServerEvent {
	return pc.Events()
}

func writeSSE(w io.Writer, eventType string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[banya-serve] marshal sse payload: %v", err)
		return
	}
	fmt.Fprintf(w, "event: %s\n", strings.ReplaceAll(eventType, "\n", " "))
	fmt.Fprintf(w, "data: %s\n\n", body)
}
