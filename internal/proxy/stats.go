package proxy

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// What the proxy did with a request.
const (
	Relayed = "relayed" // relayed to the target
	Refused = "refused" // the target sleeps and the request does not wake it: 503
	Waiting = "waiting" // a browser got the waiting page; the target is waking up
	Timeout = "timeout" // the target or the service did not come up in time
	Failed  = "failed"  // the target is up but the service did not answer
)

// Counts are the counters of one service since the proxy started.
type Counts struct {
	Wake    int        `json:"wake"`    // requests and connections that may wake the target
	Other   int        `json:"other"`   // other requests, relayed
	Refused int        `json:"refused"` // other requests, refused while the target slept
	Woke    int        `json:"woke"`    // requests that had to wait for a wake-up
	Errors  int        `json:"errors"`  // 5xx from the service, timeouts and failures
	Last    *time.Time `json:"last"`
}

// Entry is a recent request. Identical requests in a row (a client polling every two
// seconds) share one entry, with Count.
type Entry struct {
	At         time.Time `json:"at"` // arrival, or end of the last repetition
	Service    string    `json:"service"`
	Method     string    `json:"method"` // "TCP" for a connection
	Path       string    `json:"path"`
	Wakes      bool      `json:"wakes"` // may wake the target
	Client     string    `json:"client"`
	Pending    bool      `json:"pending"`
	Status     int       `json:"status"` // HTTP status; 0 for a connection
	Outcome    string    `json:"outcome"`
	Woke       bool      `json:"woke"` // waited for a wake-up
	DurationMS int64     `json:"duration_ms"`
	Bytes      int64     `json:"bytes,omitempty"` // a connection: both directions
	Count      int       `json:"count"`
}

// Stats counts what arrives on the service ports and keeps the latest requests, target
// asleep or not.
type Stats struct {
	mu       sync.Mutex
	since    time.Time
	keep     int
	services map[string]*Counts
	recent   []*Entry // oldest first
}

// NewStats prepares counters for these services; keep recent requests are kept.
func NewStats(services []*Service, keep int) *Stats {
	s := &Stats{since: time.Now(), keep: keep, services: map[string]*Counts{}}
	for _, svc := range services {
		s.services[svc.Name] = &Counts{}
	}
	return s
}

// Finish ends a request: HTTP status (0 for a connection), outcome, whether it waited
// for a wake-up, bytes relayed (connections).
type Finish func(status int, outcome string, woke bool, bytes int64)

// Begin records a request as it arrives.
func (s *Stats) Begin(service, method, path, client string, wakes bool) Finish {
	if s == nil {
		return func(int, string, bool, int64) {}
	}
	start := time.Now()
	e := &Entry{At: start, Service: service, Method: method, Path: truncate(path, 160), Wakes: wakes,
		Client: client, Pending: true, Count: 1}
	s.mu.Lock()
	s.recent = append(s.recent, e)
	if len(s.recent) > s.keep {
		s.recent = s.recent[len(s.recent)-s.keep:]
	}
	s.mu.Unlock()
	var once sync.Once
	return func(status int, outcome string, woke bool, bytes int64) {
		once.Do(func() { s.finish(e, start, status, outcome, woke, bytes) })
	}
}

func (s *Stats) finish(e *Entry, start time.Time, status int, outcome string, woke bool, bytes int64) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Pending, e.Status, e.Outcome, e.Woke, e.Bytes = false, status, outcome, woke, bytes
	e.DurationMS = now.Sub(start).Milliseconds()
	if c := s.services[e.Service]; c != nil {
		switch {
		case outcome == Refused:
			c.Refused++
		case e.Wakes:
			c.Wake++
		default:
			c.Other++
		}
		if woke {
			c.Woke++
		}
		if outcome == Timeout || outcome == Failed || (outcome == Relayed && status >= 500) {
			c.Errors++
		}
		c.Last = &now
	}
	// A repetition of the request just before it joins that entry.
	for i := len(s.recent) - 1; i > 0; i-- {
		if s.recent[i] != e {
			continue
		}
		prev := s.recent[i-1]
		if !prev.Pending && !e.Woke && !prev.Woke && e.Method != "TCP" && prev.Service == e.Service &&
			prev.Method == e.Method && prev.Path == e.Path && prev.Status == e.Status &&
			prev.Outcome == e.Outcome && prev.Client == e.Client {
			prev.Count++
			prev.At, prev.DurationMS = now, e.DurationMS
			s.recent = append(s.recent[:i], s.recent[i+1:]...)
		}
		break
	}
}

// Snapshot is what the admin API shows.
type Snapshot struct {
	Since    time.Time         `json:"since"`
	Services map[string]Counts `json:"services"`
	Recent   []Entry           `json:"recent"` // newest first
}

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Since: s.since, Services: map[string]Counts{}, Recent: make([]Entry, 0, len(s.recent))}
	for name, c := range s.services {
		snap.Services[name] = *c
	}
	for i := len(s.recent) - 1; i >= 0; i-- {
		snap.Recent = append(snap.Recent, *s.recent[i])
	}
	return snap
}

// clientOf is where the request comes from: behind a reverse proxy, the first
// X-Forwarded-For address. It is shown, never trusted.
func clientOf(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		first, _, _ := strings.Cut(fwd, ",")
		return truncate(strings.TrimSpace(first), 64)
	}
	return hostOf(r.RemoteAddr)
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
