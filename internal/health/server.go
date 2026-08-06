// Package health provides the HTTP server for /livez and /readyz. Readiness
// is decided from the session and listener running states held by Store.
//
//	GET /livez  Always 200. Means the process can answer HTTP.
//	GET /readyz 200 when both session and listener are running
//	            (store.Ready), otherwise 503.
//
// Both endpoints only return the fixed minimal JSON bodies {"status":"ok"}
// or {"status":"unavailable"}; no internal information is included. The not
// ready log is also just the fixed message "readiness check failed". Unknown
// paths return 404, and methods other than GET/HEAD return 405 (see the
// handler comment for method policy details).
//
// Server lifecycle is handled by New / Start / Shutdown. Start binds
// synchronously and returns an error without starting the goroutine on
// failure. Shutdown does a graceful shutdown and then joins the serve
// goroutine before returning. The serve goroutine's exit error is delivered
// once on ErrCh; nil means a normal exit.

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

// readHeaderTimeout is the http.Server deadline for reading the request
// header. Health endpoints only answer immediately, so connections that take
// long to send headers are cut quickly to avoid head-of-line blocking.
const readHeaderTimeout = 5 * time.Second

// Store holds the running states of the session and listener.
type Store struct {
	mu              sync.Mutex
	sessionRunning  bool
	listenerRunning bool
}

// NewStore returns a store in the not-ready state.
func NewStore() *Store { return &Store{} }

// SetSessionRunning updates the message session state.
func (s *Store) SetSessionRunning(running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionRunning = running
}

// SetListenerRunning updates the listener state.
func (s *Store) SetListenerRunning(running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listenerRunning = running
}

// Ready reports whether both the session and listener are running.
func (s *Store) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionRunning && s.listenerRunning
}

// Server is the HTTP server providing /livez and /readyz. New creates it,
// Start starts it, and Shutdown ends it.
type Server struct {
	addr   string
	store  *Store
	logger *slog.Logger

	// mu protects the lifecycle fields below. HTTP handlers do not use this
	// mutex.
	mu       sync.Mutex
	srv      *http.Server // valid after a successful Start
	ln       net.Listener // valid after a successful Start
	started  bool         // whether Start succeeded
	shutdown bool         // whether Shutdown has run at least once

	// errCh is a buffered channel where the serve goroutine writes its exit
	// error exactly once. nil means a normal exit via graceful shutdown;
	// buffered 1 lets the goroutine finish even without a reader.
	errCh chan error
	// doneCh is closed when the serve goroutine exits. Only Shutdown reads
	// it, so it is never double-read together with errCh.
	doneCh chan struct{}
}

// New builds a Server. addr is the host:port listen address; the default is
// config's DefaultHealthListen (127.0.0.1:8080). A nil store returns an
// error (prevents a readiness panic after startup). A nil logger discards
// logs.
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

// Start actually binds with net.Listen before starting the serve goroutine.
// A bind failure (address in use, insufficient permission, etc.) is returned
// synchronously as an error and no goroutine is started. A second Start
// returns an error.
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

// serve is the goroutine body started by Start. After Serve exits it writes
// an error to errCh exactly once (ErrServerClosed is converted to nil as a
// normal exit), then closes doneCh.
func (s *Server) serve() {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		s.errCh <- nil
	} else {
		s.errCh <- fmt.Errorf("health: serve: %w", err)
	}
	close(s.doneCh)
}

// Addr returns the bound listen address. Returns nil before Start. Useful
// for tests that specify 127.0.0.1:0 to get the actual port.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// ErrCh returns the channel that receives the serve goroutine's exit error.
// Exactly one value arrives on goroutine exit; nil means a normal exit via
// graceful shutdown. Shutdown does not consume this channel, so app's select
// is not a double read.
func (s *Server) ErrCh() <-chan error {
	return s.errCh
}

// Shutdown runs a graceful shutdown and waits for the serve goroutine to
// finish before returning. ctx must always have a deadline. If connections
// do not all close by the deadline, it force-closes to reliably end the
// goroutine and returns a deadline exceeded error. If the serve goroutine
// has already exited, it just joins. Calls before Start and double calls
// return an error. ErrCh is not consumed.
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

	// If the serve goroutine has already exited, just join.
	select {
	case <-s.doneCh:
	default:
		if err := srv.Shutdown(ctx); err != nil {
			// On deadline exceeded, force-close to reliably end the serve
			// goroutine. Serve after Close returns ErrServerClosed, so nil
			// arrives on errCh and the ErrCh contract (nil = normal exit)
			// holds.
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

// handler returns a ServeMux registering only /livez and /readyz. The method
// policy allows only GET/HEAD; ServeMux's method patterns (Go 1.22+) return
// 404 for unregistered paths and 405 for disallowed methods as standard
// plain text.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.handleLivez)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return mux
}

// handleLivez returns liveness. As long as the HTTP server can answer, the
// process is alive, so it always returns 200 regardless of readiness.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeStatus(w, http.StatusOK, true)
}

// handleReadyz returns the readiness verdict. When both session and listener
// are running (store.Ready), it returns 200 {"status":"ok"}; otherwise 503
// {"status":"unavailable"}. The log only outputs the fixed message.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.store.Ready() {
		writeStatus(w, http.StatusOK, true)
		return
	}
	s.logger.Warn("readiness check failed")
	writeStatus(w, http.StatusServiceUnavailable, false)
}

// writeStatus writes the fixed minimal JSON {"status":"ok"} or
// {"status":"unavailable"}.
func writeStatus(w http.ResponseWriter, code int, ok bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if ok {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	_, _ = io.WriteString(w, `{"status":"unavailable"}`)
}
