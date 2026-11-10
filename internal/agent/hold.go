package agent

import (
	"fmt"
	"time"

	"github.com/clauer-org/wakegate/internal/api"
)

// input is everything the idle decision depends on.
type input struct {
	now            time.Time
	idleSince      time.Time // last request, change of timeout, or wake-up: the latest
	idleAfter      time.Duration
	active         int // requests through the proxy, from a recent heartbeat
	commands       []commandResult
	inhibitors     []Inhibitor // the ones blocking the action
	inhibitorErr   error
	stayAwakeUntil time.Time
	sessions       []Session // the ones holding the computer
	sessionErr     error
}

type commandResult struct {
	command string
	busy    bool
	err     error
}

// verdict is the outcome: what holds the computer, and the time left once nothing does.
type verdict struct {
	holds     []api.Hold
	remaining time.Duration
}

// idle reports whether the computer should go to sleep now.
func (v verdict) idle() bool { return len(v.holds) == 0 && v.remaining == 0 }

// blocking lists the holds an explicit sleep request does not override: work in progress.
func (v verdict) blocking() []api.Hold {
	var out []api.Hold
	for _, h := range v.holds {
		if !api.Liftable(h.Kind) {
			out = append(out, h)
		}
	}
	return out
}

func evaluate(in input) verdict {
	var v verdict
	hold := func(kind, format string, args ...any) {
		v.holds = append(v.holds, api.Hold{Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}
	switch {
	case in.active == 1:
		hold(api.HoldRequests, "1 request through the proxy")
	case in.active > 1:
		hold(api.HoldRequests, "%d requests through the proxy", in.active)
	}
	for _, c := range in.commands {
		switch {
		case c.err != nil:
			// A broken check holds the computer: better awake for nothing than asleep
			// in the middle of a backup.
			hold(api.HoldCommand, "%s: check failed: %v", c.command, c.err)
		case c.busy:
			hold(api.HoldCommand, "%s", c.command)
		}
	}
	if in.inhibitorErr != nil {
		hold(api.HoldInhibitor, "inhibitors unknown: %v", in.inhibitorErr)
	}
	for _, i := range in.inhibitors {
		hold(api.HoldInhibitor, "%s: %s", i.Who, i.Why)
	}
	if in.now.Before(in.stayAwakeUntil) {
		hold(api.HoldStayAwake, "stay awake until %s", in.stayAwakeUntil.Format("15:04"))
	}
	if in.sessionErr != nil {
		hold(api.HoldSession, "sessions unknown: %v", in.sessionErr)
	}
	for _, s := range in.sessions {
		hold(api.HoldSession, "%s", s.Describe())
	}
	v.remaining = max(in.idleAfter-in.now.Sub(in.idleSince), 0)
	return v
}
