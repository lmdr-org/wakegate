package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTarget plays the computer that sleeps: the state its probe reports, its wake-ups,
// and the service behind it, which drops connections while the target is not ready.
type fakeTarget struct {
	mu       sync.Mutex
	state    State
	action   string // reported while shutting down
	bootLeft int    // probes still answering off after a wake-up
	booting  bool
	wakes    int
	offs     int // times the target went off
	bodies   []string
	// sleepOnNext: the next request reaching the service finds the target going to
	// sleep (its connection drops), and the target is off right after.
	sleepOnNext bool
}

func (t *fakeTarget) Probe(context.Context) (State, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.booting {
		if t.bootLeft > 0 {
			t.bootLeft--
			return Off, ""
		}
		t.booting, t.state = false, Ready
	}
	if t.state == ShuttingDown {
		return ShuttingDown, t.action
	}
	return t.state, ""
}

func (t *fakeTarget) wake() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.wakes++
	if t.state == Off && !t.booting {
		t.booting, t.bootLeft = true, 3
	}
	return nil
}

func (t *fakeTarget) set(s State) {
	t.mu.Lock()
	t.state, t.booting = s, false
	if s == Off {
		t.offs++
	}
	t.mu.Unlock()
}

func (t *fakeTarget) wakeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.wakes
}

func (t *fakeTarget) received() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.bodies...)
}

func dropConn(w http.ResponseWriter) {
	if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
		c.Close()
	}
}

// service echoes the path and body while the target is ready.
func (t *fakeTarget) service() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		t.mu.Lock()
		ready := t.state == Ready && !t.booting
		if ready && t.sleepOnNext {
			t.sleepOnNext, ready = false, false
			t.state = Off
		}
		if ready {
			t.bodies = append(t.bodies, r.Method+" "+r.URL.Path+" "+string(raw))
		}
		t.mu.Unlock()
		if !ready {
			dropConn(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`","body":`+strconv.Quote(string(raw))+`}`)
	})
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testMonitor(p Prober, wake func() error) *Monitor {
	return &Monitor{Prober: p, Wake: wake, BootWindow: time.Second, Log: quiet(),
		poll: 5 * time.Millisecond, cache: time.Nanosecond, resend: 20 * time.Millisecond}
}

// testService builds a service from YAML-like settings, relaying to addr.
func testService(t *testing.T, name, addr string, wakeOn, neverWake []string) *Service {
	t.Helper()
	s := &Service{Name: name, Listen: ":1", Upstream: addr, WakeOn: wakeOn, NeverWake: neverWake}
	if err := s.complete("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	s.StartTimeout = 2 * time.Second
	return s
}

type rig struct {
	target   *fakeTarget
	mon      *Monitor
	stats    *Stats
	activity *Activity
	front    *httptest.Server
	proxy    *HTTPProxy
}

// newRig puts an HTTP proxy in front of a fake target whose service handles the
// relayed requests.
func newRig(t *testing.T, st State, wakeOn, neverWake []string) *rig {
	t.Helper()
	ft := &fakeTarget{state: st}
	up := httptest.NewServer(ft.service())
	t.Cleanup(up.Close)
	addr := up.Listener.Addr().String()
	svc := testService(t, "llm", addr, wakeOn, neverWake)
	r := &rig{target: ft, mon: testMonitor(ft, ft.wake), activity: &Activity{}}
	r.stats = NewStats([]*Service{svc}, 10)
	r.proxy = NewHTTPProxy(&HTTPProxy{Svc: svc, Target: "tower", Mon: r.mon, Activity: r.activity, Stats: r.stats,
		BootTimeout: 2 * time.Second, WaitTimeout: 3 * time.Second, MaxBody: 1 << 10, Log: quiet()})
	r.front = httptest.NewServer(r.proxy)
	t.Cleanup(r.front.Close)
	return r
}

func (r *rig) do(t *testing.T, method, path, body string, header map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.front.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

// settled waits until the proxy has finished counting every request: a client can get
// its answer before the handler returns.
func (r *rig) settled(t *testing.T) Snapshot {
	t.Helper()
	waitFor(t, func() bool {
		for _, e := range r.stats.Snapshot().Recent {
			if e.Pending {
				return false
			}
		}
		return r.activity.Active() == 0
	})
	return r.stats.Snapshot()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 3 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// freeAddr returns a local address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
