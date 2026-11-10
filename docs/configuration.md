# Configuration

Two YAML files: `/etc/wakegate/proxy.yaml` on the machine that stays on, and
`/etc/wakegate/agent.yaml` on the target. Unknown settings are errors. Durations are
written `30s`, `3m`, `1h`. Check a file before restarting:

```sh
wakegate proxy -check     # -config /etc/wakegate/proxy.yaml by default
wakegate agent -check
```

Commented examples: [deploy/proxy.yaml](../deploy/proxy.yaml) and
[deploy/agent.yaml](../deploy/agent.yaml).

## Proxy

### `target`

| Setting | Default | |
|---|---|---|
| `name` | `host` | Shown on the admin page, the waiting page and in error messages. |
| `host` | required | The target's address on the network. |
| `mac` | required, unless `wake_command` | MAC address of its wired network card. |
| `broadcast` | `[255.255.255.255]` | Where the magic packet goes: your network's broadcast address, such as `192.168.1.255`, optionally with a port (`192.168.1.255:7`; port 9 by default). The packet is sent three times, half a second apart, and again every 15 seconds while the target does not answer. |
| `wake_command` | none | A program and its arguments, run instead of sending the magic packet. See below. |
| `agent` | none | URL of the agent, such as `http://192.168.1.20:9777`. |
| `agent_token_file` | `/etc/wakegate/agent.token` | The agent's token. `WAKEGATE_AGENT_TOKEN` overrides it. |
| `probe` | the first service | Without an agent: an address (`host:port`) that accepts connections when the target is awake. |
| `boot_timeout` | `3m` | How long a request waits for the target to wake up before a `504`. Raise it for a target that boots slowly from power off. |
| `shutdown_timeout` | `3m` | How long a request waits for a target that is going to sleep, before waking it again. |

**With an agent**, the proxy sends it a heartbeat every two seconds: how many requests
are in progress or waiting, and how long ago the last one ended. The answer is the
target's state. An agent that does not answer means the target sleeps or is off.

**Without an agent**, the target is awake when `probe` accepts a connection. The proxy
cannot tell that the target is about to sleep: a request it was relaying when the target
fell asleep is sent again after the wake-up.

**`wake_command`** replaces the magic packet when the proxy cannot send it to the target
itself: a proxy on a VPS, a target on another VLAN, a server woken through IPMI or a smart
plug. It runs as the `wakegate` user, at most every 15 seconds while a request waits, with
a 30-second limit. A non-zero exit is logged.

```yaml
  wake_command: [ssh, -i, /etc/wakegate/id_ed25519, root@router, etherwake, -i, br-lan, "04:d9:f5:cb:75:23"]
  # wake_command: [ipmitool, -H, 192.168.1.21, -U, admin, -f, /etc/wakegate/ipmi.pass, chassis, power, on]
```

The systemd unit hides home directories (`ProtectHome=true`): keep the keys and
passwords such a command needs in `/etc/wakegate`, readable by the `wakegate` group.

### `admin`

| Setting | Default | |
|---|---|---|
| `listen` | none: no admin page | Address of the admin page, its API and `/metrics`, such as `":9778"` or `"192.168.1.2:9778"`. |

### `services`

Each entry is a port the proxy listens on, relayed to a port on the target.

| Setting | Default | |
|---|---|---|
| `name` | required | Lower-case letters, digits, `-` and `_`. |
| `listen` | required | Where the proxy listens: `":11434"`, `"11434"` or `"192.168.1.2:11434"`. The unit allows ports below 1024. |
| `upstream` | required | A port on the target (`11434`), or `host:port` for another address. |
| `tcp` | `false` | Relay raw TCP connections instead of HTTP requests. |
| `wake_on` | every request | HTTP requests that may wake the target. |
| `never_wake` | none | HTTP requests that never wake it, even if `wake_on` matches them. |
| `waiting_page` | `true` | A browser opening a page while the target sleeps gets a page that reloads every 5 seconds, and the target wakes up. |
| `health` | none | A path that answers `2xx` once the service is ready, such as `/health`. |
| `start_timeout` | `1m` | Once the target is awake, how long the proxy keeps trying the service. |

### Which requests wake the target

A rule is an optional method and a path. A path ending in `*` matches everything that
starts with it; any other path must match exactly. The query string is not part of the
match.

```yaml
wake_on:
  - POST /api/chat        # this method, this path
  - POST /v1/*            # every POST under /v1/
  - /upload               # every method
```

For each HTTP request:

1. a request that matches `never_wake` does not wake the target;
2. otherwise, without `wake_on`, every request does; with it, the ones it matches do.

A request that may wake the target also keeps it awake, from its arrival to the end of
its answer: the agent's idle timeout starts when it ends. The other requests are relayed
while the target is awake and get a `503` with `Retry-After: 60` while it sleeps. They
never keep it awake, so a client that checks on a service every few seconds does not
prevent its sleep.

Choose `wake_on` so that it covers the requests that do work, and leaves out those a
client sends on its own: model lists, versions, health checks, a WebSocket reconnecting.
[services.md](services.md) has the rules for common services.

### Readiness after a wake-up

Once the agent answers, the service may still be starting. With `health`, the proxy
checks that path every second until it answers `2xx`, then relays. Without it, the proxy
relays at once, and if the service refuses the connection, it tries again every second
until `start_timeout`. A request whose body is above `max_body_mb` is sent only once, so
set `health` for services that receive large uploads.

### Other settings

| Setting | Default | |
|---|---|---|
| `max_body_mb` | `16` | Request bodies up to this size are kept in memory, so the request can be sent again if the target went to sleep under it. Larger bodies are streamed and sent once. |
| `state_dir` | `/var/lib/wakegate` | Where the awake window is kept (`proxy.json`). |

## Agent

| Setting | Default | |
|---|---|---|
| `listen` | `":9777"` | Address of the agent's API, for the proxy. |
| `token_file` | `/etc/wakegate/agent.token` | The token the proxy must send. `WAKEGATE_TOKEN` overrides it. At least 16 characters. |
| `idle_timeout` | `30m` | Time without requests before the target goes to sleep, in whole minutes, up to `24h`. The admin page can change it; the change is kept in `state_dir`. |
| `sleep` | `suspend` | `suspend`, `hibernate`, `poweroff`, or `none` to never sleep. |
| `keep_awake.sessions` | `all` | Logged-in users keep the target awake: `all`, `active`, or `none`. |
| `keep_awake.inhibitors` | `true` | systemd sleep inhibitors keep it awake. |
| `keep_awake.commands` | none | Shell commands; one that exits 0 means work is in progress. |
| `state_dir` | `/var/lib/wakegate` | Where the idle timeout set from the admin page is kept (`agent.json`). |

Every 15 seconds the agent checks what holds the target awake. The target goes to sleep
when nothing does and the last request ended `idle_timeout` ago. Before it sleeps, the
agent reports `shutting-down` for 5 seconds; a request that reaches the proxy meanwhile
calls the sleep off. After any wake-up, the idle timeout starts over.

What holds the target awake:

- **Requests through the proxy**, from their arrival, waits included, to the end of
  their answer.
- **Sessions**, from logind. `all` counts every logged-in user: SSH, console, desktop.
  `active` leaves out the sessions logind reports as idle: a desktop session whose screen
  is locked or idle, or a terminal without keyboard input for `IdleActionSec` (30 minutes
  by default, in `logind.conf`). A long job running in an idle terminal does not count
  then: wrap it in `systemd-inhibit`. With `none`, sessions never keep it awake.
- **Inhibitors**: a program that holds a `block` inhibitor on `sleep` (or on `shutdown`,
  with `sleep: poweroff`). Wrap a job in one:

  ```sh
  systemd-inhibit --what=sleep --who=backup --why="Copy to the NAS" restic backup /home
  ```

  From an SSH session, polkit may refuse an unprivileged user an inhibitor; run it with
  `sudo`, or from a systemd service.
- **Commands**, run with `sh -c` as the `wakegate` user, at most every 15 seconds, for
  10 seconds at most each. Exit 0 means busy, any other exit means idle, except 126 and
  127 (not executable, not found): a broken check holds the target awake, and the admin
  page shows why.

  ```yaml
  keep_awake:
    commands:
      - pgrep -x borg
      - test -e /run/backup.lock
      - curl -sf http://127.0.0.1:8188/queue | grep -q '"queue_running": \[\['
  ```

  The unit's sandbox applies to them; a command that needs more, such as `docker ps`,
  needs a drop-in (`sudo systemctl edit wakegate-agent`):

  ```ini
  [Service]
  SupplementaryGroups=docker
  ```

- **Stay awake**, set from the admin page or the API, until a given time.

The admin page's *Release hold* sets the open sessions aside (a session opened later
holds the target again) and cancels the stay-awake. The idle timeout still counts from
the last request.

Sleeping goes through logind, as `systemctl suspend` does. The installer adds a polkit
rule that allows it for the `wakegate` user. If the sleep fails, the agent logs the error
and waits another idle timeout.
