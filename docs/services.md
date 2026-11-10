# Services

Entries for `services` in `proxy.yaml`. Each one keeps its usual port, so clients only
change the host: the proxy's instead of the target's. On the target, the service must
listen on the network and start at boot ([target.md](target.md#6-the-services)).

After a change: `wakegate proxy -check && sudo systemctl restart wakegate-proxy`.

## Ollama

```yaml
  - name: ollama
    listen: ":11434"
    upstream: 11434
    wake_on:
      - POST /api/generate
      - POST /api/chat
      - POST /api/embed
      - POST /api/embeddings
      - POST /api/pull
      - POST /api/create
      - POST /v1/*
```

Generation, embeddings, model downloads and the OpenAI-compatible API wake the target.
The model list (`GET /api/tags`), the version and the running models, which Open WebUI
and other clients ask for regularly, do not: they get a `503` while the target sleeps,
and clients show the models again once it is awake. Model details (`POST /api/show`) do
not wake it either.

On the target, Ollama listens on `127.0.0.1` by default: set `OLLAMA_HOST=0.0.0.0`
(`sudo systemctl edit ollama`, then `Environment="OLLAMA_HOST=0.0.0.0"` under
`[Service]`).

## OpenAI-compatible servers

llama.cpp's `llama-server`, vLLM, LM Studio, LocalAI, llama-swap:

```yaml
  - name: llm
    listen: ":8080"          # LM Studio: 1234, vLLM: 8000
    upstream: 8080
    wake_on: [POST /*]
```

Every `POST` wakes the target: completions, chat, embeddings, reranking, tokenization.
`GET` requests (the model list, `/health`, `/props`, `/slots`) do not.

## ComfyUI

```yaml
  - name: comfyui
    listen: ":8188"
    upstream: 8188
    health: /system_stats
    wake_on:
      - POST /prompt
      - POST /api/prompt
      - POST /upload/*
      - POST /api/upload/*
```

Queuing a prompt and uploading an image (a client sends the image to edit before the
prompt) wake the target. Opening the web interface in a browser shows the waiting page,
then the interface. Its WebSocket and its polling do not wake the target, nor keep it
awake.

A render goes on after `POST /prompt` has answered. If renders can last longer than the
agent's `idle_timeout`, let the agent see the queue (`agent.yaml`):

```yaml
keep_awake:
  commands:
    - curl -sf http://127.0.0.1:8188/queue | grep -q '"queue_running": \[\['
```

Start ComfyUI with `--listen 0.0.0.0`.

## S3-compatible storage

MinIO, Garage, SeaweedFS, VersityGW: a backup tool that writes to S3 should wake the
target for any request, so leave out `wake_on`. Monitoring probes are the exception:

```yaml
  - name: s3
    listen: ":9000"
    upstream: 9000
    health: /minio/health/live   # MinIO; see your server's documentation
    never_wake: [GET /minio/health/live]
```

Each request holds the target awake until its answer ends, so a long upload is never cut
by a sleep. Uploads above `max_body_mb` are streamed: if the target went to sleep under
one, it is not sent again and the client retries it, as S3 clients do. Raise
`max_body_mb` if you want the proxy to keep larger parts in memory.

## restic REST server

```yaml
  - name: restic
    listen: ":8000"
    upstream: 8000
```

Every request wakes the target. The repository URL becomes
`rest:http://proxy:8000/`.

## SSH, rsync, Borg, restic over SFTP

```yaml
  - name: ssh
    tcp: true
    listen: ":2222"
    upstream: 22
```

Each connection wakes the target and holds it awake until it closes. The client
connects to the proxy at once and waits for the server's greeting while the target
wakes up:

```sh
ssh -p 2222 user@proxy
rsync -a -e "ssh -p 2222" ./photos/ user@proxy:/srv/photos/
borg create ssh://user@proxy:2222/srv/borg::{now} ~/documents
```

OpenSSH has no time limit on that wait unless `ConnectTimeout` is set: if it is, make it
longer than a wake-up (`-o ConnectTimeout=120`). The client records the target's host key
under the proxy's name and port.

Protocols that keep a connection open as long as a client is around, such as SMB or NFS,
hold the target awake for as long: the TCP mode suits sessions that end.

## Any other web application

```yaml
  - name: app
    listen: ":3000"
    upstream: 3000
```

Without `wake_on`, every request wakes the target and keeps it awake, and a browser gets
the waiting page while it wakes up. A page left open that polls the server keeps the
target awake as well: list the requests that do real work in `wake_on` to avoid it.

## Calling the proxy from your own client

A client that checks a service before using it can tell a sleeping target from a broken
service: the proxy's `503` carries `X-Wakegate: 1` and `Retry-After`. It can then wake
the target and wait for the service with one request on the service's port:

```sh
curl -X POST http://proxy:11434/__wake     # 200 {"ok": true, "service": "ollama"} once it is up
```

The proxy answers it; the service never sees it. Errors from the proxy have the shape
OpenAI-compatible clients display: `{"error": {"message": "…", "type": "wakegate"}}`.
