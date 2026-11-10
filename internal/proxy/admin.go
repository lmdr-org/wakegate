package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	_ "embed"

	"github.com/lmdr-org/wakegate/internal/api"
)

// Admin serves the admin page and its API.
//
// The page has no login: whoever reaches the admin port can wake the target and put it
// to sleep. Reads are open; actions (POST) are refused when a browser sends them from
// another site.
type Admin struct {
	Cfg         *Config
	Mon         *Monitor
	Agent       *AgentClient // nil without agent
	Activity    *Activity
	Stats       *Stats
	Schedule    *Scheduler
	WaitTimeout time.Duration
	Version     string
	Log         *slog.Logger
}

//go:embed web/admin.html
var adminHTML []byte

// Handler routes the admin API.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
		_, _ = w.Write(adminHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"proxy": "ok", "target": a.Mon.State(r.Context())})
	})
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /api/schedule", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, a.scheduleView())
	})
	mux.HandleFunc("POST /api/schedule", a.setSchedule)
	mux.HandleFunc("POST /api/wake", a.wake)
	mux.HandleFunc("POST /api/sleep", a.forward(api.PathSleep))
	mux.HandleFunc("POST /api/stay-awake", a.forward(api.PathStayAwake))
	mux.HandleFunc("POST /api/idle", a.forward(api.PathIdle))
	mux.HandleFunc("POST /api/release", a.forward(api.PathRelease))
	// Refuses a POST that a browser sends from another site (Sec-Fetch-Site, Origin);
	// curl and scripts send neither and go through.
	return http.NewCrossOriginProtection().Handler(mux)
}

// TargetView is the target as /api/status shows it.
type TargetView struct {
	Name  string `json:"name"`
	Host  string `json:"host"`
	MAC   string `json:"mac,omitempty"`
	State State  `json:"state"`
	// Sleep is how the target went down (suspend, hibernate, poweroff) while it does not
	// answer, if the proxy saw it go.
	Sleep  *string `json:"sleep"`
	Agent  bool    `json:"agent"`  // an agent manages its sleep
	Active int     `json:"active"` // requests and connections holding it awake
}

// ServiceView is a service as /api/status shows it.
type ServiceView struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"` // http or tcp
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	Up       *bool  `json:"up"` // null while the target does not answer
	Counts
}

// StatusView answers GET /api/status.
type StatusView struct {
	Version  string          `json:"version"`
	Target   TargetView      `json:"target"`
	Agent    json.RawMessage `json:"agent"` // the agent's GET /v1/status, null when it does not answer
	Services []ServiceView   `json:"services"`
	Since    time.Time       `json:"since"`
	Recent   []Entry         `json:"recent"`
	Wakes    Wakes           `json:"wakes"`
	Schedule ScheduleView    `json:"schedule"`
}

// ScheduleView is the awake window and whether it is open.
type ScheduleView struct {
	Schedule
	Active bool       `json:"active"`
	Until  *time.Time `json:"until"`
}

func (a *Admin) status(w http.ResponseWriter, r *http.Request) {
	st := a.Mon.State(r.Context())
	snap := a.Stats.Snapshot()
	v := StatusView{Version: a.Version, Since: snap.Since, Recent: snap.Recent, Wakes: a.Mon.Wakes(),
		Schedule: a.scheduleView(), Agent: json.RawMessage("null"),
		Target: TargetView{Name: a.Cfg.Target.Name, Host: a.Cfg.Target.Host, MAC: a.Cfg.Target.MAC, State: st,
			Agent: a.Agent != nil, Active: a.Activity.Active()}}
	if s := a.Mon.Sleep(); s != "" {
		v.Target.Sleep = &s
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	if a.Agent != nil && (st == Ready || st == ShuttingDown) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Short: a target going to sleep accepts the connection, then freezes.
			if code, raw, err := a.Agent.Do(ctx, http.MethodGet, api.PathStatus, nil); err == nil && code == http.StatusOK {
				v.Agent = raw
			}
		}()
	}
	v.Services = make([]ServiceView, len(a.Cfg.Services))
	for i, svc := range a.Cfg.Services {
		mode := "http"
		if svc.TCP {
			mode = "tcp"
		}
		v.Services[i] = ServiceView{Name: svc.Name, Mode: mode, Listen: svc.Listen, Upstream: svc.Addr(),
			Counts: snap.Services[svc.Name]}
		if st != Ready {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			up := serviceUp(ctx, svc)
			v.Services[i].Up = &up
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, v)
}

// serviceUp checks the health path if there is one, else that the port accepts
// connections.
func serviceUp(ctx context.Context, svc *Service) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if svc.Health == "" {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", svc.Addr())
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+svc.Addr()+svc.Health, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (a *Admin) scheduleView() ScheduleView {
	v := ScheduleView{Schedule: a.Schedule.Get()}
	if v.Days == nil {
		v.Days = []int{}
	}
	if active, end := a.Schedule.Active(); active {
		v.Active, v.Until = true, &end
	}
	return v
}

// setSchedule sets the awake window: {"enabled": true, "days": [1,2,3,4,5],
// "start": "08:00", "end": "23:00"}.
func (a *Admin) setSchedule(w http.ResponseWriter, r *http.Request) {
	var v Schedule
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		writeError(w, http.StatusBadRequest, 0, `expected {"enabled": true, "days": [1,2,3,4,5], "start": "08:00", "end": "23:00"}`)
		return
	}
	if err := a.Schedule.Set(v); err != nil {
		writeError(w, http.StatusBadRequest, 0, err.Error())
		return
	}
	a.Log.Info("awake window", "enabled", v.Enabled, "days", v.Days, "start", v.Start, "end", v.End)
	writeJSON(w, http.StatusOK, a.scheduleView())
}

func (a *Admin) wake(w http.ResponseWriter, r *http.Request) {
	st := a.Mon.State(r.Context())
	if st != Ready {
		a.Mon.WakeSoon(a.WaitTimeout)
		a.Log.Info("wake-up requested", "target", st)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"target": st})
}

// forward relays an action to the agent, while the target is ready.
func (a *Admin) forward(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Agent == nil {
			writeError(w, http.StatusNotFound, 0, "no agent: the target manages its own sleep")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, 0, "unreadable body")
			return
		}
		if st := a.Mon.State(r.Context()); st != Ready {
			writeError(w, http.StatusConflict, 0, "the target is not awake ("+string(st)+")")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		code, raw, err := a.Agent.Forward(ctx, http.MethodPost, path, body)
		if err != nil {
			a.Mon.Invalidate()
			writeError(w, http.StatusBadGateway, 0, "the agent did not answer: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_, _ = w.Write(raw)
	}
}
