package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var ollamaWakeOn = []string{"POST /api/generate", "POST /api/chat", "POST /v1/*"}

func TestPassiveRequestDoesNotWake(t *testing.T) {
	r := newRig(t, Off, ollamaWakeOn, nil)
	for _, path := range []string{"/api/tags", "/api/version"} {
		resp, body := r.do(t, "GET", path, "", nil)
		if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "60" || !strings.Contains(body, "asleep") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	if n := r.target.wakeCount(); n != 0 {
		t.Fatalf("%d wake signals for passive requests", n)
	}
	if c := r.settled(t).Services["llm"]; c.Refused != 2 || c.Wake != 0 {
		t.Fatalf("counts: %+v", c)
	}
}

func TestPassiveRequestRelayedWhenAwake(t *testing.T) {
	r := newRig(t, Ready, ollamaWakeOn, nil)
	resp, body := r.do(t, "GET", "/api/tags", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"/api/tags"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	r.settled(t)
	if hb := r.activity.Heartbeat(); hb.IdleSeconds != nil {
		t.Fatal("a passive request must not count as activity")
	}
}

func TestWakingRequestIsHeldThenRelayed(t *testing.T) {
	r := newRig(t, Off, ollamaWakeOn, nil)
	resp, body := r.do(t, "POST", "/v1/chat/completions", `{"model":"m"}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `{\"model\":\"m\"}`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if r.target.wakeCount() == 0 {
		t.Fatal("no wake signal")
	}
	snap := r.settled(t)
	if c := snap.Services["llm"]; c.Wake != 1 || c.Woke != 1 {
		t.Fatalf("counts: %+v", c)
	}
	if e := snap.Recent[0]; !e.Woke || e.Status != 200 || e.Outcome != Relayed {
		t.Fatalf("entry: %+v", e)
	}
	if hb := r.activity.Heartbeat(); hb.Active != 0 || hb.IdleSeconds == nil {
		t.Fatalf("heartbeat after the request: %+v", hb)
	}
}

func TestEveryRequestWakesWithoutRules(t *testing.T) {
	r := newRig(t, Off, nil, []string{"GET /health"})
	if resp, _ := r.do(t, "GET", "/health", "", nil); resp.StatusCode != 503 {
		t.Fatalf("never_wake: %d", resp.StatusCode)
	}
	if resp, body := r.do(t, "PUT", "/bucket/key", "data", nil); resp.StatusCode != 200 || !strings.Contains(body, "data") {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
}

func TestBrowserGetsWaitingPage(t *testing.T) {
	r := newRig(t, Off, ollamaWakeOn, nil)
	resp, body := r.do(t, "GET", "/", "", map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if resp.StatusCode != 503 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "tower is waking up") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	waitFor(t, func() bool { return r.mon.State(context.Background()) == Ready })
	// Once awake, the page is relayed.
	resp, body = r.do(t, "GET", "/", "", map[string]string{"Accept": "text/html"})
	if resp.StatusCode != 200 || !strings.Contains(body, `"path":"/"`) {
		t.Fatalf("awake: %d %s", resp.StatusCode, body)
	}
	// A WebSocket is never a page.
	r.target.set(Off)
	resp, _ = r.do(t, "GET", "/ws", "", map[string]string{"Accept": "text/html", "Upgrade": "websocket", "Connection": "Upgrade"})
	if resp.StatusCode != 503 || strings.Contains(resp.Header.Get("Content-Type"), "html") {
		t.Fatalf("websocket: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestRequestReplayedWhenTargetFellAsleep(t *testing.T) {
	r := newRig(t, Ready, ollamaWakeOn, nil)
	r.target.mu.Lock()
	r.target.sleepOnNext = true
	r.target.mu.Unlock()
	resp, body := r.do(t, "POST", "/api/chat", `{"q":2}`, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `{\"q\":2}`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if got := r.target.received(); len(got) != 1 || got[0] != `POST /api/chat {"q":2}` {
		t.Fatalf("service received %q", got)
	}
	if r.target.wakeCount() == 0 {
		t.Fatal("the target was not woken again")
	}
}

func TestLargeBodyIsStreamedOnce(t *testing.T) {
	r := newRig(t, Off, nil, nil)
	big := strings.Repeat("x", 4<<10) // MaxBody is 1 KiB
	resp, body := r.do(t, "POST", "/upload", big, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, big) {
		t.Fatalf("%d, %d bytes", resp.StatusCode, len(body))
	}
	r.target.mu.Lock()
	r.target.sleepOnNext = true
	r.target.mu.Unlock()
	if resp, body := r.do(t, "POST", "/upload", big, nil); resp.StatusCode != 502 || !strings.Contains(body, "not sent again") {
		t.Fatalf("failure: %d %s", resp.StatusCode, body)
	}
}

func TestTargetThatDoesNotWakeTimesOut(t *testing.T) {
	ft := &fakeTarget{state: Off}
	sent := 0
	mon := testMonitor(ft, func() error { sent++; return nil }) // the target never boots
	svc := testService(t, "llm", freeAddr(t), nil, nil)
	stats := NewStats([]*Service{svc}, 10)
	p := NewHTTPProxy(&HTTPProxy{Svc: svc, Target: "tower", Mon: mon, Activity: &Activity{}, Stats: stats,
		BootTimeout: 100 * time.Millisecond, WaitTimeout: 200 * time.Millisecond, MaxBody: 1 << 10, Log: quiet()})
	front := httptest.NewServer(p)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 504 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if sent < 2 {
		t.Fatalf("%d wake signals; it must be sent again", sent)
	}
	waitFor(t, func() bool { return !stats.Snapshot().Recent[0].Pending })
	if c := stats.Snapshot().Services["llm"]; c.Errors != 1 {
		t.Fatalf("counts: %+v", c)
	}
}

func TestShuttingDownHoldsThenWakes(t *testing.T) {
	r := newRig(t, ShuttingDown, ollamaWakeOn, nil)
	r.target.action = "suspend"
	r.mon.State(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.target.set(Off)
	}()
	resp, _ := r.do(t, "POST", "/api/generate", `{}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if w := r.mon.Wakes(); w.OK["suspend"] != 1 {
		t.Fatalf("wakes: %+v", w)
	}
}

func TestExplicitWake(t *testing.T) {
	r := newRig(t, Off, ollamaWakeOn, nil)
	resp, body := r.do(t, "POST", WakePath, "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"service":"llm"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if got := r.target.received(); len(got) != 0 {
		t.Fatalf("/__wake reached the service: %v", got)
	}
}

func TestHealthCheckedAfterWake(t *testing.T) {
	ft := &fakeTarget{state: Off}
	healthy := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if !healthy {
				healthy = true // the first check finds the service still starting
				w.WriteHeader(503)
			}
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	svc := testService(t, "llm", up.Listener.Addr().String(), nil, nil)
	svc.Health = "/health"
	p := NewHTTPProxy(&HTTPProxy{Svc: svc, Target: "tower", Mon: testMonitor(ft, ft.wake), Activity: &Activity{},
		Stats: NewStats([]*Service{svc}, 10), BootTimeout: 2 * time.Second, WaitTimeout: 3 * time.Second,
		MaxBody: 1 << 10, Log: quiet()})
	front := httptest.NewServer(p)
	defer front.Close()
	start := time.Now()
	resp, err := http.Post(front.URL+"/x", "text/plain", strings.NewReader("a"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || time.Since(start) < time.Second {
		t.Fatalf("status %d after %s: the health check must be retried", resp.StatusCode, time.Since(start))
	}
}

func TestRepeatedRequestsShareAnEntry(t *testing.T) {
	r := newRig(t, Off, ollamaWakeOn, nil)
	for range 3 {
		r.do(t, "GET", "/api/tags", "", nil)
		r.settled(t)
	}
	snap := r.stats.Snapshot()
	if len(snap.Recent) != 1 || snap.Recent[0].Count != 3 {
		t.Fatalf("recent: %+v", snap.Recent)
	}
}

func TestTCPConnectionWaitsForTarget(t *testing.T) {
	ft := &fakeTarget{state: Off}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte("SSH-2.0-test\r\n"))
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	svc := &Service{Name: "ssh", Listen: ":1", Upstream: echo.Addr().String(), TCP: true}
	if err := svc.complete("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	activity := &Activity{}
	stats := NewStats([]*Service{svc}, 10)
	p := &TCPProxy{Svc: svc, Mon: testMonitor(ft, ft.wake), Activity: activity, Stats: stats,
		BootTimeout: 2 * time.Second, WaitTimeout: 3 * time.Second, Log: quiet()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = p.Serve(context.Background(), ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	banner, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || banner != "SSH-2.0-test\r\n" {
		t.Fatalf("banner %q, %v", banner, err)
	}
	if ft.wakeCount() == 0 {
		t.Fatal("the connection did not wake the target")
	}
	if activity.Active() != 1 {
		t.Fatalf("an open connection holds the target: active %d", activity.Active())
	}
	_, _ = c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q, %v", buf, err)
	}
	c.Close()
	waitFor(t, func() bool { return activity.Active() == 0 })
	waitFor(t, func() bool { return !stats.Snapshot().Recent[0].Pending })
	e := stats.Snapshot().Recent[0]
	if e.Method != "TCP" || !e.Woke || e.Outcome != Relayed || e.Bytes != int64(len("SSH-2.0-test\r\n")+8) {
		t.Fatalf("entry: %+v", e)
	}
}

func TestMonitorRemembersHowTheTargetWentDown(t *testing.T) {
	ft := &fakeTarget{state: ShuttingDown, action: "hibernate"}
	mon := testMonitor(ft, ft.wake)
	mon.State(context.Background())
	ft.set(Off)
	if st := mon.State(context.Background()); st != Off || mon.Sleep() != "hibernate" {
		t.Fatalf("state %s, sleep %q", st, mon.Sleep())
	}
	mon.SendWake()
	if st := mon.State(context.Background()); st != Waking {
		t.Fatalf("after the wake signal: %s", st)
	}
	if err := mon.WaitReady(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if mon.Sleep() != "" || mon.Wakes().OK["hibernate"] != 1 || mon.Wakes().Last == nil {
		t.Fatalf("wakes: %+v", mon.Wakes())
	}
}
