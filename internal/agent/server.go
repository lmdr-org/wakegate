package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lmdr-org/wakegate/internal/api"
)

// Handler serves the agent API. Every route but GET /healthz needs the token.
func (a *Agent) Handler(token string) http.Handler {
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeError(w, http.StatusUnauthorized, api.NewError("missing or wrong token"))
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, a.Health())
	})
	mux.HandleFunc("POST "+api.PathHeartbeat, auth(a.serveHeartbeat))
	mux.HandleFunc("GET "+api.PathStatus, auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.Status(r.Context()))
	}))
	mux.HandleFunc("POST "+api.PathSleep, auth(a.serveSleep))
	mux.HandleFunc("POST "+api.PathStayAwake, auth(a.serveStayAwake))
	mux.HandleFunc("POST "+api.PathIdle, auth(a.serveIdle))
	mux.HandleFunc("POST "+api.PathRelease, auth(a.serveRelease))
	return mux
}

func (a *Agent) serveHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb api.Heartbeat
	if !decode(w, r, &hb, false) {
		return
	}
	writeHealth(w, a.Heartbeat(hb))
}

func (a *Agent) serveSleep(w http.ResponseWriter, r *http.Request) {
	var req api.SleepRequest
	if !decode(w, r, &req, true) {
		return
	}
	if req.Action == "" {
		req.Action = a.cfg.Sleep
		if req.Action == api.None {
			req.Action = api.Suspend
		}
	}
	switch req.Action {
	case api.Suspend, api.Hibernate, api.PowerOff:
	default:
		writeError(w, http.StatusBadRequest, api.NewError(`action: "suspend", "hibernate" or "poweroff"`))
		return
	}
	holds, err := a.RequestSleep(r.Context(), req.Action)
	switch {
	case errors.Is(err, ErrBusy):
		e := api.NewError(err.Error())
		e.HeldBy = holds
		writeError(w, http.StatusConflict, e)
	case err != nil:
		writeError(w, http.StatusConflict, api.NewError(err.Error()))
	default:
		writeJSON(w, http.StatusAccepted, a.Health())
	}
}

func (a *Agent) serveStayAwake(w http.ResponseWriter, r *http.Request) {
	var req api.StayAwakeRequest
	if !decode(w, r, &req, false) {
		return
	}
	if err := a.StayAwake(time.Duration(req.Minutes)*time.Minute, req.Extend); err != nil {
		writeError(w, http.StatusBadRequest, api.NewError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, a.sleepStatus(r.Context()))
}

func (a *Agent) serveIdle(w http.ResponseWriter, r *http.Request) {
	var req api.IdleRequest
	if !decode(w, r, &req, false) {
		return
	}
	if err := a.SetIdleTimeout(req.Minutes); err != nil {
		writeError(w, http.StatusBadRequest, api.NewError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, a.sleepStatus(r.Context()))
}

func (a *Agent) serveRelease(w http.ResponseWriter, r *http.Request) {
	resp, err := a.Release(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, api.NewError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decode reads a JSON body into v; unknown fields are errors. With optional, an empty
// body is accepted.
func decode(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil || (optional && errors.Is(err, io.EOF)) {
		return true
	}
	writeError(w, http.StatusBadRequest, api.NewError("body: "+err.Error()))
	return false
}

func writeHealth(w http.ResponseWriter, h api.Health) {
	status := http.StatusOK
	if h.State != api.Ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, h)
}

func writeError(w http.ResponseWriter, status int, e api.Error) {
	writeJSON(w, status, e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
