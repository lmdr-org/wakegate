package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lmdr-org/wakegate/internal/api"
	"github.com/lmdr-org/wakegate/internal/wol"
)

// State is the target's state as the proxy sees it.
type State string

const (
	Off          State = "off"           // does not answer
	Waking       State = "waking"        // does not answer yet; woken a moment ago
	Ready        State = "ready"         // answers
	ShuttingDown State = "shutting-down" // the agent is putting it to sleep
)

// Timings of the monitor.
const (
	PollInterval = 2 * time.Second  // how often the state is read, and the heartbeat sent
	WakeResend   = 15 * time.Second // a magic packet sent while the target was still going down is lost
)

// Prober reads the target's state, and while it shuts down, the action: suspend,
// hibernate or poweroff.
type Prober interface {
	Probe(ctx context.Context) (State, string)
}

// AgentProber sends the heartbeat to the agent; its answer is the state.
type AgentProber struct {
	Agent    *AgentClient
	Activity *Activity
	Log      *slog.Logger

	rejected atomic.Bool // the agent refused the token; logged once
}

func (p *AgentProber) Probe(ctx context.Context) (State, string) {
	ctx, cancel := context.WithTimeout(ctx, PollInterval)
	defer cancel()
	code, raw, err := p.Agent.Do(ctx, http.MethodPost, api.PathHeartbeat, p.Activity.Heartbeat())
	if err != nil {
		return Off, ""
	}
	if code == http.StatusUnauthorized {
		if !p.rejected.Swap(true) {
			p.Log.Error("the agent refused the token: requests do not hold the target awake; copy the agent's token file")
		}
		if _, raw, err = p.Agent.Do(ctx, http.MethodGet, api.PathHealth, nil); err != nil {
			return Off, ""
		}
	} else if p.rejected.Swap(false) {
		p.Log.Info("the agent accepts the token again")
	}
	var h api.Health
	_ = json.Unmarshal(raw, &h)
	if h.State == api.ShuttingDown {
		return ShuttingDown, h.Action
	}
	return Ready, ""
}

// TCPProber, for a target without agent: it is ready when Addr accepts a connection.
type TCPProber struct {
	Addr string
}

func (p TCPProber) Probe(ctx context.Context) (State, string) {
	d := net.Dialer{Timeout: PollInterval}
	c, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return Off, ""
	}
	c.Close()
	return Ready, ""
}

// Waker wakes the target: a magic packet, or the configured command.
func Waker(t Target) func() error {
	if len(t.WakeCommand) > 0 {
		return func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, t.WakeCommand[0], t.WakeCommand[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", t.WakeCommand[0], err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	return func() error { return wol.Send(t.MAC, t.Broadcast, 3, 500*time.Millisecond) }
}

// ErrBootTimeout: the target was not ready in time.
var ErrBootTimeout = errors.New("the target did not wake up in time")

// Wakes counts the wake-ups since the proxy started, by how the target went down
// ("suspend", "hibernate", "poweroff", or "unknown" when the proxy did not see it go).
type Wakes struct {
	OK       map[string]int     `json:"ok"`
	Seconds  map[string]float64 `json:"seconds"` // total, from the first wake signal to ready
	Failures int                `json:"failures"`
	Last     *LastWake          `json:"last"`
}

// LastWake is the most recent successful wake-up.
type LastWake struct {
	From    string    `json:"from"`
	Seconds float64   `json:"seconds"`
	At      time.Time `json:"at"`
}

// Monitor follows the target's state (cached for PollInterval), wakes it and waits
// for it.
type Monitor struct {
	Prober     Prober
	Wake       func() error
	BootWindow time.Duration // after a wake signal, "off" shows as "waking" this long
	Log        *slog.Logger
	// Hold counts a background wake-up as a waiting request, like Activity.Begin: a
	// browser that opens a page while the target is about to sleep calls the sleep off.
	// nil: not counted.
	Hold   func() (end func())
	poll   time.Duration // between two reads in WaitReady; 1 s by default
	cache  time.Duration // PollInterval by default
	resend time.Duration // WakeResend by default

	mu        sync.Mutex
	last      State
	lastAt    time.Time
	wokenAt   time.Time // last wake signal of the current wake-up
	sentAt    time.Time
	inFlight  bool   // a background wake-up is running
	probing   bool   // a read is under way; others use the last state meanwhile
	sleep     string // how the target went down, seen while it shut down
	wakeStart time.Time
	wakeFrom  string
	wakes     Wakes
}

func (m *Monitor) cacheFor() time.Duration {
	if m.cache == 0 {
		return PollInterval
	}
	return m.cache
}

// State returns the target's state, read at most once per PollInterval.
func (m *Monitor) State(ctx context.Context) State {
	m.mu.Lock()
	if !m.lastAt.IsZero() && (time.Since(m.lastAt) < m.cacheFor() || m.probing) {
		st := m.displayLocked(m.last)
		m.mu.Unlock()
		return st
	}
	m.probing = true
	m.mu.Unlock()
	st, action := m.Prober.Probe(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probing = false
	m.last, m.lastAt = st, time.Now()
	switch st {
	case ShuttingDown:
		m.wokenAt = time.Time{}
		if action != "" {
			m.sleep = action
		}
	case Ready:
		m.wokenAt, m.sleep = time.Time{}, ""
		if !m.wakeStart.IsZero() {
			m.countWakeLocked()
		}
	case Off:
		if !m.wakeStart.IsZero() && time.Since(m.wakeStart) > m.BootWindow {
			m.wakes.Failures++
			m.Log.Error("the target did not wake up", "from", m.wakeFrom, "after", m.BootWindow)
			m.wakeStart = time.Time{}
		}
	}
	return m.displayLocked(st)
}

func (m *Monitor) countWakeLocked() {
	d := time.Since(m.wakeStart)
	m.initLocked()
	m.wakes.OK[m.wakeFrom]++
	m.wakes.Seconds[m.wakeFrom] += d.Seconds()
	m.wakes.Last = &LastWake{From: m.wakeFrom, Seconds: d.Seconds(), At: time.Now()}
	m.Log.Info("target awake", "from", m.wakeFrom, "after", d.Round(100*time.Millisecond))
	m.wakeStart = time.Time{}
}

func (m *Monitor) initLocked() {
	if m.wakes.OK == nil {
		m.wakes.OK, m.wakes.Seconds = map[string]int{}, map[string]float64{}
	}
}

func (m *Monitor) displayLocked(st State) State {
	if st == Off && !m.wokenAt.IsZero() && time.Since(m.wokenAt) < m.BootWindow {
		return Waking
	}
	return st
}

// Sleep says how the target went down (suspend, hibernate, poweroff) while it does not
// answer, if the proxy saw it go; "" otherwise.
func (m *Monitor) Sleep() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last != Off {
		return ""
	}
	return m.sleep
}

// Wakes returns a copy of the wake-up counters.
func (m *Monitor) Wakes() Wakes {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	w := Wakes{OK: map[string]int{}, Seconds: map[string]float64{}, Failures: m.wakes.Failures}
	for k, v := range m.wakes.OK {
		w.OK[k] = v
	}
	for k, v := range m.wakes.Seconds {
		w.Seconds[k] = v
	}
	if m.wakes.Last != nil {
		last := *m.wakes.Last
		w.Last = &last
	}
	return w
}

// Run reads the state every PollInterval until ctx ends, so the heartbeat goes out and
// the proxy sees the target go to sleep without waiting for a request.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(max(m.cacheFor(), 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.State(ctx)
		}
	}
}

// Invalidate makes the next State read the target again (after a failed relay).
func (m *Monitor) Invalidate() {
	m.mu.Lock()
	m.lastAt = time.Time{}
	m.mu.Unlock()
}

// SendWake sends the wake signal, at most once per WakeResend.
func (m *Monitor) SendWake() {
	resend := m.resend
	if resend == 0 {
		resend = WakeResend
	}
	m.mu.Lock()
	if !m.sentAt.IsZero() && time.Since(m.sentAt) < resend {
		m.mu.Unlock()
		return
	}
	m.sentAt = time.Now()
	if m.wokenAt.IsZero() || time.Since(m.wokenAt) >= m.BootWindow {
		m.wokenAt = time.Now()
	}
	if m.wakeStart.IsZero() {
		m.wakeStart, m.wakeFrom = time.Now(), m.sleep
		if m.wakeFrom == "" {
			m.wakeFrom = "unknown"
		}
	}
	m.mu.Unlock()
	if err := m.Wake(); err != nil {
		m.Log.Error("wake signal not sent", "err", err)
		return
	}
	m.Log.Info("wake signal sent")
}

// WaitReady waits until the target is ready: while it shuts down, it waits for it to be
// off; while it is off, it wakes it, again every WakeResend.
func (m *Monitor) WaitReady(ctx context.Context, timeout time.Duration) error {
	poll := m.poll
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		switch m.State(ctx) {
		case Ready:
			return nil
		case Off, Waking:
			m.SendWake()
		}
		if time.Now().After(deadline) {
			return ErrBootTimeout
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// WakeSoon starts a wake-up in the background, unless one is running (a waiting page,
// the Wake button, the awake window).
func (m *Monitor) WakeSoon(timeout time.Duration) {
	m.mu.Lock()
	if m.inFlight {
		m.mu.Unlock()
		return
	}
	m.inFlight = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			m.inFlight = false
			m.mu.Unlock()
		}()
		if m.Hold != nil {
			end := m.Hold()
			defer end()
		}
		if err := m.WaitReady(context.Background(), timeout); err != nil {
			m.Log.Warn("background wake-up", "err", err)
		}
	}()
}
