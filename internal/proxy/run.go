package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Run listens on every service port, and on the admin port if there is one, until ctx
// ends.
func Run(ctx context.Context, cfg *Config, version string, log *slog.Logger) error {
	activity := &Activity{}
	var agent *AgentClient
	var prober Prober = TCPProber{Addr: cfg.Target.Probe}
	if cfg.Target.Agent != "" {
		token, err := cfg.ReadAgentToken()
		if err != nil {
			return err
		}
		agent = &AgentClient{URL: cfg.Target.Agent, Token: token, HTTP: &http.Client{}}
		prober = &AgentProber{Agent: agent, Activity: activity, Log: log}
	}
	boot := cfg.Target.BootTimeout
	wait := boot + cfg.Target.ShutdownTimeout
	mon := &Monitor{Prober: prober, Wake: Waker(cfg.Target), BootWindow: boot, Log: log, Hold: activity.Begin}
	stats := NewStats(cfg.Services, 50)
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		log.Warn("state directory unavailable: the awake window will not be saved", "dir", cfg.StateDir, "err", err)
	}
	sched := &Scheduler{Mon: mon, Agent: agent, Store: filepath.Join(cfg.StateDir, "proxy.json"), BootTimeout: wait, Log: log}
	if err := sched.Load(); err != nil {
		log.Warn("saved awake window unreadable; disabled", "err", err)
	}

	type server struct {
		name  string
		ln    net.Listener
		serve func(net.Listener) error
		stop  func(context.Context) error
	}
	var servers []server
	closeAll := func() {
		for _, s := range servers {
			s.ln.Close()
		}
	}
	listen := func(name, addr string) (net.Listener, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return ln, nil
	}
	httpServer := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	}
	for _, svc := range cfg.Services {
		ln, err := listen(svc.Name, svc.Listen)
		if err != nil {
			return err
		}
		if svc.TCP {
			p := &TCPProxy{Svc: svc, Mon: mon, Activity: activity, Stats: stats, BootTimeout: boot, WaitTimeout: wait, Log: log}
			servers = append(servers, server{svc.Name, ln, func(ln net.Listener) error { return p.Serve(ctx, ln) },
				func(context.Context) error { return ln.Close() }})
			continue
		}
		p := NewHTTPProxy(&HTTPProxy{Svc: svc, Target: cfg.Target.Name, Mon: mon, Activity: activity, Stats: stats,
			BootTimeout: boot, WaitTimeout: wait, MaxBody: int64(cfg.MaxBodyMB) << 20, Log: log})
		srv := httpServer(p)
		servers = append(servers, server{svc.Name, ln, srv.Serve, srv.Shutdown})
	}
	if cfg.Admin.Listen != "" {
		ln, err := listen("admin", cfg.Admin.Listen)
		if err != nil {
			return err
		}
		admin := &Admin{Cfg: cfg, Mon: mon, Agent: agent, Activity: activity, Stats: stats, Schedule: sched,
			WaitTimeout: wait, Version: version, Log: log}
		srv := httpServer(admin.Handler())
		servers = append(servers, server{"admin", ln, srv.Serve, srv.Shutdown})
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", s.name, err)
			}
		}()
	}
	go mon.Run(ctx)
	go sched.Run(ctx)
	names := make([]string, len(cfg.Services))
	for i, svc := range cfg.Services {
		names[i] = svc.Name + "=" + svc.Listen
	}
	log.Info("proxy started", "version", version, "target", cfg.Target.Name, "agent", cfg.Target.Agent != "",
		"services", names, "admin", cfg.Admin.Listen, "state", mon.State(ctx))

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.stop(stopCtx)
	}
	return err
}
