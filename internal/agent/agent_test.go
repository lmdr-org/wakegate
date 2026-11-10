package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clauer-org/wakegate/internal/api"
)

// fakeSystem records sleeps and serves canned checks.
type fakeSystem struct {
	mu         sync.Mutex
	sleeps     []string
	sleepErr   error
	sessions   []Session
	sessionErr error
	inhibitors []Inhibitor
	busy       map[string]bool
	busyErr    map[string]error
	resumed    chan struct{} // WaitResume returns when it is closed (or at once if nil)
}

func (f *fakeSystem) Sleep(_ context.Context, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, action)
	return f.sleepErr
}

func (f *fakeSystem) WaitResume(ctx context.Context, _ time.Duration) error {
	if f.resumed == nil {
		return nil
	}
	select {
	case <-f.resumed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeSystem) Sessions(context.Context) ([]Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, f.sessionErr
}

func (f *fakeSystem) Inhibitors(context.Context) ([]Inhibitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inhibitors, nil
}

func (f *fakeSystem) Busy(_ context.Context, cmd string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy[cmd], f.busyErr[cmd]
}

func (f *fakeSystem) Memory() (*api.Memory, error) {
	return &api.Memory{Total: 8 << 30, Available: 6 << 30}, nil
}

func (f *fakeSystem) sleepCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sleeps...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestAgent(t *testing.T, yaml string, sys System) *Agent {
	t.Helper()
	cfg, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = t.TempDir()
	a := New(cfg, sys, quiet(), "test")
	a.Grace = 50 * time.Millisecond
	return a
}

// idleFor pretends the last activity was d ago.
func (a *Agent) idleFor(d time.Duration) {
	a.mu.Lock()
	a.lastActivity = time.Now().Add(-d)
	a.mu.Unlock()
	a.checkMu.Lock()
	a.checksAt = time.Time{}
	a.checkMu.Unlock()
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9777" || cfg.IdleTimeout != 30*time.Minute || cfg.Sleep != api.Suspend ||
		cfg.KeepAwake.Sessions != SessionsAll || !*cfg.KeepAwake.Inhibitors {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, bad := range []string{
		"idle_timeout: 30s",
		"idle_timeout: 90s",
		"idle_timeout: 25h",
		"sleep: shutdown",
		"keep_awake: {sessions: some}",
		"keep_awake: {commands: ['  ']}",
		"unknown: 1",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	cfg, err := LoadConfig("../../deploy/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenFile != "/etc/wakegate/agent.token" {
		t.Fatalf("token file %q", cfg.TokenFile)
	}
}

func TestReadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("0123456789abcdef0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKEGATE_TOKEN", "")
	if tok, err := ReadToken(path); err != nil || tok != "0123456789abcdef0123" {
		t.Fatalf("%q, %v", tok, err)
	}
	t.Setenv("WAKEGATE_TOKEN", "short")
	if _, err := ReadToken(path); err == nil {
		t.Fatal("a short token must be refused")
	}
}

func TestEvaluate(t *testing.T) {
	now := time.Now()
	base := input{now: now, idleSince: now.Add(-40 * time.Minute), idleAfter: 30 * time.Minute}
	if v := evaluate(base); !v.idle() {
		t.Fatalf("idle for 40 of 30 minutes: %+v", v)
	}
	in := base
	in.idleSince = now.Add(-10 * time.Minute)
	if v := evaluate(in); v.idle() || v.remaining != 20*time.Minute {
		t.Fatalf("10 of 30 minutes: %+v", v)
	}
	in = base
	in.active = 2
	in.commands = []commandResult{{command: "pgrep restic", busy: true}, {command: "false"}, {command: "x", err: errors.New("exit status 127")}}
	in.inhibitors = []Inhibitor{{Who: "backup", Why: "copying"}}
	in.stayAwakeUntil = now.Add(time.Hour)
	in.sessions = []Session{{ID: "3", User: "lmdr", Remote: true}}
	v := evaluate(in)
	var got []string
	for _, h := range v.holds {
		got = append(got, h.Kind+": "+h.Detail)
	}
	want := []string{
		"requests: 2 requests through the proxy",
		"command: pgrep restic",
		"command: x: check failed: exit status 127",
		"inhibitor: backup: copying",
		"stay_awake: stay awake until " + in.stayAwakeUntil.Format("15:04"),
		"session: lmdr (SSH)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("holds:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(v.blocking()) != 4 {
		t.Fatalf("blocking: %+v", v.blocking())
	}
}

func TestIdleSleepsAndWakes(t *testing.T) {
	sys := &fakeSystem{}
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false}", sys)
	a.tick(context.Background())
	if len(sys.sleepCalls()) != 0 {
		t.Fatal("slept right after start")
	}
	a.idleFor(31 * time.Minute)
	a.tick(context.Background())
	if got := sys.sleepCalls(); len(got) != 1 || got[0] != api.Suspend {
		t.Fatalf("sleeps: %v", got)
	}
	if h := a.Health(); h.State != api.Ready {
		t.Fatalf("after resume: %+v", h)
	}
	// Back from sleep, the timeout starts over.
	if s := a.Status(context.Background()); s.Sleep.InSeconds == nil || *s.Sleep.InSeconds < 29*60 {
		t.Fatalf("after resume: %+v", s.Sleep)
	}
}

func TestShuttingDownUntilResume(t *testing.T) {
	sys := &fakeSystem{resumed: make(chan struct{})}
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false}", sys)
	a.idleFor(time.Hour)
	done := make(chan struct{})
	go func() { a.tick(context.Background()); close(done) }()
	waitFor(t, func() bool { return len(sys.sleepCalls()) == 1 })
	if h := a.Health(); h.State != api.ShuttingDown || h.Action != api.Suspend {
		t.Fatalf("while asleep: %+v", h)
	}
	close(sys.resumed)
	<-done
	if h := a.Health(); h.State != api.Ready {
		t.Fatalf("after resume: %+v", h)
	}
}

func TestRequestDuringGraceCancelsSleep(t *testing.T) {
	sys := &fakeSystem{}
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false}", sys)
	a.Grace = 300 * time.Millisecond
	a.idleFor(time.Hour)
	done := make(chan struct{})
	go func() { a.tick(context.Background()); close(done) }()
	waitFor(t, func() bool { return a.Health().State == api.ShuttingDown })
	a.Heartbeat(api.Heartbeat{Active: 1})
	<-done
	if got := sys.sleepCalls(); len(got) != 0 {
		t.Fatalf("slept despite the request: %v", got)
	}
	if a.Health().State != api.Ready {
		t.Fatal("not ready after the cancelled sleep")
	}
}

func TestFailedSleepWaitsAnotherTimeout(t *testing.T) {
	sys := &fakeSystem{sleepErr: errors.New("Access denied")}
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false}", sys)
	a.idleFor(time.Hour)
	a.tick(context.Background())
	a.tick(context.Background())
	if got := sys.sleepCalls(); len(got) != 1 {
		t.Fatalf("sleeps: %v", got)
	}
}

func TestHeartbeat(t *testing.T) {
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false}", &fakeSystem{})
	a.idleFor(time.Hour)
	ctx := context.Background()
	if v := a.verdict(ctx, api.Suspend); !v.idle() {
		t.Fatalf("%+v", v)
	}
	a.Heartbeat(api.Heartbeat{Active: 2})
	v := a.verdict(ctx, api.Suspend)
	if v.idle() || len(v.holds) != 1 || v.holds[0].Kind != api.HoldRequests {
		t.Fatalf("active requests: %+v", v)
	}
	// The requests ended 10 minutes ago: the computer is idle for 10 minutes, no more.
	ten := 600.0
	a.idleFor(time.Hour)
	a.Heartbeat(api.Heartbeat{IdleSeconds: &ten})
	v = a.verdict(ctx, api.Suspend)
	if v.idle() || v.remaining < 19*time.Minute || v.remaining > 21*time.Minute {
		t.Fatalf("ended 10 min ago: %+v", v)
	}
	// An older end does not move the last activity back.
	hour := 3600.0
	a.Heartbeat(api.Heartbeat{IdleSeconds: &hour})
	if v := a.verdict(ctx, api.Suspend); v.remaining < 19*time.Minute {
		t.Fatalf("older end: %+v", v)
	}
}

func TestSessionsPolicyAndRelease(t *testing.T) {
	sys := &fakeSystem{sessions: []Session{
		{ID: "1", User: "lmdr", Remote: true},
		{ID: "2", User: "lmdr", Type: "wayland", Idle: true},
	}}
	ctx := context.Background()
	all := newTestAgent(t, "keep_awake: {inhibitors: false}", sys)
	all.idleFor(time.Hour)
	if v := all.verdict(ctx, api.Suspend); len(v.holds) != 2 {
		t.Fatalf("all: %+v", v.holds)
	}
	active := newTestAgent(t, "keep_awake: {sessions: active, inhibitors: false}", sys)
	active.idleFor(time.Hour)
	if v := active.verdict(ctx, api.Suspend); len(v.holds) != 1 || v.holds[0].Detail != "lmdr (SSH)" {
		t.Fatalf("active: %+v", v.holds)
	}

	if err := all.StayAwake(time.Hour, false); err != nil {
		t.Fatal(err)
	}
	resp, err := all.Release(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.IgnoredSessions != 2 || !resp.StayAwakeCancelled || len(resp.Sleep.HeldBy) != 0 {
		t.Fatalf("release: %+v", resp)
	}
	// A session opened afterwards holds the computer again; a closed one is forgotten.
	sys.mu.Lock()
	sys.sessions = []Session{{ID: "1", User: "lmdr", Remote: true}, {ID: "7", User: "lmdr"}}
	sys.mu.Unlock()
	all.idleFor(time.Hour)
	v := all.verdict(ctx, api.Suspend)
	if len(v.holds) != 1 || v.holds[0].Detail != "lmdr (console)" {
		t.Fatalf("after release: %+v", v.holds)
	}
	if s := all.Status(ctx); s.Sleep.IgnoredSessions != 1 {
		t.Fatalf("ignored: %d", s.Sleep.IgnoredSessions)
	}
}

func TestInhibitorsDependOnAction(t *testing.T) {
	sys := &fakeSystem{inhibitors: []Inhibitor{
		{What: "sleep", Who: "restic", Why: "backup", Mode: "block"},
		{What: "sleep", Who: "gsd-power", Why: "lock screen", Mode: "delay"},
		{What: "idle:handle-lid-switch", Who: "gnome", Why: "x", Mode: "block"},
	}}
	a := newTestAgent(t, "keep_awake: {sessions: none}", sys)
	a.idleFor(time.Hour)
	ctx := context.Background()
	if v := a.verdict(ctx, api.Suspend); len(v.holds) != 1 || v.holds[0].Detail != "restic: backup" {
		t.Fatalf("suspend: %+v", v.holds)
	}
	if v := a.verdict(ctx, api.PowerOff); len(v.holds) != 0 {
		t.Fatalf("poweroff: %+v", v.holds)
	}
}

func TestSetIdleTimeoutPersists(t *testing.T) {
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false}", &fakeSystem{})
	if err := a.SetIdleTimeout(15); err != nil {
		t.Fatal(err)
	}
	if err := a.SetIdleTimeout(24*60 + 1); err == nil {
		t.Fatal("more than 24 h must be refused")
	}
	again := New(a.cfg, &fakeSystem{}, quiet(), "test")
	if again.IdleTimeout() != 15*time.Minute {
		t.Fatalf("after restart: %s", again.IdleTimeout())
	}
	// A shorter timeout counts from the change, not from the last activity.
	a.idleFor(time.Hour)
	if err := a.SetIdleTimeout(10); err != nil {
		t.Fatal(err)
	}
	if v := a.verdict(context.Background(), api.Suspend); v.idle() {
		t.Fatal("shortening the timeout put the computer to sleep at once")
	}
}

func TestAPI(t *testing.T) {
	sys := &fakeSystem{busy: map[string]bool{}}
	a := newTestAgent(t, "keep_awake: {sessions: none, inhibitors: false, commands: [pgrep restic]}", sys)
	const token = "0123456789abcdef0123"
	srv := httptest.NewServer(a.Handler(token))
	defer srv.Close()
	call := func(method, path, body string, auth bool) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v
	}
	if code, v := call("GET", "/healthz", "", false); code != 200 || v["state"] != "ready" {
		t.Fatalf("healthz: %d %v", code, v)
	}
	if code, _ := call("GET", "/v1/status", "", false); code != 401 {
		t.Fatalf("status without token: %d", code)
	}
	if code, v := call("GET", "/v1/status", "", true); code != 200 || v["hostname"] == "" {
		t.Fatalf("status: %d %v", code, v)
	}
	if code, _ := call("POST", "/v1/heartbeat", `{"active": 1, "idle_seconds": null}`, true); code != 200 {
		t.Fatalf("heartbeat: %d", code)
	}
	if code, _ := call("POST", "/v1/idle", `{"minutes": 15, "extra": 1}`, true); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	if code, v := call("POST", "/v1/idle", `{"minutes": 15}`, true); code != 200 || v["idle_minutes"] != 15.0 {
		t.Fatalf("idle: %d %v", code, v)
	}
	if code, v := call("POST", "/v1/stay-awake", `{"minutes": 120}`, true); code != 200 || v["stay_awake_until"] == nil {
		t.Fatalf("stay-awake: %d %v", code, v)
	}
	// A request in progress refuses an explicit sleep; the stay-awake does not.
	if code, v := call("POST", "/v1/sleep", "", true); code != 409 || v["held_by"] == nil {
		t.Fatalf("sleep while busy: %d %v", code, v)
	}
	a.Heartbeat(api.Heartbeat{Active: 0})
	sys.mu.Lock()
	sys.busy["pgrep restic"] = true
	sys.mu.Unlock()
	a.idleFor(0)
	if code, _ := call("POST", "/v1/sleep", `{"action": "hibernate"}`, true); code != 409 {
		t.Fatalf("sleep during a backup: %d", code)
	}
	sys.mu.Lock()
	sys.busy["pgrep restic"] = false
	sys.mu.Unlock()
	a.idleFor(0)
	if code, _ := call("POST", "/v1/sleep", `{"action": "standby"}`, true); code != 400 {
		t.Fatalf("unknown action: %d", code)
	}
	if code, v := call("POST", "/v1/sleep", `{"action": "hibernate"}`, true); code != 202 || v["action"] != "hibernate" {
		t.Fatalf("sleep: %d %v", code, v)
	}
	if code, v := call("GET", "/healthz", "", false); code != 503 || v["state"] != "shutting-down" {
		t.Fatalf("healthz while going to sleep: %d %v", code, v)
	}
	waitFor(t, func() bool { return len(sys.sleepCalls()) == 1 && a.Health().State == api.Ready })
	if got := sys.sleepCalls(); got[0] != api.Hibernate {
		t.Fatalf("sleeps: %v", got)
	}
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
