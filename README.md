# Uptrack

Uptime monitoring for HTTP services: Uptrack checks your endpoints on a schedule, records latency and
availability, opens an incident when a service goes down and alerts you in Telegram or through a webhook.
A public status page shows the last 30 days.

Written in Go with the standard library's HTTP server, PostgreSQL and Prometheus metrics.

[![CI](https://github.com/oldsoska/uptrack/actions/workflows/ci.yml/badge.svg)](https://github.com/oldsoska/uptrack/actions/workflows/ci.yml)

> Status: **MVP.** See [ROADMAP](docs/ROADMAP.md). It watches [Slotly](https://github.com/oldsoska/slotly),
> a booking platform I built in Java.

## Highlights

- **Goroutine per monitor, capped by a semaphore.** Each monitor runs on its own ticker; a buffered channel limits
  how many HTTP checks are in flight at once. A monitor's state is owned by its goroutine, so it needs no lock.
  Tested under the race detector, including a test that asserts the cap is never exceeded.
- **No alert storms.** A monitor goes DOWN only after N consecutive failures (default 2) and alerts once per
  incident; the first success resolves it and reports the outage length.
- **Consistent by construction.** A check result, the monitor's new status and the incident change are written in
  one transaction; a partial unique index guarantees at most one open incident per monitor.
- **SSRF protection.** With `BLOCK_PRIVATE_TARGETS` (on by default) the dialer refuses loopback, private,
  link-local and CGNAT addresses. The check runs in `net.Dialer.Control`, i.e. on the resolved IP of every
  connection, so DNS rebinding and redirects to internal hosts are blocked too.
- **Stats in SQL.** Uptime and p50/p95 latency (successful checks only, so timeouts do not skew it) come from
  `percentile_cont ... FILTER`, per monitor via `LEFT JOIN LATERAL`; daily bars use `generate_series` so days
  without data still appear.
- **Graceful shutdown.** On SIGTERM the server stops accepting requests, finishes in-flight ones, cancels the
  scheduler context and waits for running checks and alert deliveries.
- **Small and locked down.** A 15 MB static binary on a distroless image running as non-root; the container
  health check is the binary itself, since the image has no shell.

## Architecture

```mermaid
flowchart LR
    Admin[Admin / curl] -->|REST + bearer token| API
    Visitor[Anyone] -->|GET /status| API
    Prom[Prometheus] -->|GET /metrics| API
    subgraph Uptrack [one Go process]
        API[net/http server] --> Store
        API -->|upsert / remove| Sched[Scheduler<br/>goroutine per monitor]
        Sched -->|semaphore| Checker[HTTP checker<br/>SSRF guard]
        Sched --> Store[(store: pgx)]
        Sched --> Notify[Notifiers<br/>retry + backoff]
    end
    Checker --> Targets[Monitored services]
    Store --> PG[(PostgreSQL)]
    Notify --> TG[Telegram]
    Notify --> WH[Webhook]
```

| Package | Responsibility |
|---|---|
| `cmd/uptrack` | Wiring, configuration, graceful shutdown, retention job, `healthcheck` subcommand |
| `internal/monitor` | Domain model and the UP/DOWN state machine — no I/O |
| `internal/checker` | One HTTP check: timeout, expected status range, SSRF guard |
| `internal/scheduler` | Runs monitors, applies results, triggers alerts |
| `internal/store` | PostgreSQL via pgx, embedded migrations, stats queries |
| `internal/notify` | Telegram, webhook, fan-out and retry with exponential backoff |
| `internal/httpapi` | Admin REST API, status page, health and metrics endpoints |
| `internal/metrics` | Prometheus collectors |
| `internal/config` | Environment variables with validation |

## Running locally

Requirements: Docker. Go 1.25+ only for development.

```bash
docker compose up -d --build
```

- Status page: http://localhost:8081/status
- Metrics: http://localhost:8081/metrics

Create a monitor (the token is the local default from `docker-compose.yml`):

```bash
curl -X POST localhost:8081/api/v1/monitors \
  -H "Authorization: Bearer local-dev-admin-token-change-me-0123" -H "Content-Type: application/json" \
  -d '{"name":"Slotly","url":"https://slotly-minx.onrender.com/actuator/health","intervalSeconds":60,"timeoutSeconds":20}'
```

Tests (need Docker for the PostgreSQL container):

```bash
go test -race ./...
```

## API

All `/api/v1` endpoints require `Authorization: Bearer <ADMIN_TOKEN>`. Errors are RFC 9457 Problem Details.

| Method and path | Description |
|---|---|
| `GET /api/v1/monitors` | All monitors with status and last-24h stats |
| `POST /api/v1/monitors` | Create; returns `201` and `Location` |
| `GET /api/v1/monitors/{id}` | One monitor with 24h and 7d stats |
| `PUT /api/v1/monitors/{id}` | Replace settings; `"paused": true` stops checks |
| `DELETE /api/v1/monitors/{id}` | Delete with its history |
| `GET /api/v1/monitors/{id}/results?limit=` | Latest check results (max 500) |
| `GET /api/v1/incidents?open=true&limit=` | Incidents, newest first |
| `GET /status` | Public HTML status page (names only, never URLs) |
| `GET /healthz`, `GET /readyz` | Liveness, and readiness including the database |
| `GET /metrics` | Prometheus metrics |

Monitor fields: `name`, `url` (http/https), `method` (`GET`/`HEAD`, default `GET`), `intervalSeconds` (30–86400,
default 60), `timeoutSeconds` (1–30, below the interval, default 10), `expectedStatusMin`/`Max` (default 200–399),
`failureThreshold` (1–10, default 2), `paused`.

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | — | Required, e.g. `postgres://user:pass@host/db?sslmode=require` |
| `ADMIN_TOKEN` | — | Required, at least 32 characters |
| `PORT` | `8080` | |
| `BLOCK_PRIVATE_TARGETS` | `true` | Refuse checks to internal addresses (SSRF guard) |
| `MAX_CONCURRENT_CHECKS` | `20` | Semaphore size |
| `RESULT_RETENTION` | `720h` | Check history older than this is deleted hourly; incidents are kept |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID` | — | Telegram alerts, set both |
| `WEBHOOK_URL` | — | JSON POST on every incident change |

## Design decisions

| Decision | Why |
|---|---|
| Standard library `net/http` instead of a framework | Go 1.22 routing (`GET /api/v1/monitors/{id}`) covers the API; fewer dependencies to audit and upgrade. |
| Goroutine per monitor instead of a central timer heap | Simplest correct model in Go and cheap at thousands of monitors; the semaphore bounds the expensive part, network I/O. |
| Status in the database, history in a separate table | Restarting resumes each monitor from its stored status and failure count, so an outage in progress is neither re-alerted nor forgotten. |
| A small own migrator over a library | Embedded SQL files, one transaction, an advisory lock against concurrent starts — enough for this project. |
| Labels by `monitor_id` | Bounded metric cardinality; names and URLs change and could be long. |
| Alerts delivered after the transaction commits | A slow Telegram must not hold a database transaction open. The trade-off — an alert can be lost if the process dies in between — is on the roadmap (transactional outbox). |
| Single active instance | The scheduler is in-process; two instances would check twice. Leader election via `pg_try_advisory_lock` is on the roadmap. |
