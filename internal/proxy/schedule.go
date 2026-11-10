package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/lmdr-org/wakegate/internal/api"
	"github.com/lmdr-org/wakegate/internal/jsonfile"
)

// Schedule is the awake window: on the chosen days, from Start to End, the target is
// woken at the start and kept awake until the end. A window that ends before it starts
// (22:00 to 02:00) runs past midnight and belongs to the day it starts.
type Schedule struct {
	Enabled bool   `json:"enabled"`
	Days    []int  `json:"days"`  // 1 = Monday … 7 = Sunday
	Start   string `json:"start"` // "08:00"
	End     string `json:"end"`   // "23:00"
}

// Validate checks the days and times.
func (s Schedule) Validate() error {
	for _, d := range s.Days {
		if d < 1 || d > 7 {
			return fmt.Errorf("day %d: from 1 (Monday) to 7 (Sunday)", d)
		}
	}
	start, err := clockMinutes(s.Start)
	if err != nil {
		return err
	}
	end, err := clockMinutes(s.End)
	if err != nil {
		return err
	}
	if start == end {
		return fmt.Errorf("empty window: %s to %s", s.Start, s.End)
	}
	if s.Enabled && len(s.Days) == 0 {
		return fmt.Errorf("an enabled window needs at least one day")
	}
	return nil
}

func clockMinutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("time %q: HH:MM", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// isoDay: 1 = Monday … 7 = Sunday.
func isoDay(t time.Time) int {
	if d := int(t.Weekday()); d != 0 {
		return d
	}
	return 7
}

// Window reports whether now falls in the window, and when it ends.
func (s Schedule) Window(now time.Time) (inside bool, end time.Time) {
	if !s.Enabled || s.Validate() != nil {
		return false, time.Time{}
	}
	start, _ := clockMinutes(s.Start)
	stop, _ := clockMinutes(s.End)
	// Today's window, then yesterday's, which may run past midnight.
	for _, back := range []int{0, 1} {
		day := now.AddDate(0, 0, -back)
		if !slices.Contains(s.Days, isoDay(day)) {
			continue
		}
		midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, now.Location())
		from := midnight.Add(time.Duration(start) * time.Minute)
		to := midnight.Add(time.Duration(stop) * time.Minute)
		if stop < start {
			to = to.AddDate(0, 0, 1)
		}
		if !now.Before(from) && now.Before(to) {
			return true, to
		}
	}
	return false, time.Time{}
}

// Scheduler applies the awake window: a wake-up when it starts, then a stay-awake until
// its end while the target answers. Only entering the window wakes the target: a sleep
// requested during the window is respected, and restarting the proxy wakes nothing.
type Scheduler struct {
	Mon         *Monitor
	Agent       *AgentClient // nil without agent: the window only wakes the target
	Store       string       // settings file; "" keeps nothing
	BootTimeout time.Duration
	Log         *slog.Logger
	Now         func() time.Time // time.Now by default

	mu         sync.Mutex
	sched      Schedule
	wasInside  bool
	pushedEnd  time.Time // end already sent to the agent…
	pushedAt   time.Time // …at this time
	lastTarget State
	started    bool
	pushPeriod time.Duration // 5 min by default
}

type proxySettings struct {
	Schedule  Schedule  `json:"schedule"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Load reads the saved window back.
func (s *Scheduler) Load() error {
	v, err := jsonfile.Load[proxySettings](s.Store)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sched = v.Schedule
	s.mu.Unlock()
	return nil
}

// Get returns the window in effect.
func (s *Scheduler) Get() Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sched
}

// Set changes the window and saves it. Setting a window that has already started does
// not wake the target: only its start time does.
func (s *Scheduler) Set(v Schedule) error {
	if err := v.Validate(); err != nil {
		return err
	}
	if err := jsonfile.Save(s.Store, proxySettings{Schedule: v, UpdatedAt: s.now()}); err != nil {
		return fmt.Errorf("window not saved: %w", err)
	}
	s.mu.Lock()
	s.sched = v
	s.wasInside, _ = v.Window(s.now())
	s.pushedEnd = time.Time{}
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Active reports whether the window is open, and its end.
func (s *Scheduler) Active() (bool, time.Time) {
	return s.Get().Window(s.now())
}

// Run applies the window every 30 seconds until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick wakes the target when the window opens and keeps it awake while it is open.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	inside, end := s.sched.Window(now)
	entering := inside && !s.wasInside && s.started
	s.started = true // the first tick, at start-up, wakes nothing
	s.wasInside = inside
	s.mu.Unlock()
	if entering {
		s.Log.Info("awake window: waking the target", "until", end)
		s.Mon.WakeSoon(s.BootTimeout)
	}
	if s.Agent == nil {
		return
	}
	target := s.Mon.State(ctx)
	s.mu.Lock()
	period := s.pushPeriod
	if period == 0 {
		period = 5 * time.Minute
	}
	becameReady := target == Ready && s.lastTarget != Ready
	s.lastTarget = target
	push := inside && target == Ready &&
		(becameReady || !s.pushedEnd.Equal(end) || now.Sub(s.pushedAt) >= period)
	s.mu.Unlock()
	if !push {
		return
	}
	minutes := int(math.Ceil(end.Sub(now).Minutes()))
	code, raw, err := s.Agent.Do(ctx, http.MethodPost, api.PathStayAwake, api.StayAwakeRequest{Minutes: minutes, Extend: true})
	if err != nil || code != http.StatusOK {
		s.Log.Warn("awake window: the agent refused the stay-awake", "status", code, "err", err, "body", string(raw))
		return
	}
	s.mu.Lock()
	if !s.pushedEnd.Equal(end) {
		s.Log.Info("awake window: the target stays awake", "until", end)
	}
	s.pushedEnd, s.pushedAt = end, now
	s.mu.Unlock()
}
