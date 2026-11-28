# Cloud Runner Protocol (`runner-v1`)

`runner-v1` is the HTTP+JSON contract between the `box` CLI / fleet dispatcher and a remote cloud runner (such as Modal). The Go client stays backend-agnostic: it speaks only this protocol and reads opaque auth headers from the host's token file.

Golden fixtures live under [`docs/protocol/runner-v1/`](protocol/runner-v1/).

_Status: frozen v1 contract (2026-11-27), tracking #276, #277, #278, and #280._

## Conventions

- **Protocol version:** `1` (reported by `GET /v1/probe`).
- **Timestamps:** RFC 3339 UTC (`Z` suffix), matching [`internal/dash/types.go`](../internal/dash/types.go). Nullable timestamps are `null` in JSON when unset.
- **Job IDs:** Bare IDs of the form `<repo>.<ref>` accepted by `validJob` (`^[a-z][a-z0-9-]*\.[0-9]{8}-[0-9]{6}-[0-9a-f]+$`, [`cmd/box/box.go`](../cmd/box/box.go#L1515)), where `<repo>` satisfies `validBox` (`^[a-z][a-z0-9-]{0,62}$`) and `<ref>` is `YYYYMMDD-HHMMSS-<hex>`. The client chooses `id` before submission so retries are idempotent; fleet qualification (`h1-<tag>~<bare>`, [`cmd/box/job_identity.go`](../cmd/box/job_identity.go)) stays client-side and is never sent on the wire.
- **Sizes:** `small`, `medium`, `large`, `gpu` from [`internal/mode/size.go`](../internal/mode/size.go).

## Authentication

Every request carries the extra headers from the host's local token file:

```json
{
  "headers": {
    "Modal-Key": "<key>",
    "Modal-Secret": "<secret>"
  }
}
```

The Go client reads the `headers` map verbatim from the token file and sends every entry on each HTTP request. Backend-specific header names (such as `Modal-Key` / `Modal-Secret`) never appear in Go source code.

## Error Envelope

Non-2xx JSON responses use a uniform error envelope:

```json
{
  "error": "<code>",
  "message": "<human text>"
}
```

| HTTP status | `error` code | Client behavior | Fixture |
| --- | --- | --- | --- |
| `400` | `invalid` | Bad request / malformed spec; exit code `2` (no host fallback) | — |
| `400` | `timeout_cap` | Requested `timeoutSec` exceeds runner `maxTimeoutSec`; exit code `2` | — |
| `401` | `unauthorized` | Invalid or missing auth headers | — |
| `402` | `budget` | Monthly `capUSD` reached; fleet turn-away to next host | [`error-budget.json`](protocol/runner-v1/error-budget.json) |
| `404` | `not_found` | Unknown job ID | — |
| `409` | `busy` | Runner at `maxConcurrent`; fleet turn-away to next host | [`error-busy.json`](protocol/runner-v1/error-busy.json) |
| `409` | `gone` | Sandbox no longer kept (`keepSec` expired or `function` mode); `pull` unavailable | [`error-gone.json`](protocol/runner-v1/error-gone.json) |
| `423` | `paused` | Runner paused (`POST /v1/pause`); fleet turn-away to next host | [`error-paused.json`](protocol/runner-v1/error-paused.json) |
| `5xx` / network | — | Unconfirmed submission error; **do not** retry on another host | — |

## Endpoints

### `GET /v1/probe`

Reports runner availability, supported sizes, concurrency, pause state, timeout cap, and monthly budget status.

- **Response:** `200 OK`, [`probe.json`](protocol/runner-v1/probe.json)
  - `protocol` (`int`): `1`
  - `sizes` (`[]string`): supported sizes (subset of `small`, `medium`, `large`, `gpu`)
  - `running` (`int`): active jobs count
  - `maxConcurrent` (`int`): configured concurrency ceiling
  - `paused` (`bool`): whether new submissions are turned away (`423 paused`)
  - `maxTimeoutSec` (`int`): maximum allowed `timeoutSec`
  - `budget` (`object`):
    - `month` (`string`): `YYYY-MM`
    - `spentUSD` (`number`): settled spend this month
    - `runningUSD` (`number`): estimated accrued spend for active jobs
    - `capUSD` (`number`): configured monthly cap
    - `creditUSD` (`number`): remaining or monthly credit reference
    - `warnAt` (`number`): warning threshold fraction (e.g. `0.75`)

### `POST /v1/jobs`

Submits a job for execution. Uses `multipart/form-data` with two parts:

1. `spec` (`application/json`): job specification matching [`job-spec.json`](protocol/runner-v1/job-spec.json):
   - `id` (`string`): client-chosen bare job ID (`<repo>.<ref>`)
   - `repo` (`string`): repository/box name (`validBox`)
   - `ref` (`string`): bundle ref suffix (`YYYYMMDD-HHMMSS-<hex>`); commit lives at `refs/agent-box/<ref>` in the bundle
   - `cmd` (`[]string`): command argv (one string runs via shell, matching `box run`)
   - `env` (`map[string]string`): environment variables
   - `timeoutSec` (`int`): run timeout in seconds; `0` selects the runner's `maxTimeoutSec`
   - `size` (`string`): one of `small`, `medium`, `large`, `gpu`
   - `reason` (`string`): agent's reason for running remotely
   - `source` (`dash.SourceRevision`): exact JSON fields `commit`, `dirty`, `snapshotCommit` from [`internal/dash/types.go`](../internal/dash/types.go#L217-L221)
2. `bundle` (`application/octet-stream`): `git bundle` bytes containing the worktree snapshot at `refs/agent-box/<ref>`.

- **Responses:**
  - `201 Created`: new job accepted, returning `{"id": "<id>", "state": "queued"}` ([`job-queued.json`](protocol/runner-v1/job-queued.json)).
  - `200 OK`: duplicate `id` (idempotent retry); returns the existing Job object (`{"id", "state", ...}`).
  - `402 budget` / `409 busy` / `423 paused`: fleet turns away to the next configured host (and exits `75` if all hosts turn away).
  - `400 invalid` / `400 timeout_cap`: client error; `box` exits with code `2` without falling back.
  - Network error or `5xx`: `box` returns an unconfirmed-submission error and **does not** retry on another host, because the job may already be queued or running on the first runner. Same-runner retries with the identical `id` are safe via `200 OK` deduplication.

### `GET /v1/jobs?limit=20`

Lists recent jobs, newest first.

- **Query:** `limit` (`int`, default `20`).
- **Response:** `200 OK`, `{"jobs": [Job, ...]}` ([`job-list.json`](protocol/runner-v1/job-list.json)).

### `GET /v1/jobs/{id}?wait=25`

Fetches a single job, with optional long-polling.

- **Query:** `wait` (`int`, seconds, up to `25`): server holds the request open up to `wait` seconds until the job reaches a terminal state (`done`, `cancelled`, `lost`) or transitions state, then returns the current Job object.
- **Response:** `200 OK`, [`job-done.json`](protocol/runner-v1/job-done.json):
  - `id` (`string`): bare job ID
  - `state` (`string`): `queued | running | done | cancelled | lost`
  - `exit` (`int | null`): process exit code once finished (`137` on cancel/SIGKILL), else `null`
  - `attempt` (`int`): execution attempt counter (starts at `1`)
  - `startedAt` (`string | null`): RFC 3339 UTC timestamp
  - `endedAt` (`string | null`): RFC 3339 UTC timestamp
  - `logBytes` (`int64`): total log bytes written so far
  - `tail` (`string`): trailing log excerpt (`dash.TailBytes` = 2 KiB)
  - `mode` (`string`): `function | sandbox`
  - `cost` (`object`): `{"usd": number, "cpuSec": number, "gpuSec": number}`
  - `budget` (`object`): current monthly budget snapshot (same schema as `/v1/probe`)

### `GET /v1/jobs/{id}/log?offset=&limit=`

Reads a chunk of a job's log using the exact `dash.LogChunk` JSON schema from [`internal/dash/types.go`](../internal/dash/types.go#L381-L389).

- **Query:** `offset` (`int64`, default `0`), `limit` (`int64`, capped at `dash.MaxLogChunk` = `65536` bytes).
- **Response:** `200 OK`, [`log-chunk.json`](protocol/runner-v1/log-chunk.json):
  - `offset` (`int64`): next byte offset to request
  - `data` (`string`): UTF-8 log slice (invalid UTF-8 replaced with `U+FFFD`)
  - `eof` (`bool`): no more bytes currently available
  - `paused` (`bool`): whether the underlying container is paused
  - `stopped` (`bool,omitempty`): true when read from a finished/stopped container
  - `logBytes` (`int64,omitempty`): total log size in bytes
  - `dataBase64` (`string,omitempty`): exact raw bytes when UTF-8 splits or is invalid

### `POST /v1/jobs/{id}/cancel`

Cancels a queued or running job.

- **Response:** `200 OK`, [`cancel.json`](protocol/runner-v1/cancel.json): `{"cancelled": true, "exit": 137}`.

### `GET /v1/jobs/{id}/file?path=rel`

Streams a tar archive of the relative path `rel` from the job's kept sandbox.

- **Response:**
  - `200 OK` (`application/x-tar`): tar stream while the sandbox is kept within `keepSec`.
  - `409 Conflict`: `{"error": "gone", "message": "..."}` ([`error-gone.json`](protocol/runner-v1/error-gone.json)) once `keepSec` expires, or in `function` mode where files are not retained.

### `POST /v1/pause`

Toggles runner admission pause state.

- **Request:** `{"paused": bool}`
- **Response:** `200 OK`, [`pause.json`](protocol/runner-v1/pause.json): `{"paused": bool}`.

### `POST /v1/stop`

Emergency stop: cancels every running and queued job on the runner.

- **Response:** `200 OK`, [`stop.json`](protocol/runner-v1/stop.json): `{"stopped": <int>}`.

### `PUT /v1/config`

Updates mutable runner configuration.

- **Request / Response (`200 OK`):** [`config.json`](protocol/runner-v1/config.json) echoing the applied values:
  - `capUSD` (`number`): monthly budget cap in USD
  - `maxConcurrent` (`int`): concurrent job ceiling
  - `maxTimeoutSec` (`int`): maximum job timeout in seconds
  - `keepSec` (`int`): post-job sandbox retention in `sandbox` mode
  - `mode` (`string`): `function | sandbox`

## v1 Scope & Open Design Decisions

- **No D1 and no R2 in v1:** The fleet state controller uses Durable Object SQLite only (see [`docs/controller-protocol.md`](controller-protocol.md)); R2 object storage and retention lifecycle rules arrive with retained logs (#258).
- **Hibernated WebSocket deferred:** No server-to-host push transport in v1; deferred until a feature requires it (#169 / #279).
- **Self-hosted `cmd/boxctl` deferred:** No standalone Go+SQLite controller binary in v1; S6 ships an in-memory reference `http.Handler` for Go tests.
- **Open design decisions for Tyler:**
  1. **Cloud `pull` in v1:** `box pull` is refused in `function` mode and available only during `keepSec` in `sandbox` mode until R2-backed retention (#258) lands.
  2. **Cloud host `priority` default:** Cloud hosts default to `priority: -1` when omitted in `hosts.json` so free local PC hosts are preferred before metered cloud spend.
  3. **Split pause controls:** Pausing the PC lives on the dashboard (`boxd`), while `box cloud pause` talks directly to the runner (`POST /v1/pause`), keeping Modal tokens off the PC.
