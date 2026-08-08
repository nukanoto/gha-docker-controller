// Shutdown tests use real contexts, channels, and WaitGroups.
package app

import (
	"context"
	"log/slog"

	"github.com/nukanoto/arc-docker/internal/config"
)

// newShutdownTestApp creates an app with only shutdown state.
func newShutdownTestApp(cfg *config.Config) *app {
	a := &app{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
	a.runCtx, a.cancel = context.WithCancel(context.Background())
	return a
}

// addWaitGate delays a real WaitGroup completion deterministically.
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
