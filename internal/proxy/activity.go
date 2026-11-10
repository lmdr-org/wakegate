package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/clauer-org/wakegate/internal/api"
)

// Activity counts the requests and connections that hold the target awake, and when
// the last one ended. The heartbeat carries it to the agent.
type Activity struct {
	mu      sync.Mutex
	active  int
	lastEnd time.Time
}

// Begin counts one request; call the returned function when it ends.
func (a *Activity) Begin() (end func()) {
	a.mu.Lock()
	a.active++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.active--
			a.lastEnd = time.Now()
			a.mu.Unlock()
		})
	}
}

// Active is the number of requests in progress.
func (a *Activity) Active() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

// Heartbeat is what the agent is told.
func (a *Activity) Heartbeat() api.Heartbeat {
	a.mu.Lock()
	defer a.mu.Unlock()
	hb := api.Heartbeat{Active: a.active}
	if !a.lastEnd.IsZero() {
		idle := time.Since(a.lastEnd).Seconds()
		hb.IdleSeconds = &idle
	}
	return hb
}

// AgentClient calls the agent API.
type AgentClient struct {
	URL   string // http://192.168.1.20:9777
	Token string
	HTTP  *http.Client
}

// Do sends body (JSON, unless nil) and returns the status and up to 1 MiB of the answer.
func (c *AgentClient) Do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, err
}

// Forward sends a raw JSON body, as received by the admin API.
func (c *AgentClient) Forward(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var v any
	if len(bytes.TrimSpace(body)) > 0 {
		v = json.RawMessage(body)
	}
	return c.Do(ctx, method, path, v)
}
