# Architecture

## The shape

```
cmd/app            wiring: env config, contract routes, shutdown
internal/api       HTTP routes, JSON errors, status page
internal/monitor   Service (scheduler + on-demand checks)
                   NetChecker (HTTP / TCP / certificate)
                   Store (policy, history, incidents, uptime)
                   Persister (JSON snapshot to DATA_FILE)
internal/alert     Dispatcher → log, file, webhook channels
```

A check, scheduled or from `POST /targets/{id}/check`, goes through
`Service.CheckNow`: it takes the target's lock, so checks of one target never
overlap. `NetChecker` turns the network result into an `Observation`, and
`Store.Record` applies the policy under the store's mutex. If the belief
changed, `Record` returns a `Transition` and the service hands it to the
`Dispatcher`, which sends to every channel on its own goroutine, so a slow
webhook never delays a check. The handler then answers with the observation
and the resulting state.

Every target has its own goroutine with a ticker at `interval_seconds`. The
first scheduled check is one interval after creation, not immediately, so a
manual test with two checks is not disturbed by a third.

## The data

All in memory, in `Store`: targets, the last 300 000 observations per target
(time, up/down, response time, detail, certificate expiry), the current
belief, the streak of disagreeing observations, and the incidents. Alert
deliveries (the last 500) are in the `Dispatcher`.

With `DATA_FILE` set (the default in the image), the store is written to one
JSON file at most every 5 seconds when it has changed, and on shutdown. The
write goes to a temporary file that is then renamed into place. On start the
file is loaded and scheduling resumes. So a restart keeps uptime history,
current belief, a streak in progress and an open incident. The time the
monitor itself was down holds no checks and therefore counts neither for nor
against uptime. Alert deliveries are not persisted. `POST /reset` clears the
memory and the file.

## Three decisions

1. **N consecutive observations, in both directions, rather than k of the last
   m.** "3 in a row" is the rule in the brief and is easy to explain on a
   status page. A k-of-m window would catch a target that flaps (down, up,
   down, up) which this policy never flags. I accept that blind spot for a rule
   whose cost is easy to state: detection takes up to `3 × interval` (plus
   timeouts), and the incident records both `started_at` (the first failed
   check) and `detected_at` (the third), so the delay is visible, not hidden.

2. **Uptime as a share of checks, not of wall-clock time.** The alternative
   is to treat the time between checks as up or down according to the last
   observation. That invents data for the gaps, including the time the
   monitor was not running. Counting checks reports only what was measured.
   The period is always returned with the figure, and an empty period gives
   `null`, not 100%.

3. **An expiring certificate is a failed check, not a separate alert.** The
   alternative was a separate warning type. Folding it into the HTTPS check
   means it goes through the same policy, incidents and alerts as anything
   else, at the cost of reporting a site that still works as down. That is
   deliberate: the certificate expiring on Saturday is an outage already
   scheduled.

Also: the standard library only (`net/http` 1.22 routing is enough). A JSON
snapshot instead of SQLite, because the data is small and append-mostly, and
it keeps the binary static with no cgo.

## What is not built yet

- Editing or deleting a target (only `/reset` removes targets).
- Retries with back-off for a failing webhook: a failed delivery is recorded
  in `/alerts` but not retried.
- A slow-but-answering threshold separate from the timeout: today a response
  is up if it arrives within `timeout_seconds`.
- History is capped per target, not by age.
