// Package health provides the /livez and /readyz HTTP endpoints.

package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// readHeaderTimeout limits slow request headers on the small health server.
const readHeaderTimeout = 5 * time.Second

// Store holds session and listener readiness state.
type Store struct {
	mu              sync.Mutex
	sessionRunning  bool
	listenerRunning bool
}

// NewStore returns a not-ready store.
func NewStore() *Store { return &Store{} }

// SetSessionRunning updates session readiness.
func (s *Store) SetSessionRunning(running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionRunning = running
}

// SetListenerRunning updates listener readiness.
func (s *Store) SetListenerRunning(running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listenerRunning = running
}

// Ready reports whether both dependencies are running.
func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionRunning && s.listenerRunning
}

// Server provides /livez and /readyz.
type Server struct {
	addr   string
	store  *Store
	logger *slog.Logger

	// HTTP handlers do not access these lifecycle fields.
	mu       sync.Mutex
	srv      *http.Server // valid after a successful Start
	ln       net.Listener // valid after a successful Start
	started  bool         // whether Start succeeded
	shutdown bool         // whether Shutdown has run at least once

	// The buffer lets serve finish even when no reader remains.
	errCh chan error
	// doneCh is the join signal for Shutdown.
	doneCh chan struct{}
}

// New builds a health server. A nil store is rejected because readiness needs
// it after startup.
func New(addr string, store *Store, logger *slog.Logger) (*Server, error) {
	if store == nil {
		return nil, errors.New("health: nil store")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		addr:   addr,
		store:  store,
		logger: logger,
		errCh:  make(chan error, 1),
		doneCh: make(chan struct{}),
	}, nil
}

// Start binds the address synchronously, then starts serving.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("health: server already started")
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("health: listen on %s: %w", s.addr, err)
	}
	s.srv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	s.ln = ln
	s.started = true
	s.mu.Unlock()

	go s.serve()
	s.logger.Info("health server listening", "addr", ln.Addr().String())
	return nil
}

// serve sends one exit result and closes doneCh.
func (s *Server) serve() {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		s.errCh <- nil
	} else {
		s.errCh <- fmt.Errorf("health: serve: %w", err)
	}
	close(s.doneCh)
}

// Addr returns the bound address, or nil before Start.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// ErrCh returns the single serve-goroutine result channel.
func (s *Server) ErrCh() <-chan error {
	return s.errCh
}

// Shutdown gracefully stops the server and joins its goroutine.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return errors.New("health: shutdown before start")
	}
	if s.shutdown {
		s.mu.Unlock()
		return errors.New("health: server already shut down")
	}
	s.shutdown = true
	srv := s.srv
	s.mu.Unlock()

	select {
	case <-s.doneCh:
	default:
		if err := srv.Shutdown(ctx); err != nil {
			// Force-close so Shutdown never leaves the serve goroutine behind.
			_ = srv.Close()
			<-s.doneCh
			s.logger.Info("health server stopped")
			return fmt.Errorf("health: graceful shutdown: %w", err)
		}
		<-s.doneCh
	}
	s.logger.Info("health server stopped")
	return nil
}

// handler registers only the two health endpoints.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.handleLivez)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return mux
}

// handleLivez reports that the HTTP server can answer.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeStatus(w, http.StatusOK, true)
}

// handleReadyz reports whether the session and listener are running.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.store.Ready() {
		writeStatus(w, http.StatusOK, true)
		return
	}
	s.logger.Warn("readiness check failed")
	writeStatus(w, http.StatusServiceUnavailable, false)
}

func writeStatus(w http.ResponseWriter, code int, ok bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if ok {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	_, _ = io.WriteString(w, `{"status":"unavailable"}`)
}
