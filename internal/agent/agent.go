package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lmdr-org/wakegate/internal/api"
	"github.com/lmdr-org/wakegate/internal/jsonfile"
)

// Timings. The proxy polls every two seconds: the grace period leaves it time to see
// "shutting-down", and to report a request that is waiting, before the computer sleeps.
const (
	CheckInterval = 15 * time.Second
	DefaultGrace  = 5 * time.Second
	ResumeTimeout = 2 * time.Minute
	// activeFor is how long a heartbeat that reports requests holds the computer: a
	// proxy that stops sending heartbeats does not hold it forever.
	activeFor = 10 * time.Second
	// checksFor is how long the results of the slow checks (commands, sessions,
	// inhibitors) are reused, so a status page polled every few seconds does not run
	// them each time.
	checksFor = 5 * time.Second
	// MaxStayAwake bounds POST /v1/stay-awake.
	MaxStayAwake = 7 * 24 * time.Hour
)

var (
	// ErrNotReady: a sleep is already under way.
	ErrNotReady = errors.New("already going to sleep")
	// ErrBusy: work in progress prevents an explicit sleep.
	ErrBusy = errors.New("work in progress")
)

// Agent holds the state of the computer and decides when it sleeps.
type Agent struct {
	// Grace is the time between announcing "shutting-down" and sleeping; a request
	// reported meanwhile calls the sleep off. DefaultGrace unless changed before Run.
	Grace time.Duration

	cfg     *Config
	sys     System
	log     *slog.Logger
	version string

	checkMu  sync.Mutex // serializes the slow checks
	checks   checkResults
	checksAt time.Time

	mu             sync.Mutex
	state          api.State
	action         string // while shutting down
	lastActivity   time.Time
	activeUntil    time.Time
	active         int
	activity       uint64 // counts heartbeats that reported a request, so a pending sleep can notice one
	stayAwakeUntil time.Time
	idleMinutes    int       // set through POST /v1/idle; 0 for the configured timeout
	changedAt      time.Time // the new timeout counts from there
	ignored        map[string]bool
	awakeSince     time.Time
	tickAt         time.Time
	failing        map[string]string // last error of each check, to log changes only
}

type checkResults struct {
	commands     []commandResult
	sessions     []Session
	sessionErr   error
	inhibitors   []Inhibitor
	inhibitorErr error
}

type settings struct {
	IdleMinutes int       `json:"idle_minutes,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// New prepares an agent; the idle timeout chosen through the API is read back from the
// state directory.
func New(cfg *Config, sys System, log *slog.Logger, version string) *Agent {
	now := time.Now()
	a := &Agent{Grace: DefaultGrace, cfg: cfg, sys: sys, log: log, version: version,
		state: api.Ready, lastActivity: now, awakeSince: now, tickAt: now,
		ignored: map[string]bool{}, failing: map[string]string{}}
	if boot, err := bootTime(); err == nil && boot.Before(now) {
		a.awakeSince = boot
	}
	s, err := jsonfile.Load[settings](a.settingsPath())
	switch {
	case err != nil:
		log.Warn("settings unreadable, using the configured idle timeout", "file", a.settingsPath(), "err", err)
	case s.IdleMinutes >= 1 && time.Duration(s.IdleMinutes)*time.Minute <= MaxIdle:
		a.idleMinutes = s.IdleMinutes
	}
	return a
}

func (a *Agent) settingsPath() string {
	if a.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(a.cfg.StateDir, "agent.json")
}

// Run checks every CheckInterval whether the computer should sleep, until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.tick(ctx)
		}
	}
}

func (a *Agent) tick(ctx context.Context) {
	now := time.Now()
	a.mu.Lock()
	if Slept(now.Round(0).Sub(a.tickAt.Round(0)), now.Sub(a.tickAt)) {
		// Back from a sleep the agent did not ask for (power button, lid, another
		// tool): the idle timeout starts over, as after any wake-up.
		a.wokeLocked(now)
		a.log.Info("awake again after a sleep the agent did not request")
	}
	a.tickAt = now
	ready := a.state == api.Ready
	a.mu.Unlock()
	if !ready || a.cfg.Sleep == api.None {
		return
	}
	seq := a.activitySeq()
	if !a.verdict(ctx, a.cfg.Sleep).idle() {
		return
	}
	if a.begin(a.cfg.Sleep) != nil {
		return
	}
	a.finish(ctx, a.cfg.Sleep, seq, "idle")
}

// activitySeq is read before a sleep is decided: finish compares it again after the
// grace period, so a request that arrives anywhere in between cancels the sleep.
func (a *Agent) activitySeq() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activity
}

func (a *Agent) wokeLocked(now time.Time) {
	a.awakeSince, a.lastActivity, a.tickAt = now, now, now
}

// IdleTimeout is the timeout in effect.
func (a *Agent) IdleTimeout() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.idleTimeoutLocked()
}

func (a *Agent) idleTimeoutLocked() time.Duration {
	if a.idleMinutes > 0 {
		return time.Duration(a.idleMinutes) * time.Minute
	}
	return a.cfg.IdleTimeout
}

// runChecks runs the slow checks, or reuses results younger than checksFor. They are
// not cut short with the request that asked: a status page with a short timeout would
// otherwise turn a slow command into a failed check.
func (a *Agent) runChecks(ctx context.Context) checkResults {
	ctx = context.WithoutCancel(ctx)
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	if !a.checksAt.IsZero() && time.Since(a.checksAt) < checksFor {
		return a.checks
	}
	var r checkResults
	var wg sync.WaitGroup
	r.commands = make([]commandResult, len(a.cfg.KeepAwake.Commands))
	for i, cmd := range a.cfg.KeepAwake.Commands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			busy, err := a.sys.Busy(ctx, cmd)
			r.commands[i] = commandResult{command: cmd, busy: busy, err: err}
		}()
	}
	if a.cfg.KeepAwake.Sessions != SessionsNone {
		r.sessions, r.sessionErr = a.sys.Sessions(ctx)
	}
	if *a.cfg.KeepAwake.Inhibitors {
		r.inhibitors, r.inhibitorErr = a.sys.Inhibitors(ctx)
	}
	wg.Wait()
	a.logFailures(r)
	a.checks, a.checksAt = r, time.Now()
	return r
}

// logFailures logs a check when it starts or stops failing, not at every evaluation.
func (a *Agent) logFailures(r checkResults) {
	now := map[string]string{}
	for _, c := range r.commands {
		if c.err != nil {
			now["command "+c.command] = c.err.Error()
		}
	}
	if r.sessionErr != nil {
		now["sessions"] = r.sessionErr.Error()
	}
	if r.inhibitorErr != nil {
		now["inhibitors"] = r.inhibitorErr.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for check, msg := range now {
		if a.failing[check] != msg {
			a.log.Warn("check failed; it holds the computer awake", "check", check, "err", msg)
		}
	}
	for check := range a.failing {
		if _, still := now[check]; !still {
			a.log.Info("check works again", "check", check)
		}
	}
	a.failing = now
}

// verdict evaluates the holds for a sleep with this action.
func (a *Agent) verdict(ctx context.Context, action string) verdict {
	r := a.runChecks(ctx)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	in := input{now: now, idleAfter: a.idleTimeoutLocked(), commands: r.commands,
		inhibitorErr: r.inhibitorErr, stayAwakeUntil: a.stayAwakeUntil, sessionErr: r.sessionErr}
	in.idleSince = a.lastActivity
	if a.changedAt.After(in.idleSince) {
		in.idleSince = a.changedAt
	}
	if now.Before(a.activeUntil) {
		in.active = a.active
	}
	for _, i := range r.inhibitors {
		if i.Blocks(action) {
			in.inhibitors = append(in.inhibitors, i)
		}
	}
	if a.cfg.KeepAwake.Sessions != SessionsNone {
		in.sessions = a.holdingSessionsLocked(r.sessions, r.sessionErr == nil)
	}
	return evaluate(in)
}

// holdingSessionsLocked applies the sessions policy and the release. When the list is
// known, ignored sessions that have closed are forgotten.
func (a *Agent) holdingSessionsLocked(sessions []Session, known bool) []Session {
	var out []Session
	still := map[string]bool{}
	for _, s := range sessions {
		if a.ignored[s.ID] {
			still[s.ID] = true
			continue
		}
		if a.cfg.KeepAwake.Sessions == SessionsActive && s.Idle {
			continue
		}
		out = append(out, s)
	}
	if known {
		a.ignored = still
	}
	return out
}

// begin announces "shutting-down".
func (a *Agent) begin(action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != api.Ready {
		return ErrNotReady
	}
	a.state, a.action = api.ShuttingDown, action
	return nil
}

// finish waits the grace period, then sleeps, unless a request arrived meanwhile. After
// a suspend or a hibernation it returns once the computer is back.
func (a *Agent) finish(ctx context.Context, action string, seq uint64, reason string) {
	a.log.Info("going to sleep", "action", action, "reason", reason, "grace", a.Grace)
	t := time.NewTimer(a.Grace)
	select {
	case <-t.C:
	case <-ctx.Done():
		t.Stop()
	}
	a.mu.Lock()
	cancelled := a.activity != seq
	a.mu.Unlock()
	if cancelled {
		a.log.Info("request arrived; staying awake", "action", action)
		a.backToReady(false)
		return
	}
	start := time.Now()
	err := a.sys.Sleep(ctx, action)
	if err == nil && action != api.PowerOff {
		err = a.sys.WaitResume(ctx, ResumeTimeout)
	}
	switch {
	case err != nil:
		a.log.Error("sleep failed; staying awake for another idle timeout", "action", action, "err", err)
		a.backToReady(false)
	case action == api.PowerOff:
		// Stays "shutting-down" until the machine is off.
	default:
		a.log.Info("awake again", "action", action, "after", time.Since(start).Round(time.Second))
		a.backToReady(true)
	}
}

func (a *Agent) backToReady(woke bool) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state, a.action = api.Ready, ""
	a.lastActivity = now
	if woke {
		a.wokeLocked(now)
	}
}

// Heartbeat records what the proxy reports and returns the state.
func (a *Agent) Heartbeat(hb api.Heartbeat) api.Health {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if hb.Active > 0 {
		a.active, a.activeUntil = hb.Active, now.Add(activeFor)
		a.lastActivity = now
		a.activity++
	} else {
		a.active, a.activeUntil = 0, time.Time{}
		if hb.IdleSeconds != nil && *hb.IdleSeconds >= 0 {
			// A request that started and ended between two heartbeats. The margin
			// absorbs jitter: the same end reported twice is not a new request.
			end := now.Add(-time.Duration(*hb.IdleSeconds * float64(time.Second)))
			if end.After(a.lastActivity.Add(time.Second)) {
				a.lastActivity = end
				a.activity++
			}
		}
	}
	return a.healthLocked()
}

// Health is the state for GET /healthz.
func (a *Agent) Health() api.Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthLocked()
}

func (a *Agent) healthLocked() api.Health {
	return api.Health{State: a.state, Action: a.action}
}

// Status describes the computer and what holds it awake.
func (a *Agent) Status(ctx context.Context) api.Status {
	sleep := a.sleepStatus(ctx)
	host, _ := os.Hostname()
	mem, err := a.sys.Memory()
	if err != nil {
		mem = nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return api.Status{Version: a.version, Hostname: host, State: a.state, Action: a.action,
		AwakeSince: a.awakeSince, Memory: mem, Sleep: sleep}
}

func (a *Agent) sleepStatus(ctx context.Context) api.SleepStatus {
	v := a.verdict(ctx, a.cfg.Sleep)
	a.mu.Lock()
	defer a.mu.Unlock()
	s := api.SleepStatus{Action: a.cfg.Sleep, IdleMinutes: int(a.idleTimeoutLocked() / time.Minute),
		DefaultMinutes: int(a.cfg.IdleTimeout / time.Minute), HeldBy: v.holds,
		IgnoredSessions: len(a.ignored)}
	if s.HeldBy == nil {
		s.HeldBy = []api.Hold{}
	}
	if time.Now().Before(a.stayAwakeUntil) {
		until := a.stayAwakeUntil.Round(0)
		s.StayAwakeUntil = &until
	}
	if a.cfg.Sleep != api.None && a.state == api.Ready && len(v.holds) == 0 {
		in := int(v.remaining.Round(time.Second) / time.Second)
		s.InSeconds = &in
	}
	return s
}

// RequestSleep puts the computer to sleep now, unless work is in progress: sessions and
// a stay-awake do not prevent it. The sleep itself runs in the background.
func (a *Agent) RequestSleep(ctx context.Context, action string) ([]api.Hold, error) {
	seq := a.activitySeq()
	if blocking := a.verdict(ctx, action).blocking(); len(blocking) > 0 {
		return blocking, ErrBusy
	}
	if err := a.begin(action); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.stayAwakeUntil = time.Time{}
	a.mu.Unlock()
	go a.finish(context.WithoutCancel(ctx), action, seq, "requested")
	return nil, nil
}

// StayAwake holds the computer awake for the given time; 0 cancels. With extend, an
// end already further away is kept.
func (a *Agent) StayAwake(d time.Duration, extend bool) error {
	if d < 0 || d > MaxStayAwake {
		return fmt.Errorf("minutes: from 0 to %d", int(MaxStayAwake/time.Minute))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	until := time.Time{}
	if d > 0 {
		until = time.Now().Add(d)
	}
	if extend && !until.After(a.stayAwakeUntil) {
		return nil
	}
	a.stayAwakeUntil = until
	a.log.Info("stay awake", "until", until.Format(time.DateTime), "extend", extend)
	return nil
}

// SetIdleTimeout changes the idle timeout and saves it; 0 restores the configured one.
// The new timeout counts from now: shortening it does not put a computer that has been
// idle for long to sleep on the spot.
func (a *Agent) SetIdleTimeout(minutes int) error {
	if minutes < 0 || time.Duration(minutes)*time.Minute > MaxIdle {
		return fmt.Errorf("minutes: from 1 to %d, or 0 for the configured timeout", int(MaxIdle/time.Minute))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := jsonfile.Save(a.settingsPath(), settings{IdleMinutes: minutes, UpdatedAt: time.Now()}); err != nil {
		return fmt.Errorf("not saved: %w", err)
	}
	a.idleMinutes, a.changedAt = minutes, time.Now()
	a.log.Info("idle timeout", "timeout", a.idleTimeoutLocked())
	return nil
}

// Release stops the open sessions from holding the computer (a session opened later
// holds it again) and cancels the stay-awake. Work in progress still holds it, and the
// idle timeout counts from the last request: a computer idle for longer goes to sleep
// at the next check.
func (a *Agent) Release(ctx context.Context) (api.ReleaseResponse, error) {
	var resp api.ReleaseResponse
	if a.cfg.KeepAwake.Sessions != SessionsNone {
		sessions, err := a.sys.Sessions(ctx)
		if err != nil {
			return resp, err
		}
		a.mu.Lock()
		for _, s := range sessions {
			if !a.ignored[s.ID] {
				a.ignored[s.ID] = true
				resp.IgnoredSessions++
			}
		}
		a.mu.Unlock()
	}
	a.mu.Lock()
	if time.Now().Before(a.stayAwakeUntil) {
		a.stayAwakeUntil, resp.StayAwakeCancelled = time.Time{}, true
	}
	a.mu.Unlock()
	a.log.Info("hold released", "sessions_ignored", resp.IgnoredSessions, "stay_awake_cancelled", resp.StayAwakeCancelled)
	resp.Sleep = a.sleepStatus(ctx)
	return resp, nil
}
