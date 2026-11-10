package proxy

import (
	"net/http"

	"github.com/clauer-org/wakegate/internal/promtext"
)

// metrics serves GET /metrics. Everything comes from the proxy itself: scraping it
// never wakes the target, and a sleeping target is not a failing scrape target.
func (a *Admin) metrics(w http.ResponseWriter, r *http.Request) {
	st := a.Mon.State(r.Context())
	var m promtext.Writer
	m.Gauge("wakegate_build_info", "Version of the proxy.").Set(promtext.Labels{"version": a.Version}, 1)
	m.Gauge("wakegate_target_up", "Whether the target answers.").Set(nil, promtext.Bool(st == Ready || st == ShuttingDown))
	states := m.Gauge("wakegate_target_state", "State of the target as the proxy sees it.")
	for _, s := range []State{Off, Waking, Ready, ShuttingDown} {
		states.Set(promtext.Labels{"state": string(s)}, promtext.Bool(st == s))
	}
	m.Gauge("wakegate_active_requests", "Requests and connections holding the target awake.").
		Set(nil, float64(a.Activity.Active()))

	wakes := a.Mon.Wakes()
	ok := m.Counter("wakegate_wakes_total", "Wake-ups that succeeded, by how the target went down.")
	secs := m.Counter("wakegate_wake_seconds_total", "Time from the first wake signal to ready, summed.")
	for from, n := range wakes.OK {
		ok.Set(promtext.Labels{"from": from}, float64(n))
		secs.Set(promtext.Labels{"from": from}, wakes.Seconds[from])
	}
	m.Counter("wakegate_wake_failures_total", "Wake-ups that did not reach ready within boot_timeout.").
		Set(nil, float64(wakes.Failures))

	snap := a.Stats.Snapshot()
	reqs := m.Counter("wakegate_requests_total", "Requests and connections received, by service and kind.")
	woke := m.Counter("wakegate_requests_woke_total", "Requests that waited for a wake-up.")
	errs := m.Counter("wakegate_requests_errors_total", "Requests that failed: 5xx, timeouts, unreachable service.")
	for _, svc := range a.Cfg.Services {
		c := snap.Services[svc.Name]
		for kind, n := range map[string]int{"wake": c.Wake, "other": c.Other, "refused": c.Refused} {
			reqs.Set(promtext.Labels{"service": svc.Name, "kind": kind}, float64(n))
		}
		woke.Set(promtext.Labels{"service": svc.Name}, float64(c.Woke))
		errs.Set(promtext.Labels{"service": svc.Name}, float64(c.Errors))
	}

	active, _ := a.Schedule.Active()
	m.Gauge("wakegate_schedule_enabled", "Whether the awake window is enabled.").
		Set(nil, promtext.Bool(a.Schedule.Get().Enabled))
	m.Gauge("wakegate_schedule_active", "Whether the awake window is open now.").Set(nil, promtext.Bool(active))

	w.Header().Set("Content-Type", promtext.ContentType)
	_, _ = m.WriteTo(w)
}
