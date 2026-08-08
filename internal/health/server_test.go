package health

// Endpoint tests use a real loopback server and standard HTTP client.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// testClient prevents a server-side failure from hanging the test.
var testClient = &http.Client{Timeout: 5 * time.Second}

// startTestServer creates a ready loopback server and joins it on cleanup.
func startTestServer(t *testing.T, sessionRunning, listenerRunning bool) *Server {
	t.Helper()
	store := NewStore()
	store.SetSessionRunning(sessionRunning)
	store.SetListenerRunning(listenerRunning)
	srv, err := New("127.0.0.1:0", store, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown returned an error: %v", err)
		}
	})
	return srv
}

// TestEndpoints covers live, ready, unknown-path, and method handling.
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
		{name: "livez returns 200 when session/listener are stopped", method: "GET", path: "/livez", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "livez returns 200 while running", session: true, listener: true, method: "GET", path: "/livez", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "readyz returns 200 when session/listener are running", session: true, listener: true, method: "GET", path: "/readyz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "readyz returns 503 when session is stopped", listener: true, method: "GET", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`},
		{name: "readyz returns 503 when listener is stopped", session: true, method: "GET", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`},
		{name: "readyz returns 503 when both are stopped", method: "GET", path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`},
		{name: "unregistered path returns 404", method: "GET", path: "/", wantStatus: http.StatusNotFound},
		{name: "POST returns 405", method: "POST", path: "/livez", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startTestServer(t, tt.session, tt.listener)
			req, err := http.NewRequest(tt.method, "http://"+srv.Addr().String()+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest returned an error: %v", err)
			}
			resp, err := testClient.Do(req)
			if err != nil {
				t.Fatalf("request returned an error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status is incorrect: got=%d want=%d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantBody == "" {
				return
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading the body returned an error: %v", err)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body is incorrect: got=%q want=%q", body, tt.wantBody)
			}
		})
	}
}
