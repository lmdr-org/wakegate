// Package api is the HTTP protocol between the proxy and the agent.
//
// The agent answers GET /healthz without a token. Every other route needs
// "Authorization: Bearer <token>". Request and response bodies are JSON; errors are
// {"error": {"message": "...", "type": "wakegate"}}.
package api

import "time"

// Routes served by the agent.
const (
	PathHealth    = "/healthz"
	PathHeartbeat = "/v1/heartbeat"
	PathStatus    = "/v1/status"
	PathSleep     = "/v1/sleep"
	PathStayAwake = "/v1/stay-awake"
	PathIdle      = "/v1/idle"
	PathRelease   = "/v1/release"
)

// State is the state the agent reports.
type State string

const (
	Ready        State = "ready"
	ShuttingDown State = "shutting-down" // about to suspend, hibernate or power off
)

// What the target does when it goes to sleep.
const (
	Suspend   = "suspend"
	Hibernate = "hibernate"
	PowerOff  = "poweroff"
	None      = "none" // stay on
)

// Health answers GET /healthz and POST /v1/heartbeat: 200 when ready, 503 when shutting
// down.
type Health struct {
	State  State  `json:"state"`
	Action string `json:"action,omitempty"` // while shutting down: suspend, hibernate or poweroff
}

// Heartbeat is what the proxy sends every two seconds.
type Heartbeat struct {
	// Active counts the requests and connections the proxy is relaying that keep the
	// target awake.
	Active int `json:"active"`
	// IdleSeconds is the time since the last of them ended; null if none has since the
	// proxy started.
	IdleSeconds *float64 `json:"idle_seconds"`
}

// Hold is one reason the target does not go to sleep.
type Hold struct {
	Kind   string `json:"kind"`   // see the Hold* constants
	Detail string `json:"detail"` // for people: "2 requests through the proxy", "lmdr (ssh)"
}

// Kinds of Hold.
const (
	HoldRequests  = "requests"   // requests relayed by the proxy
	HoldCommand   = "command"    // a keep_awake command reports work
	HoldInhibitor = "inhibitor"  // a systemd sleep inhibitor
	HoldStayAwake = "stay_awake" // POST /v1/stay-awake
	HoldSession   = "session"    // a logged-in user
)

// Liftable reports whether an explicit request (POST /v1/sleep, POST /v1/release) may
// override a hold of this kind: a session or a stay-awake is not work in progress.
func Liftable(kind string) bool { return kind == HoldSession || kind == HoldStayAwake }

// SleepStatus is when and why the target goes to sleep.
type SleepStatus struct {
	Action         string `json:"action"`          // what happens when idle
	IdleMinutes    int    `json:"idle_minutes"`    // in effect
	DefaultMinutes int    `json:"default_minutes"` // from the configuration
	// InSeconds is the time left before the target goes to sleep, if nothing holds it;
	// null while something does, or when Action is "none".
	InSeconds       *int       `json:"in_seconds"`
	HeldBy          []Hold     `json:"held_by"`
	StayAwakeUntil  *time.Time `json:"stay_awake_until"`
	IgnoredSessions int        `json:"ignored_sessions"` // sessions a release set aside
}

// Memory is the target's memory, in bytes.
type Memory struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
}

// Status answers GET /v1/status.
type Status struct {
	Version    string      `json:"version"`
	Hostname   string      `json:"hostname"`
	State      State       `json:"state"`
	Action     string      `json:"action,omitempty"`
	AwakeSince time.Time   `json:"awake_since"` // boot, or the last resume from sleep
	Memory     *Memory     `json:"memory"`
	Sleep      SleepStatus `json:"sleep"`
}

// SleepRequest is the body of POST /v1/sleep. Without an action, the configured one is
// used ("suspend" if it is "none").
type SleepRequest struct {
	Action string `json:"action"`
}

// StayAwakeRequest is the body of POST /v1/stay-awake. Minutes 0 cancels. With Extend,
// an earlier end is never moved closer.
type StayAwakeRequest struct {
	Minutes int  `json:"minutes"`
	Extend  bool `json:"extend"`
}

// IdleRequest is the body of POST /v1/idle: minutes without requests before sleep, 0 for
// the configured value.
type IdleRequest struct {
	Minutes int `json:"minutes"`
}

// ReleaseResponse answers POST /v1/release.
type ReleaseResponse struct {
	IgnoredSessions    int         `json:"ignored_sessions"`
	StayAwakeCancelled bool        `json:"stay_awake_cancelled"`
	Sleep              SleepStatus `json:"sleep"`
}

// Error is the body of every error response, from the agent and the proxy alike. Its
// shape is the one OpenAI-compatible clients display.
type Error struct {
	Error  ErrorBody `json:"error"`
	HeldBy []Hold    `json:"held_by,omitempty"` // POST /v1/sleep refused with 409
}

// ErrorBody says what went wrong.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"` // always "wakegate"
}

// NewError builds an error body.
func NewError(msg string) Error { return Error{Error: ErrorBody{Message: msg, Type: "wakegate"}} }
