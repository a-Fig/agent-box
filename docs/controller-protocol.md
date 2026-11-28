# Fleet Controller Protocol (`controller-v1`)

`controller-v1` is the HTTP+JSON contract for the lightweight cloud state cache (Cloudflare Worker + Durable Object SQLite) that lets `box` and `box app` see a host's last known dashboard state when the host itself is asleep or unreachable.

Golden fixtures live under [`docs/protocol/controller-v1/`](protocol/controller-v1/).

_Status: frozen v1 contract (2026-11-27), tracking #276, #277, #278, and #280._

## Conventions

- **Protocol version:** `1` (reported by `GET /v1/health`).
- **Timestamps:** RFC 3339 UTC (`Z` suffix), matching [`internal/dash/types.go`](../internal/dash/types.go).
- **Host IDs:** `id` must match `^[a-z0-9-]{1,40}$`.
- **Body size limit:** `PUT /v1/hosts/{id}/state` rejects bodies over `256 KiB` (`262144` bytes) with `413`.
- **Zero-parse storage:** The Durable Object stores the `state` JSON text raw in SQLite without parsing `dash.State` fields inside the Worker, keeping request handling under the 10 ms Cloudflare Worker CPU cap.

## Authentication

Endpoints other than `GET /v1/health` require:

```http
Authorization: Bearer <token>
```

Tokens are validated with constant-time comparison:

- **`HOST_TOKEN`**: authorizes host state writes (`PUT /v1/hosts/{id}/state`).
- **`CLIENT_TOKEN`**: authorizes client reads (`GET /v1/hosts`, `GET /v1/usage`).

A missing or invalid token receives `401` (`unauthorized`); presenting a token without the required role (e.g. `CLIENT_TOKEN` on `PUT`) receives `403` (`forbidden`).

## Error Envelope

Non-2xx JSON responses use the same error envelope as `runner-v1`:

```json
{
  "error": "<code>",
  "message": "<human text>"
}
```

| HTTP status | `error` code | Meaning | Fixture |
| --- | --- | --- | --- |
| `400` | `bad_request` | Invalid host ID, malformed JSON, or missing required envelope fields | — |
| `401` | `unauthorized` | Missing or invalid `Authorization: Bearer` token | [`error-unauthorized.json`](protocol/controller-v1/error-unauthorized.json) |
| `403` | `forbidden` | Token lacks permission for this endpoint (e.g. client token on host write) | — |
| `404` | `not_found` | Unknown route | — |
| `413` | `too_large` | Request body exceeds `262144` bytes (`256 KiB`) | [`error-too-large.json`](protocol/controller-v1/error-too-large.json) |

## Endpoints

### `GET /v1/health`

Unauthenticated liveness and protocol check.

- **Auth:** none
- **Response:** `200 OK`, [`health.json`](protocol/controller-v1/health.json): `{"ok": true, "protocol": 1}`

### `PUT /v1/hosts/{id}/state`

Pushes a host's latest trimmed [`dash.State`](../internal/dash/types.go#L338-L347).

- **Auth:** `HOST_TOKEN`
- **Path param:** `id` matching `^[a-z0-9-]{1,40}$`
- **Body limit:** `262144` bytes (`256 KiB`); larger payloads receive `413` ([`error-too-large.json`](protocol/controller-v1/error-too-large.json)).
- **Request:** JSON matching [`put-state.json`](protocol/controller-v1/put-state.json):
  - `id` (`string`): host ID (must match `{id}` path parameter)
  - `name` (`string`): human-readable host name
  - `seq` (`int64`): monotonically increasing per-process sequence number
  - `sentAt` (`string`): RFC 3339 UTC timestamp when `boxd` sent the snapshot
  - `state` (`dash.State`): exact `dash.State` JSON (`machines`, `boxes`, `jobs`, `queue`, `updates`, `now`), with `jobs` trimmed to all currently running/paused/queued jobs plus at most the last `20` finished jobs so payloads stay compact.
- **Response:** `204 No Content` (empty body).

### `GET /v1/hosts`

Returns the latest cached state for all registered hosts.

- **Auth:** `CLIENT_TOKEN`
- **Response:** `200 OK`, [`hosts.json`](protocol/controller-v1/hosts.json):
  - `now` (`string`): controller server RFC 3339 UTC timestamp
  - `hosts` (`[]object`):
    - `id` (`string`): host ID
    - `name` (`string`): host display name
    - `seenAt` (`string`): RFC 3339 UTC timestamp when the controller stored this snapshot
    - `stale` (`bool`): `true` when `now - seenAt > 180s`
    - `state` (`dash.State`): raw `dash.State` JSON last uploaded by the host

### `GET /v1/usage`

Returns daily controller request usage against the free-tier budget.

- **Auth:** `CLIENT_TOKEN`
- **Response:** `200 OK`, [`usage.json`](protocol/controller-v1/usage.json):
  - `day` (`string`): UTC date `YYYY-MM-DD`
  - `requests` (`int`): requests counted today
  - `limit` (`int`): daily request ceiling (e.g. `100000`)

## Push & Read Cadence

- **Host pushes (`boxd`):** `boxd` pushes to `PUT /v1/hosts/{id}/state` on state change at most once every `15s`, plus a `60s` heartbeat when idle (~2k–3k requests/day per host).
- **Failure backoff:** Failed `PUT` attempts back off starting at `60s` up to `5m` (`300s`).
- **Client reads (`box`, `box app`, `box ui`):** Clients only query `GET /v1/hosts` when a host is unreachable directly over the tailnet, keeping steady-state controller traffic low.

## v1 Scope & Open Design Decisions

- **No D1 and no R2 in v1:** The controller uses Durable Object SQLite only; R2 object storage and retention lifecycle rules arrive with retained logs (#258).
- **Hibernated WebSocket deferred:** No server-to-host push transport in v1; deferred until a feature requires it (#169 / #279).
- **Self-hosted `cmd/boxctl` deferred:** No standalone Go+SQLite controller binary in v1; S6 ships an in-memory reference `http.Handler` for Go tests.
- **Open design decisions for Tyler:**
  1. **Cloud `pull` in v1:** `box pull` is refused in `function` mode and available only during `keepSec` in `sandbox` mode until R2-backed retention (#258) lands.
  2. **Cloud host `priority` default:** Cloud hosts default to `priority: -1` when omitted in `hosts.json` so free local PC hosts are preferred before metered cloud spend.
  3. **Split pause controls:** Pausing the PC lives on the dashboard (`boxd`), while `box cloud pause` talks directly to the runner (`POST /v1/pause`), keeping Modal tokens off the PC.
