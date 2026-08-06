// shutdown_helpers_test.go defines only the minimal app and wait-gate
// construction helpers shared by shutdown tests. No I/O components are
// built; each test uses real contexts, channels, and WaitGroups.
package app

import (
	"context"
	"log/slog"

	"github.com/nukanoto/gha-docker-controller/internal/config"
)

// newShutdownTestApp returns a minimal app for shutdown verification.
// runCtx/cancel and wg are real; I/O components stay nil (shutdown tolerates
// nil). The logger uses a real slog.DiscardHandler.
func newShutdownTestApp(cfg *config.Config) *app {
	a := &app{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
	a.runCtx, a.cancel = context.WithCancel(context.Background())
	return a
}

// addWaitGate registers a goroutine that holds wg until release is closed
// and returns a started channel announcing that the goroutine entered its
// wait. Real channels let tests reliably delay wg completion past the start
// of shutdown.
func addWaitGate(a *app, release chan struct{}) chan struct{} {
	started := make(chan struct{})
	a.wg.Add(1)
	go func() {
		close(started)
		<-release
		a.wg.Done()
	}()
	return started
}
