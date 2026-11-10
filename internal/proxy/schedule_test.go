package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var paris = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		return time.FixedZone("CET", 3600)
	}
	return loc
}()

// at is a time in the week of Monday 2 November 2026; day 1 is that Monday.
func at(day int, hhmm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-11-0"+string(rune('1'+day))+" "+hhmm, paris)
	return t
}

func TestScheduleWindow(t *testing.T) {
	weekdays := Schedule{Enabled: true, Days: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "23:00"}
	night := Schedule{Enabled: true, Days: []int{5}, Start: "22:00", End: "02:00"}
	for _, c := range []struct {
		name   string
		s      Schedule
		now    time.Time
		inside bool
		end    string
	}{
		{"Monday 7:59", weekdays, at(1, "07:59"), false, ""},
		{"Monday 8:00", weekdays, at(1, "08:00"), true, "2026-11-02 23:00"},
		{"Monday 22:59", weekdays, at(1, "22:59"), true, "2026-11-02 23:00"},
		{"Monday 23:00", weekdays, at(1, "23:00"), false, ""},
		{"Saturday noon", weekdays, at(6, "12:00"), false, ""},
		{"disabled", Schedule{Days: []int{1}, Start: "08:00", End: "23:00"}, at(1, "12:00"), false, ""},
		// Friday 22:00 to 02:00: Saturday 1:00 belongs to it, Saturday 23:00 does not.
		{"after midnight", night, at(6, "01:00"), true, "2026-11-07 02:00"},
		{"the next evening", night, at(6, "23:00"), false, ""},
	} {
		inside, end := c.s.Window(c.now)
		if inside != c.inside {
			t.Errorf("%s: inside %v, want %v", c.name, inside, c.inside)
			continue
		}
		if c.end != "" && end.Format("2006-01-02 15:04") != c.end {
			t.Errorf("%s: ends %s, want %s", c.name, end.Format("2006-01-02 15:04"), c.end)
		}
	}
}

func TestScheduleValidate(t *testing.T) {
	for _, bad := range []Schedule{
		{Enabled: true, Days: []int{8}, Start: "08:00", End: "09:00"},
		{Enabled: true, Days: []int{1}, Start: "8h", End: "09:00"},
		{Enabled: true, Days: []int{1}, Start: "08:00", End: "08:00"},
		{Enabled: true, Start: "08:00", End: "09:00"},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// fakeAgent records the calls the proxy makes.
type fakeAgent struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeAgent) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.Path+" "+string(raw))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"state":"ready"}`)
	})
}

func (f *fakeAgent) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Restarting the proxy inside the window does not wake the target; entering it does.
// While the window is open and the target ready, the agent is asked to stay awake until
// its end, as an extension.
func TestSchedulerWakesOnEntryAndKeepsAwake(t *testing.T) {
	fa := &fakeAgent{}
	srv := httptest.NewServer(fa.handler())
	defer srv.Close()
	ft := &fakeTarget{state: Off}
	mon := testMonitor(ft, ft.wake)
	mon.BootWindow = time.Hour
	agent := &AgentClient{URL: srv.URL, Token: "t", HTTP: http.DefaultClient}
	now := at(1, "07:59")
	s := &Scheduler{Mon: mon, Agent: agent, Store: t.TempDir() + "/proxy.json", BootTimeout: time.Second,
		Log: quiet(), Now: func() time.Time { return now }}
	if err := s.Set(Schedule{Enabled: true, Days: []int{1}, Start: "08:00", End: "23:00"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.Tick(ctx) // start-up, outside the window
	if ft.wakeCount() != 0 {
		t.Fatal("outside the window: no wake-up")
	}
	now = at(1, "08:00")
	s.Tick(ctx)
	waitFor(t, func() bool { return mon.State(ctx) == Ready })
	waitFor(t, func() bool {
		mon.mu.Lock()
		defer mon.mu.Unlock()
		return !mon.inFlight
	})
	s.Tick(ctx)
	calls := fa.got()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "/v1/stay-awake ") {
		t.Fatalf("agent calls: %v", calls)
	}
	var body struct {
		Minutes int  `json:"minutes"`
		Extend  bool `json:"extend"`
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(calls[0], "/v1/stay-awake ")), &body)
	if body.Minutes != 15*60 || !body.Extend {
		t.Fatalf("stay-awake %+v, want 900 minutes as an extension", body)
	}
	s.Tick(ctx)
	if n := len(fa.got()); n != 1 {
		t.Fatalf("sent again within 5 minutes: %d calls", n)
	}

	// A new proxy started inside the window, target off: no wake-up.
	ft.set(Off)
	wakes := ft.wakeCount()
	s2 := &Scheduler{Mon: mon, Agent: agent, Store: s.Store, BootTimeout: time.Second, Log: quiet(),
		Now: func() time.Time { return now }}
	if err := s2.Load(); err != nil || !s2.Get().Enabled {
		t.Fatalf("reloaded: %+v, %v", s2.Get(), err)
	}
	s2.Tick(ctx)
	time.Sleep(20 * time.Millisecond)
	if ft.wakeCount() != wakes {
		t.Fatal("restart inside the window: no wake-up")
	}
}
