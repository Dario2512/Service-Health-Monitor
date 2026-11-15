# Service Health Monitor

A service that watches other services over HTTP and TCP, decides whether
each one is up by a stated policy rather than a single check, alerts when it
changes its mind, records incidents and uptime, and publishes a status page
anyone can read. For whoever runs a handful of services and wants to hear
about real outages and nothing else.

- **Student:** Friskan Dario, group SE
- **Project:** 7 — Service Health Monitor
- **Language:** Go (standard library only)

## What it does

Each target is checked every `interval_seconds`, with a `timeout_seconds`
limit, and can also be checked on demand. An HTTP target is up when it answers
below 400 within the timeout; for HTTPS the certificate must also verify and
must not expire within `cert_warn_days` (default 14), so a certificate that
runs out on Saturday shows as a failure now. A TCP target is up when it
accepts a connection.

What one check saw (`observed`) and what the service believes (`state`) are
kept apart. The state changes only after **3 consecutive** observations
disagree with it, in either direction. A change of state opens or closes an
incident and sends an alert to every channel: the service log, a JSON-lines
file, and a webhook when `ALERT_WEBHOOK_URL` is set. Recoveries are alerted
too. A failed check on its own never alerts.

Uptime is the share of checks that observed the target up, over a period that
is always reported with the figure. A period with no checks has no
percentage (`null`), not 100%. With `DATA_FILE` set (the default in the
container) history, state and incidents survive a restart.

Not built: authentication, editing or deleting targets, checks other than
HTTP GET and TCP connect.

## How to run it

```
docker run --rm -p 8080:8080 ghcr.io/dario2512/service-health-monitor:latest
```

To keep history across containers, add `-v monitor-data:/data`. To send alerts
to a Slack/Mattermost-style webhook, add `-e ALERT_WEBHOOK_URL=https://...`.

Without Docker:

```
go run ./cmd/app
go run ./cmd/app --version
DATA_FILE=./data/monitor.json go run ./cmd/app   # keep state across restarts
go test ./...
```

| Variable            | Default                                    | Meaning                          |
|---------------------|--------------------------------------------|----------------------------------|
| `DATA_FILE`         | unset (`/data/monitor.json` in the image)  | where state is saved; unset = memory only |
| `ALERT_FILE`        | `$TMPDIR/service-health-monitor-alerts.jsonl` (`/data/alerts.jsonl` in the image) | file alert channel |
| `ALERT_WEBHOOK_URL` | unset                                      | webhook alert channel            |

## Routes

| Route | Method | |
|---|---|---|
| `/health`, `/version`, `/` | GET | common contract |
| `/reset` | POST | 204, discards targets, history, incidents, alerts and the data file |
| `/targets` | POST | `{"name","type":"http"\|"tcp","address","interval_seconds","timeout_seconds"}`, optional `cert_warn_days`. 201 |
| `/targets` | GET | targets with state, last observation, last response time, uptime over the last 24 h |
| `/targets/{id}` | GET | one target |
| `/targets/{id}/check` | POST | one check now: `{"observed","response_ms","state",...}` |
| `/targets/{id}/uptime` | GET | `?from=&to=` (RFC 3339 or Unix seconds, default last 24 h) → `{"period","checks","up","uptime_percent"}` |
| `/incidents` | GET | newest first: `started_at`, `detected_at`, `ended_at`, summary and observations |
| `/alerts` | GET | configured channels and every delivery attempt |
| `/status` | GET | HTML status page for non-operators |

Errors are `{"error":"validation|not_found|conflict|unprocessable","message":"..."}`
with 400, 404, 409 (duplicate name) or 422 (timeout longer than interval;
`from` not before `to`).

## What you should see

Point a target at a port nothing listens on and check it twice:

```
curl -i -H "Content-Type: application/json" \
  -d '{"name":"ghost","type":"tcp","address":"127.0.0.1:9","interval_seconds":60,"timeout_seconds":2}' \
  http://localhost:8080/targets
curl -X POST http://localhost:8080/targets/1/check
curl -X POST http://localhost:8080/targets/1/check
```

Both checks answer `"observed":"down"` with `"state":"up"`. `GET /targets`
shows `"state":"up"` with `"consecutive_disagreeing":2`, and `GET /incidents`
is `[]`. A third check flips the state to `down`, `/incidents` gets one entry,
the log prints `ALERT [DOWN] ghost is down`, and `/status` says
"One service is having problems".
