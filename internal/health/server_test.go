package health

// Tests use only a real loopback server (127.0.0.1:0) and the standard HTTP
// client; no mocks/stubs/fakes. Endpoint behavior is grouped into one
// table-driven test.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// testClient is the HTTP client connecting to the real loopback. It has a
// timeout so server-side problems do not hang the test itself.
var testClient = &http.Client{Timeout: 5 * time.Second}

// startTestServer starts a server on the real loopback 127.0.0.1:0 with a
// concrete Store and sets the session/listener running states. Cleanup runs
// a graceful shutdown and guarantees no serve goroutine remains.
func startTestServer(t *testing.T, sessionRunning, listenerRunning bool) *Server {
	t.Helper()
	store := NewStore()
	store.SetSessionRunning(sessionRunning)
	store.SetListenerRunning(listenerRunning)
	srv, err := New("127.0.0.1:0", store, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New が error を返した: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start が error を返した: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown が error を返した: %v", err)
		}
	})
	return srv
}

// TestEndpoints verifies /livez and /readyz status/body table-driven. It
// covers the 4 session/listener running combinations plus an unregistered
// path and a disallowed method.
func TestEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		session    bool
		listener   bool
		method     string
		path       string
		wantStatus int
		wantBody   string // empty means body is not verified
	}{
		{name: "livez は session/listener 停止でも 200", method: "GET", path: "/livez", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "livez は稼働中も 200", session: true, listener: true, method: "GET", path: "/livez", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "readyz は session/listener 稼働で 200", session: true, listener: true, method: "GET", path: "/readyz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "readyz は session 停止で 503", listener: true, method: "GET", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`},
		{name: "readyz は listener 停止で 503", session: true, method: "GET", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`},
		{name: "readyz は両方停止で 503", method: "GET", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`},
		{name: "登録外 path は 404", method: "GET", path: "/", wantStatus: http.StatusNotFound},
		{name: "POST は 405", method: "POST", path: "/livez", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startTestServer(t, tt.session, tt.listener)
			req, err := http.NewRequest(tt.method, "http://"+srv.Addr().String()+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest が error を返した: %v", err)
			}
			resp, err := testClient.Do(req)
			if err != nil {
				t.Fatalf("リクエストが error を返した: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status が不正です: 実測値=%d、期待値=%d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantBody == "" {
				return
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("body の読み取りが error を返した: %v", err)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body が不正です: 実測値=%q、期待値=%q", body, tt.wantBody)
			}
		})
	}
}
