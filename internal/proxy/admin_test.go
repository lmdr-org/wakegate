package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clauer-org/wakegate/internal/agent"
	"github.com/clauer-org/wakegate/internal/api"
)

const testToken = "0123456789abcdef0123"

// stubSystem is an agent.System that never sleeps for real: Sleep turns the fake
// target off, as a suspend would.
type stubSystem struct{ target *fakeTarget }

func (s stubSystem) Sleep(context.Context, string) error {
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.target.set(Off)
	}()
	return nil
}

// WaitResume returns once the target has gone off and come back, as the kernel's
// suspend counters would show.
func (s stubSystem) WaitResume(ctx context.Context, _ time.Duration) error {
	s.target.mu.Lock()
	offs := s.target.offs
	s.target.mu.Unlock()
	for {
		s.target.mu.Lock()
		back := s.target.offs > offs && s.target.state == Ready
		s.target.mu.Unlock()
		if back {
			return nil
		}
		if !sleepCtx(ctx, 5*time.Millisecond) {
			return ctx.Err()
		}
	}
}
func (stubSystem) Sessions(context.Context) ([]agent.Session, error)     { return nil, nil }
func (stubSystem) Inhibitors(context.Context) ([]agent.Inhibitor, error) { return nil, nil }
func (stubSystem) Busy(context.Context, string) (bool, error)            { return false, nil }
func (stubSystem) Memory() (*api.Memory, error) {
	return &api.Memory{Total: 1 << 34, Available: 1 << 33}, nil
}

// agentRig runs a real agent behind a switch: while the fake target is not ready, its
// connections drop, as a sleeping machine's would.
type agentRig struct {
	*rig
	agent  *agent.Agent
	client *AgentClient
	admin  *httptest.Server
}

func newAgentRig(t *testing.T) *agentRig {
	t.Helper()
	r := newRig(t, Ready, ollamaWakeOn, nil)
	cfg, err := agent.ParseConfig([]byte("keep_awake: {sessions: none, inhibitors: false}"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = t.TempDir()
	a := agent.New(cfg, stubSystem{target: r.target}, quiet(), "test")
	a.Grace = 300 * time.Millisecond
	h := a.Handler(testToken)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.target.mu.Lock()
		up := r.target.state != Off && !r.target.booting
		r.target.mu.Unlock()
		if !up {
			dropConn(w)
			return
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	client := &AgentClient{URL: srv.URL, Token: testToken, HTTP: &http.Client{}}
	// The monitor now reads the real agent; the fake target still boots on wake-up.
	r.mon.Prober = &AgentProber{Agent: client, Activity: r.activity, Log: quiet()}
	r.mon.Hold = r.activity.Begin
	r.mon.Wake = func() error {
		r.target.mu.Lock()
		defer r.target.mu.Unlock()
		r.target.wakes++
		if r.target.state == Off {
			r.target.state = Ready
		}
		return nil
	}
	cfgProxy, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	cfgProxy.Services = []*Service{r.proxy.Svc}
	sched := &Scheduler{Mon: r.mon, Agent: client, BootTimeout: time.Second, Log: quiet()}
	admin := &Admin{Cfg: cfgProxy, Mon: r.mon, Agent: client, Activity: r.activity, Stats: r.stats,
		Schedule: sched, WaitTimeout: 3 * time.Second, Version: "test", Log: quiet()}
	adminSrv := httptest.NewServer(admin.Handler())
	t.Cleanup(adminSrv.Close)
	return &agentRig{rig: r, agent: a, client: client, admin: adminSrv}
}

func post(t *testing.T, url, body string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatCarriesActivity(t *testing.T) {
	ar := newAgentRig(t)
	ctx := context.Background()
	end := ar.activity.Begin()
	ar.mon.State(ctx) // sends a heartbeat with one request in progress
	st := ar.agent.Status(ctx)
	if len(st.Sleep.HeldBy) != 1 || st.Sleep.HeldBy[0].Kind != api.HoldRequests {
		t.Fatalf("held by %+v", st.Sleep.HeldBy)
	}
	end()
	ar.mon.State(ctx)
	if st := ar.agent.Status(ctx); len(st.Sleep.HeldBy) != 0 || st.Sleep.InSeconds == nil {
		t.Fatalf("after the request: %+v", st.Sleep)
	}
}

// A request that arrives while the agent announces a sleep calls the sleep off: the
// heartbeat reports it waiting, and it is relayed without a sleep and wake-up cycle.
func TestRequestDuringGraceCallsSleepOff(t *testing.T) {
	ar := newAgentRig(t)
	ctx := context.Background()
	if code, body := post(t, ar.admin.URL+"/api/sleep", `{"action": "suspend"}`, nil); code != 202 {
		t.Fatalf("sleep: %d %s", code, body)
	}
	if st := ar.mon.State(ctx); st != ShuttingDown {
		t.Fatalf("state %s", st)
	}
	go ar.mon.Run(t.Context()) // the heartbeat goes out every 2 s in production
	resp, body := ar.do(t, "POST", "/api/chat", `{"q":1}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `{\"q\":1}`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := ar.target.wakeCount(); n != 0 {
		t.Fatalf("the target slept and was woken %d times", n)
	}
}

// A browser opening a page during the grace period calls the sleep off too.
func TestBrowserDuringGraceCallsSleepOff(t *testing.T) {
	ar := newAgentRig(t)
	ctx := context.Background()
	if code, body := post(t, ar.admin.URL+"/api/sleep", `{"action": "suspend"}`, nil); code != 202 {
		t.Fatalf("sleep: %d %s", code, body)
	}
	if st := ar.mon.State(ctx); st != ShuttingDown {
		t.Fatalf("state %s", st)
	}
	go ar.mon.Run(t.Context())
	resp, _ := ar.do(t, "GET", "/", "", map[string]string{"Accept": "text/html"})
	if resp.StatusCode != 503 {
		t.Fatalf("waiting page: %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return ar.agent.Health().State == api.Ready })
	if n := ar.target.wakeCount(); n != 0 {
		t.Fatalf("the target slept and was woken %d times", n)
	}
}

// Once the agent has gone through with the sleep, a request waits for the target to
// go down, wakes it and is relayed.
func TestRequestAfterSleepWakesTarget(t *testing.T) {
	ar := newAgentRig(t)
	ctx := context.Background()
	if code, body := post(t, ar.admin.URL+"/api/sleep", `{"action": "suspend"}`, nil); code != 202 {
		t.Fatalf("sleep: %d %s", code, body)
	}
	waitFor(t, func() bool { return ar.mon.State(ctx) == Off })
	if ar.mon.Sleep() != "suspend" {
		t.Fatalf("sleep %q", ar.mon.Sleep())
	}
	resp, body := ar.do(t, "POST", "/api/chat", `{"q":1}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `{\"q\":1}`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if ar.target.wakeCount() == 0 {
		t.Fatal("no wake-up")
	}
}

func TestAdminStatusAndActions(t *testing.T) {
	ar := newAgentRig(t)
	var v StatusView
	getJSON(t, ar.admin.URL+"/api/status", &v)
	if v.Target.State != Ready || !v.Target.Agent || len(v.Services) != 1 || v.Services[0].Up == nil ||
		!*v.Services[0].Up || string(v.Agent) == "null" {
		t.Fatalf("status: %+v", v)
	}
	var st api.Status
	if err := json.Unmarshal(v.Agent, &st); err != nil || st.Sleep.IdleMinutes != 30 {
		t.Fatalf("agent status: %s", v.Agent)
	}
	// curl sends no Origin: allowed. A page on another site: refused.
	if code, body := post(t, ar.admin.URL+"/api/idle", `{"minutes": 15}`, nil); code != 200 || !strings.Contains(body, `"idle_minutes":15`) {
		t.Fatalf("idle: %d %s", code, body)
	}
	if code, _ := post(t, ar.admin.URL+"/api/idle", `{"minutes": 10}`, map[string]string{
		"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Fatalf("cross-site action: %d", code)
	}
	if code, body := post(t, ar.admin.URL+"/api/schedule", `{"enabled": true, "days": [], "start": "08:00", "end": "09:00"}`, nil); code != 400 {
		t.Fatalf("bad window: %d %s", code, body)
	}
	if code, _ := post(t, ar.admin.URL+"/api/schedule", `{"enabled": true, "days": [1], "start": "08:00", "end": "09:00"}`, nil); code != 200 {
		t.Fatalf("window: %d", code)
	}

	resp, err := http.Get(ar.admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`wakegate_target_up 1`,
		`wakegate_target_state{state="ready"} 1`,
		`wakegate_requests_total{kind="wake",service="llm"} 0`,
		`wakegate_schedule_enabled 1`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics lacks %q:\n%s", want, raw)
		}
	}

	// Asleep: actions that need the agent are refused, the wake-up is accepted.
	ar.target.set(Off)
	if code, _ := post(t, ar.admin.URL+"/api/stay-awake", `{"minutes": 60}`, nil); code != http.StatusConflict {
		t.Fatalf("stay-awake while asleep: %d", code)
	}
	if code, _ := post(t, ar.admin.URL+"/api/wake", ``, nil); code != http.StatusAccepted {
		t.Fatalf("wake: %d", code)
	}
	waitFor(t, func() bool { return ar.mon.State(context.Background()) == Ready })
}

func TestAdminPage(t *testing.T) {
	ar := newAgentRig(t)
	resp, err := http.Get(ar.admin.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "Awake window") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header)
	}
}

func TestAdminWithoutAgent(t *testing.T) {
	r := newRig(t, Ready, nil, nil)
	cfg, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services = []*Service{r.proxy.Svc}
	admin := &Admin{Cfg: cfg, Mon: r.mon, Activity: r.activity, Stats: r.stats, Log: quiet(), Version: "test",
		Schedule: &Scheduler{Mon: r.mon, Log: quiet()}, WaitTimeout: time.Second}
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()
	var v StatusView
	getJSON(t, srv.URL+"/api/status", &v)
	if v.Target.Agent || string(v.Agent) != "null" || v.Services[0].Up == nil || !*v.Services[0].Up {
		t.Fatalf("status: %+v", v)
	}
	if code, body := post(t, srv.URL+"/api/sleep", `{}`, nil); code != http.StatusNotFound || !strings.Contains(body, "no agent") {
		t.Fatalf("sleep without agent: %d %s", code, body)
	}
}

func TestWakeCommand(t *testing.T) {
	if err := Waker(Target{WakeCommand: []string{"true"}})(); err != nil {
		t.Fatal(err)
	}
	err := Waker(Target{WakeCommand: []string{"sh", "-c", "echo no route >&2; exit 3"}})()
	if err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("a failing command must report its output: %v", err)
	}
}
