package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// TCPProxy relays raw connections to one service. Every connection wakes the target
// and holds it awake until it closes. The client's connection is accepted at once and
// waits, without any data, while the target wakes up: an SSH client waits for the
// server's banner.
type TCPProxy struct {
	Svc         *Service
	Mon         *Monitor
	Activity    *Activity
	Stats       *Stats
	BootTimeout time.Duration
	WaitTimeout time.Duration
	Log         *slog.Logger
}

// Serve accepts connections until ln is closed.
func (p *TCPProxy) Serve(ctx context.Context, ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		go p.handle(ctx, c)
	}
}

func (p *TCPProxy) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	finish := p.Stats.Begin(p.Svc.Name, "TCP", "", hostOf(c.RemoteAddr().String()), true)
	end := p.Activity.Begin() // waits included, as for HTTP requests
	defer end()
	woke := false
	if st := p.Mon.State(ctx); st != Ready {
		woke = true
		p.Log.Info("connection held until the target is ready", "service", p.Svc.Name, "target", st)
		timeout := p.BootTimeout
		if st == ShuttingDown {
			timeout = p.WaitTimeout
		}
		if err := p.Mon.WaitReady(ctx, timeout); err != nil {
			p.Log.Warn("connection dropped: the target did not wake up", "service", p.Svc.Name, "err", err)
			finish(0, Timeout, woke, 0)
			return
		}
	}
	up, err := dialUntil(ctx, p.Svc.Addr(), time.Now().Add(p.Svc.StartTimeout))
	if err != nil {
		p.Mon.Invalidate()
		p.Log.Warn("connection dropped: the service does not accept connections", "service", p.Svc.Name, "err", err)
		finish(0, Failed, woke, 0)
		return
	}
	defer up.Close()
	n := splice(c, up)
	end()
	finish(0, Relayed, woke, n)
}

func dialUntil(ctx context.Context, addr string, deadline time.Time) (net.Conn, error) {
	d := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	for {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		if !sleepCtx(ctx, time.Second) {
			return nil, ctx.Err()
		}
	}
}

// splice copies both ways until both sides are done, and returns the bytes copied. An
// end of stream is passed on as a half-close; an error closes both connections.
func splice(a, b net.Conn) int64 {
	var wg sync.WaitGroup
	var ab, ba int64
	copyHalf := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		var err error
		*n, err = io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go copyHalf(b, a, &ab)
	go copyHalf(a, b, &ba)
	wg.Wait()
	return ab + ba
}
