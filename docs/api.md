# API

Both sides speak JSON over HTTP. Errors have one shape:

```json
{"error": {"message": "the target is not awake (off)", "type": "wakegate"}}
```

## Proxy admin API

Served on `admin.listen`, with the admin page at `/`. There is no login: reads are open,
and actions (`POST`) are refused when a browser sends them from another site. Scripts
such as `curl` send no `Origin` header and are not affected.

| Method | Path | |
|---|---|---|
| `GET` | `/healthz` | `{"proxy": "ok", "target": "<state>"}` |
| `GET` | `/api/status` | Everything the admin page shows, below. |
| `GET` | `/metrics` | Prometheus metrics. |
| `POST` | `/api/wake` | Wake the target in the background: `202 {"target": "<state>"}`. |
| `POST` | `/api/sleep` | `{"action": "suspend"}` (or `hibernate`, `poweroff`; the agent's `sleep` setting if left out). |
| `POST` | `/api/stay-awake` | `{"minutes": 120}`; `0` cancels. |
| `POST` | `/api/idle` | `{"minutes": 15}`: the idle timeout; `0` restores the configured one. |
| `POST` | `/api/release` | Set the open sessions aside and cancel the stay-awake. |
| `GET` | `/api/schedule` | The awake window. |
| `POST` | `/api/schedule` | `{"enabled": true, "days": [1, 2, 3, 4, 5], "start": "08:00", "end": "23:00"}`; 1 is Monday. |

`sleep`, `stay-awake`, `idle` and `release` go to the agent: they answer `404` without
an agent, `409` while the target is not awake, and otherwise what the agent answers.

```sh
curl -s http://raspberrypi:9778/api/status | jq .target
curl -s -X POST http://raspberrypi:9778/api/stay-awake -d '{"minutes": 240}'
curl -s -X POST http://raspberrypi:9778/api/sleep -d '{"action": "suspend"}'
```

### States

| `target.state` | |
|---|---|
| `off` | The target does not answer. `target.sleep` says how it went down, if the proxy saw it: `suspend`, `hibernate` or `poweroff`. |
| `waking` | It does not answer yet; a wake-up was sent less than `boot_timeout` ago. |
| `ready` | It answers. |
| `shutting-down` | The agent is putting it to sleep. |

### `GET /api/status`

```json
{
  "version": "v0.1.0",
  "target": {"name": "tower", "host": "192.168.1.20", "mac": "04:d9:f5:cb:75:23",
             "state": "ready", "sleep": null, "agent": true, "active": 1},
  "agent": { "…": "the agent's GET /v1/status, or null when it does not answer" },
  "services": [
    {"name": "ollama", "mode": "http", "listen": ":11434", "upstream": "192.168.1.20:11434",
     "up": true, "wake": 12, "other": 340, "refused": 51, "woke": 3, "errors": 0,
     "last": "2026-11-10T16:34:00+01:00"}
  ],
  "since": "2026-11-10T08:00:00+01:00",
  "recent": [
    {"at": "2026-11-10T16:34:00+01:00", "service": "ollama", "method": "POST", "path": "/api/chat",
     "wakes": true, "client": "192.168.1.31", "pending": false, "status": 200, "outcome": "relayed",
     "woke": true, "duration_ms": 6900, "count": 1}
  ],
  "wakes": {"ok": {"suspend": 3}, "seconds": {"suspend": 14.2}, "failures": 0,
            "last": {"from": "suspend", "seconds": 4.6, "at": "2026-11-10T16:33:54+01:00"}},
  "schedule": {"enabled": false, "days": [], "start": "", "end": "", "active": false, "until": null}
}
```

- `target.active`: requests and connections holding the target awake, waits included.
- `services[].up`: whether the service answers (its `health` path, or its port); `null`
  while the target does not answer. The counters run from `since`, the proxy's start:
  `wake` counts requests that may wake the target, `other` the other ones relayed,
  `refused` the other ones refused while it slept, `woke` the requests that waited for a
  wake-up, `errors` the `5xx`, timeouts and failures.
- `recent`: the last 50 requests, newest first. `outcome` is `relayed`, `refused` (asleep,
  not woken), `waiting` (a browser got the waiting page), `timeout` or `failed`. A
  connection has `"method": "TCP"`, `status` 0 and `bytes`. Identical requests in a row
  share one entry, with `count`. `status` 499 means the client left before the answer.
- `wakes`: successful wake-ups by how the target went down (`unknown` if the proxy did not
  see it go), with their total and last duration, from the first wake signal to ready.

### Metrics

| Metric | |
|---|---|
| `wakegate_target_up` | 1 while the target answers. |
| `wakegate_target_state{state}` | 1 for the current state. |
| `wakegate_active_requests` | Requests and connections holding the target awake. |
| `wakegate_wakes_total{from}`, `wakegate_wake_seconds_total{from}` | Successful wake-ups and their duration. |
| `wakegate_wake_failures_total` | Wake-ups that did not reach ready within `boot_timeout`. |
| `wakegate_requests_total{service,kind}` | `kind`: `wake`, `other`, `refused`. |
| `wakegate_requests_woke_total{service}`, `wakegate_requests_errors_total{service}` | |
| `wakegate_schedule_enabled`, `wakegate_schedule_active` | The awake window. |
| `wakegate_build_info{version}` | |

They come from the proxy alone: scraping it never wakes the target, and a sleeping target
is not a failing scrape. Counters start from zero when the proxy restarts.

## Agent API

Served on the agent's `listen`. Every route except `GET /healthz` needs
`Authorization: Bearer <token>`.

| Method | Path | |
|---|---|---|
| `GET` | `/healthz` | `200 {"state": "ready"}`, or `503 {"state": "shutting-down", "action": "suspend"}`. |
| `POST` | `/v1/heartbeat` | `{"active": 2, "idle_seconds": 12.5}`: requests in progress through the proxy, and the time since the last one ended (`null` if none yet). Answers like `/healthz`. |
| `GET` | `/v1/status` | Below. |
| `POST` | `/v1/sleep` | `{"action": "suspend"}`: `202`, then the target goes to sleep after 5 seconds. `409` with `held_by` while work is in progress (requests, commands, inhibitors); sessions and a stay-awake do not prevent it. |
| `POST` | `/v1/stay-awake` | `{"minutes": 120, "extend": false}`; `0` cancels; with `extend`, a later end is kept. Up to 7 days. |
| `POST` | `/v1/idle` | `{"minutes": 15}`, 1 to 1440; `0` restores the configured timeout. Kept across restarts. |
| `POST` | `/v1/release` | Sets the open sessions aside, cancels the stay-awake. `{"ignored_sessions": 2, "stay_awake_cancelled": true, "sleep": {…}}` |

### `GET /v1/status`

```json
{
  "version": "v0.1.0",
  "hostname": "tower",
  "state": "ready",
  "awake_since": "2026-11-10T16:33:54+01:00",
  "memory": {"total": 34359738368, "available": 21474836480},
  "sleep": {
    "action": "suspend",
    "idle_minutes": 30,
    "default_minutes": 30,
    "in_seconds": null,
    "held_by": [
      {"kind": "requests", "detail": "1 request through the proxy"},
      {"kind": "session", "detail": "lmdr (SSH, pts/0)"}
    ],
    "stay_awake_until": null,
    "ignored_sessions": 0
  }
}
```

`awake_since` is the boot, or the last wake-up from sleep. `sleep.in_seconds` is the time
left before the target goes to sleep, when nothing holds it. `held_by[].kind` is one of
`requests`, `command`, `inhibitor`, `stay_awake`, `session`.
