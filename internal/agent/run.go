package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// Run serves the agent API and puts the computer to sleep when idle, until ctx ends.
func Run(ctx context.Context, cfg *Config, version string, log *slog.Logger) error {
	token, err := ReadToken(cfg.TokenFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		log.Warn("state directory unavailable: a changed idle timeout will not be saved", "dir", cfg.StateDir, "err", err)
	}
	a := New(cfg, Linux{}, log, version)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.Handler(token), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go a.Run(ctx)
	log.Info("agent started", "version", version, "listen", cfg.Listen, "idle_timeout", a.IdleTimeout(),
		"sleep", cfg.Sleep, "sessions", cfg.KeepAwake.Sessions, "inhibitors", *cfg.KeepAwake.Inhibitors,
		"commands", len(cfg.KeepAwake.Commands))
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(stopCtx)
	return err
}
