package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"github.com/lmdr-org/wakegate/internal/api"
)

// WakePath is served by the proxy on every HTTP service port: POST /__wake wakes the
// target, waits until the service is up, and answers 200. A client that checks a
// service before using it calls it when its check gets the proxy's 503.
const WakePath = "/__wake"

// HTTPProxy relays one HTTP service.
type HTTPProxy struct {
	Svc      *Service
	Target   string // name, for messages and the waiting page
	Mon      *Monitor
	Activity *Activity
	Stats    *Stats
	// BootTimeout bounds a wake-up; WaitTimeout a target going down plus its wake-up.
	BootTimeout time.Duration
	WaitTimeout time.Duration
	MaxBody     int64
	Log         *slog.Logger

	rp     *httputil.ReverseProxy
	health *http.Client
}

// attempt receives the error of a relay that may be tried again; nothing has been
// written to the client then.
type attempt struct{ err error }

type attemptKey struct{}

// NewHTTPProxy prepares the relay to the service's upstream.
func NewHTTPProxy(p *HTTPProxy) *HTTPProxy {
	target := &url.URL{Scheme: "http", Host: p.Svc.Addr()}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A sleeping target does not answer: fail fast rather than wait for the TCP timeout.
	transport.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport:     transport,
		FlushInterval: -1, // streams (server-sent events, token by token) go out as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if a, ok := r.Context().Value(attemptKey{}).(*attempt); ok {
				a.err = err // the loop in relayWaking decides
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			p.Mon.Invalidate()
			if p.wantsPage(r) {
				p.writeWaitingPage(w, Ready)
				return
			}
			writeError(w, http.StatusBadGateway, 0, fmt.Sprintf("%s did not answer: %v", p.Svc.Name, err))
		},
	}
	p.health = &http.Client{Transport: transport, Timeout: 3 * time.Second}
	return p
}

func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	explicit := r.Method == http.MethodPost && r.URL.Path == WakePath
	wakes := explicit || p.Svc.Wakes(r.Method, r.URL.Path)
	finish := p.Stats.Begin(p.Svc.Name, r.Method, r.URL.Path, clientOf(r), wakes)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	outcome, woke := Relayed, false
	defer func() {
		status := sw.status
		if !sw.wrote && r.Context().Err() != nil {
			status = 499 // the client left before the answer
		}
		finish(status, outcome, woke, 0)
	}()
	if explicit {
		outcome, woke = p.serveWake(sw, r)
		return
	}
	st := p.Mon.State(r.Context())
	if st != Ready && p.wantsPage(r) {
		p.Mon.WakeSoon(p.WaitTimeout)
		p.Log.Info("browser: waking the target", "service", p.Svc.Name, "target", st)
		outcome, woke = Waiting, true
		p.writeWaitingPage(sw, st)
		return
	}
	if wakes {
		outcome, woke = p.relayWaking(sw, r)
		return
	}
	if st == Off || st == Waking {
		// A request that only checks on the service (a model list, a health probe, a
		// WebSocket reconnecting) does not wake the target.
		outcome = Refused
		writeError(sw, http.StatusServiceUnavailable, 60, fmt.Sprintf("%s is asleep; this request does not wake it", p.Target))
		return
	}
	p.rp.ServeHTTP(sw, r)
}

// wantsPage: a browser opening a page gets the waiting page rather than a request held
// for a minute.
func (p *HTTPProxy) wantsPage(r *http.Request) bool {
	if !*p.Svc.WaitingPage || r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
		return false
	}
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" || strings.Contains(r.Header.Get("Accept"), "text/html")
}

// ready waits for the target, then for the service, and returns false after writing
// the error if either does not come up. woke reports a wait for a wake-up.
func (p *HTTPProxy) ready(w http.ResponseWriter, r *http.Request, serviceDeadline *time.Time, check bool) (ok, woke bool) {
	if st := p.Mon.State(r.Context()); st != Ready {
		woke, check = true, true
		p.Log.Info("request held until the target is ready", "service", p.Svc.Name, "path", r.URL.Path, "target", st)
		timeout := p.BootTimeout
		if st == ShuttingDown {
			timeout = p.WaitTimeout
		}
		if err := p.Mon.WaitReady(r.Context(), timeout); err != nil {
			if r.Context().Err() == nil {
				writeError(w, http.StatusGatewayTimeout, 0, fmt.Sprintf("%s did not wake up within %s", p.Target, timeout))
			}
			return false, woke
		}
		*serviceDeadline = time.Time{} // a fresh start: the service gets its whole start_timeout
	}
	if serviceDeadline.IsZero() {
		*serviceDeadline = time.Now().Add(p.Svc.StartTimeout)
	}
	if check && p.Svc.Health != "" {
		if err := p.waitHealthy(r.Context(), *serviceDeadline); err != nil {
			if r.Context().Err() == nil {
				writeError(w, http.StatusGatewayTimeout, 0, fmt.Sprintf("%s did not start within %s", p.Svc.Name, p.Svc.StartTimeout))
			}
			return false, woke
		}
	}
	return true, woke
}

// relayWaking relays a request that may wake the target: it waits for the target and
// the service, then relays, and sends the request again if it did not go through (the
// target was going to sleep, the service was still starting).
//
// The body is kept in memory to be sent again, up to MaxBody. A larger body is not: once
// the target is ready, what was read and the rest are streamed, in a single attempt.
func (p *HTTPProxy) relayWaking(w http.ResponseWriter, r *http.Request) (outcome string, woke bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, p.MaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, 0, "unreadable request body")
		return Failed, false
	}
	replay := int64(len(body)) <= p.MaxBody
	// Counted from the start, waits included: the heartbeat tells the agent a request
	// is waiting, and a sleep still in its grace period is called off.
	end := p.Activity.Begin()
	defer end()
	var serviceDeadline time.Time
	check := false
	for {
		ok, waited := p.ready(w, r, &serviceDeadline, check)
		woke = woke || waited
		if !ok {
			return gaveUp(r), woke
		}
		a := &attempt{}
		req := r.Clone(context.WithValue(r.Context(), attemptKey{}, a))
		if replay {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		} else {
			req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
			req.ContentLength = r.ContentLength
			req.GetBody = nil
		}
		p.rp.ServeHTTP(w, req)
		if a.err == nil {
			if woke {
				p.Log.Info("request relayed after a wake-up", "service", p.Svc.Name, "path", r.URL.Path)
			}
			return Relayed, woke
		}
		if r.Context().Err() != nil {
			return Relayed, woke
		}
		p.Mon.Invalidate()
		if !replay {
			p.Log.Warn("request failed; its body is too large to be sent again", "service", p.Svc.Name,
				"path", r.URL.Path, "err", a.err)
			writeError(w, http.StatusBadGateway, 0, fmt.Sprintf(
				"%s did not answer; requests above %d MiB are not sent again", p.Svc.Name, p.MaxBody>>20))
			return Failed, woke
		}
		if time.Now().After(serviceDeadline) {
			writeError(w, http.StatusBadGateway, 0, fmt.Sprintf("%s did not answer: %v", p.Svc.Name, a.err))
			return Failed, woke
		}
		p.Log.Info("request not relayed; trying again", "service", p.Svc.Name, "path", r.URL.Path, "err", a.err)
		check = true
		if !sleepCtx(r.Context(), time.Second) {
			return Relayed, woke
		}
	}
}

// serveWake answers POST /__wake once the service is up.
func (p *HTTPProxy) serveWake(w http.ResponseWriter, r *http.Request) (outcome string, woke bool) {
	end := p.Activity.Begin()
	defer end()
	var deadline time.Time
	ok, woke := p.ready(w, r, &deadline, true)
	if !ok {
		return gaveUp(r), woke
	}
	if p.Svc.Health == "" {
		if err := waitDial(r.Context(), p.Svc.Addr(), deadline); err != nil {
			if r.Context().Err() == nil {
				writeError(w, http.StatusGatewayTimeout, 0, fmt.Sprintf("%s did not start within %s", p.Svc.Name, p.Svc.StartTimeout))
			}
			return gaveUp(r), woke
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": p.Svc.Name})
	return Relayed, woke
}

// gaveUp is the outcome of a wait that ended early: a timeout, unless the client left
// first (its status is then 499, not an error).
func gaveUp(r *http.Request) string {
	if r.Context().Err() != nil {
		return Relayed
	}
	return Timeout
}

// waitHealthy polls the health path until it answers 2xx.
func (p *HTTPProxy) waitHealthy(ctx context.Context, deadline time.Time) error {
	u := "http://" + p.Svc.Addr() + p.Svc.Health
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := p.health.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not healthy", u)
		}
		if !sleepCtx(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

// waitDial waits until addr accepts a connection.
func waitDial(ctx context.Context, addr string, deadline time.Time) error {
	for {
		d := net.Dialer{Timeout: 3 * time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		if !sleepCtx(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

// sleepCtx waits d; false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

//go:embed web/waiting.html
var waitingHTML string

var waitingPage = template.Must(template.New("waiting").Parse(waitingHTML))

func (p *HTTPProxy) writeWaitingPage(w http.ResponseWriter, st State) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	line := fmt.Sprintf("%s is waking up.", p.Target)
	if st == Ready {
		line = fmt.Sprintf("%s is awake; %s is starting.", p.Target, p.Svc.Name)
	}
	_ = waitingPage.Execute(w, map[string]string{"Service": p.Svc.Name, "Target": p.Target, "Line": line})
}

func writeError(w http.ResponseWriter, status, retryAfter int, msg string) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.Header().Set("X-Wakegate", "1")
	writeJSON(w, status, api.NewError(msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusWriter records the status without breaking Flush (streams) or Hijack
// (WebSockets).
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) Flush() { _ = http.NewResponseController(s.ResponseWriter).Flush() }

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(s.ResponseWriter).Hijack()
	if err == nil {
		s.wrote = true
		if s.status == http.StatusOK {
			s.status = http.StatusSwitchingProtocols
		}
	}
	return conn, rw, err
}
