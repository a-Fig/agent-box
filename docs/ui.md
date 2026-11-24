# The dashboard: a page and an app for people

Planned 2026-11-24, and revised the same day after a Fable review. Tyler: "a UI & app, like I have
with murmur. Something very clean & modern & light weight that gives me visibility & control into
machines, boxes, agents, what they are doing, historical data & use cases", running on the laptop
and on the PC, "both [with] the same level of control".

## Shape

One server, one page, three ways in.

- **`boxd`** is the server: a Go binary in the `agents` distro on the PC, run by systemd. It is
  the one source of truth: it watches Incus, keeps the history, serves the page and its JSON API,
  and carries out the controls. One server means the laptop, the PC and a phone see the same thing
  and can do the same things, with nothing to keep in sync.
- **Reached over the tailnet only**, at `https://agent-box.<tailnet>.ts.net` (port 443), through
  `tailscale serve`, which gives it HTTPS and tells it who is asking. `boxd` itself listens on
  127.0.0.1 only.
- **The page** is static files embedded in `boxd`: plain HTML, CSS and JS with no framework and no
  build step, like Murmur's `internal/web/static`. It follows Murmur's look (Tyler, 2026-11-23),
  light and dark.
- **Ways in:**
  - `box ui` (laptop) opens the page in the browser.
  - `box app` (laptop) opens it in a window of its own, through
    [glaze](https://github.com/crgimenes/glaze), as `mur app` does (see Murmur's
    `docs/design/desktop.md`).
  - The tray on the PC gets an **Open dashboard** item, which opens the same window.
  - Any browser on the tailnet, a phone included.

### Why a server on the PC rather than `box ui` serving from the laptop like `mur ui`

- History has to outlive the laptop being asleep.
- The PC's view has to work when the laptop is off.
- A laptop-side server would need a second copy of every control for the PC.

Incus's own UI stays on port 8443, for raw admin.

### When the PC can't be reached

`boxd` lives in the distro, so it is gone whenever the PC is off or asleep, or WSL has stopped. The
tray's **Release memory** does exactly that. The app and `box ui` check the address first. If it
doesn't answer, they show a clear "PC unreachable" screen naming the likely reason (asleep, or
memory released from the tray) and retry every 10s, rather than leaving a browser error.

## Who may look and act

- **Who gets in.** `tailscale serve` adds `Tailscale-User-Login` to each request and strips any
  copy the client sent. Tagged devices (the Vi phones) and Funnel traffic get no such header.
  `boxd` accepts a request only when that login is in `"owners"` in `/etc/agent-box/boxd.json`;
  `setup-distro.sh` writes Tyler's login there.
  - Users from other tailnets who were shared in do get a login header, so the check is an
    allowlist that fails closed when it is empty.
- **The login names Tyler, not a person at a keyboard.** Agents on the laptop also arrive as Tyler,
  so they could call the controls too. This is accepted, because the controls do no more than
  `box` already lets agents do, with one exception, the mode. Every control is written to the
  history with the caller's tailnet device (from `X-Forwarded-For`, looked up with
  `tailscale whois`), and the page shows it.
- **Mode is the one control agents can't otherwise reach.** A mode request is honoured only when
  the browser that sent it presents a session cookie. `boxd` sets that cookie only after a
  click-through on the page: an HTML confirm that the page itself serves, which needs a real
  browser with JS. That stops a casual `curl`, not a determined agent. The skill never mentions
  the API.
- **Requests from other sites.** Controls are `POST` only, with a JSON body and an `X-Agent-Box: 1`
  header. Another site can't send that without a CORS preflight, which `boxd` never grants.
- **Agents' text is shown, never run.** Commands, branches and log tails are written by agents.
  The page sets them with `textContent` only, never as HTML, under a strict Content-Security-Policy
  (`default-src 'self'`, no inline script).
- **Boxes can't reach `boxd`.** A test from inside a box checks that neither the distro's
  `127.0.0.1:7766` nor the serve address answers: boxes are on `incusbr0` with no route to the
  host's loopback, and they aren't on the tailnet.
- **The repo is public:** the tailnet name and logins live in config on the PC, never in code.

## What it shows

**Overview**, the first screen, answering "what's happening on my PC right now":
- each machine (today the PC; later guest hosts, #8): its mode and why (Full, Shared, My PC, or
  "yielding: <game>"), a mode request waiting on a game ("Full when the game ends"), what fits now
  (`box status`'s room), the caps against what is allocated, and whether the tray is alive;
- each box: name, repo, size, state, memory and CPU use, and its expiry;
- running jobs: command, agent, repo and branch, how long they have run, and a live log tail.

**Boxes**: every box with its full state.

**Jobs**: running and finished, filterable by repo, agent, kind, machine and result. Each shows its
whole log while the box still exists, and the kept tail after.

**History**: charts over a day or a week:
- jobs per day by result;
- box-hours by size;
- what agents use boxes for: jobs by kind (see Use cases below);
- the busiest repos and agents;
- the median run time per repo;
- a mode and yield timeline, with the runs it turned away (refusals), so it is plain what games
  cost agents;
- memory in use against the cap.

## Controls

The same set everywhere, since everywhere is the same page.

| Control | What `boxd` does |
|---|---|
| Mode: Full / Shared / My PC | writes `user.agent-box.mode-request` as `{mode, at, by}` (by = the device) on the project. See the mode handshake below. |
| Cancel a job | what `box cancel` does |
| Delete a box | `incus delete --force`, after a confirm on the page |
| Extend a box | adds 1h, 4h or 24h to `user.expires` |

- **Not in the first version:** stop or start a box (a stopped box still holds the caps), creating
  boxes or running commands. Agents run work; the page watches and steers.
- **Release memory stays tray-only**, because it stops `boxd` itself.

### The mode handshake

- **The tray stays the only writer of `user.agent-box.mode`.** `boxd` writes only
  `mode-request`. Both patch single keys (`incus project set`, or a PATCH of one key), never a PUT
  of the whole project config.
- **The tray applies a request** within one tick: it saves the request to `boxtray.json`, then
  clears the key.
  - A request older than 10 minutes is dropped, so one written while the tray was down isn't
    applied days later.
  - A running game still wins. The request waits until the yield ends, and the page shows it as
    pending.
- **The last choice wins**, whether it was a tray click or a page request. The tray compares the
  request's `at` with the time of its own last click.
- **The tray writes `user.agent-box.tray-seen`** (a time) every 30s. That is the dashboard's "tray
  alive" signal, and it explains a request that isn't being picked up.

## Data

### Agent identity

`box run` records who asked, in `/work/.box/<job>.meta` (JSON) next to `.log` and `.exit`:
- the command, and its `kind` (see Use cases below);
- the repo (remote URL without credentials), branch and commit;
- the laptop's hostname and the worktree path;
- the client: `box mcp` takes the MCP `initialize` `clientInfo` name and version, for example
  `claude-code` or `codex`; the CLI says `cli`;
- the session: the Murmur session name when the environment has one, else the T3 thread or the
  agent's own session ID if one is in the environment. Which variables exist is found and written
  down when this is built. The page shows the Murmur name when there is one, else the worktree's
  folder name;
- the size and the GPU flag.

It is self-reported by the agent's own cert, so it is for display, never for decisions. `--env`
values are never recorded, only their names.

### Use cases

`kind` comes from the command by a small table of patterns, kept in one place in `internal/dash`.
A command that matches none is `other`.

| kind | patterns |
|---|---|
| `go-test` | `go test` |
| `go-build` | `go build`, `go vet` |
| `js-test` | `vitest`, `jest`, `npm test`, `npm run test` |
| `py-test` | `pytest`, `uv run pytest` |
| `gradle` | `gradle`, `./gradlew` |
| `build` | `make`, `cargo build`, `npm run build` |
| `gpu` | any run on a GPU box |

### Refusals

A run turned away with exit 75 never reaches a box, so `box` also sends `POST /api/refusals`
(`{reason, size, gpu, repo, agent}`), as a best effort with a 2s timeout, ignoring errors. Without
it the history couldn't show what yields and full caps cost.

### History

`boxd` writes JSON lines to `/var/lib/agent-box/history/`, in two kinds of files:
- **`events-YYYY-MM-DD.jsonl`**, kept 180 days:
  - job started and job ended, with the `.meta`, exit code and duration;
  - box created and box deleted;
  - mode and yield changes, mode requests, and controls (with the device);
  - refusals.
- **`samples-YYYY-MM-DD.jsonl`**, kept 14 days: a line per box and one for the machine each minute
  (memory, CPU time, allocated memory).

How the files are written and read:
- Every line has `"v": 1`. A reader skips a truncated last line, so a crash mid-write loses one
  line, not the file.
- After a restart, `boxd` reconciles jobs it recorded as started but never as ended. It checks the
  box: a job whose box is gone is recorded as `lost`.
- JSON lines keep to the standard library. SQLite would need cgo or a large pure-Go port.
- The history API reads the last 7 days into memory at start; older days are read on demand.

**Secrets in the history.** Commands and log tails can hold secrets, and the history keeps them
longer than the boxes do. Before writing, `boxd` masks anything that looks like a token in both:
strings matching `box`'s own secret-name rule, `KEY=value` pairs whose name matches it, and
high-entropy strings of 32 characters or more. A job's kept tail is its last 2KiB, kept 30 days,
then dropped from the event.

Logs themselves aren't kept: a box's logs go when the box goes, since boxes are compute, never
storage.

### Watching Incus

`boxd` talks to Incus's REST API on its unix socket (`/var/lib/incus/unix.socket`, with stdlib
`net/http` and a unix dialer).
- **It polls rather than following events.** `/1.0/events` is WebSocket-only, and the standard
  library has no WebSocket client. `boxd` polls `GET /1.0/instances?project=agents&recursion=2`
  every 3s, which is a single call for ten boxes at most, and diffs the result to make its own
  events.
- **Job files are read with `exec`, bounded.** The command is
  `tail -c +<offset> <job>.log | head -c 65536`, with a 3s timeout. A frozen box can't run it, so
  its log is shown as paused; it isn't an error.
- `.meta` and `.exit` are read the same way, once per job.
- **Its access.** `boxd` uses the PC's own Incus admin access, not one of the agents' restricted
  certs, and it only ever touches project `agents`.

## API

JSON over HTTPS, on the same origin as the page.

### Conventions

- **Times** are RFC3339 UTC. **Sizes** are bytes.
- **`cpuPct`** is the share of the box's own CPU limit, from 0 to 100.
- **A job's key** is `box/id/startedAt`, since box names and job IDs get reused.
- **Errors** are `{error, code}`:
  - 400 for a bad request;
  - 403 when the caller isn't an owner;
  - 404 for something that's gone;
  - 409 for a request that can't be applied now (`code: "yielding"`, `"no-session"` and so on);
  - 502 when Incus failed.

### Live updates

The page polls nothing. It opens one `GET /api/events` server-sent-events stream.
- The first event is `state`, the full `/api/state`.
- After that, each event carries one whole object, which replaces the old one: `machine`, `box`,
  `job`, `box-gone` and `job-gone`.
- On reconnect the page starts over from `state`. There is no replay.

### Endpoints

| Method and path | Returns or does |
|---|---|
| `GET /api/state` | `{machines, boxes, jobs, now}`. `jobs` holds the running jobs plus the last 50 finished. |
| `GET /api/events` | SSE, as above |
| `GET /api/jobs?repo=&agent=&kind=&machine=&result=&before=&limit=` | finished jobs from history, newest first |
| `GET /api/jobs/{key}/log?offset=` | `{offset, data, eof, paused}`, at most 64KiB per call |
| `GET /api/history?range=day\|week` | `{buckets}`, where each bucket is `{start, jobs: {byResult, byKind, byRepo, byAgent}, boxHours: {bySize}, refusals: {byReason}, medianSeconds: {byRepo}, memUsed, memCap}`. A day has hourly buckets, a week daily ones. The mode timeline is a separate `segments: [{start, end, mode, reason}]`. |
| `POST /api/mode` | `{mode}`; needs the session cookie |
| `POST /api/session` | sets the session cookie after the page's confirm |
| `POST /api/jobs/{key}/cancel` | |
| `POST /api/boxes/{name}/delete` | |
| `POST /api/boxes/{name}/extend` | `{hours}` |
| `POST /api/refusals` | from `box`, as above |

### Shapes

- **`Machine`:** `{id, name, reachable, mode, modeReason, modeRequested: {mode, at, by} | null,
  yielding, trayLastSeen, caps: {cpu, memory, instances, disk}, allocated: {cpu, memory, instances,
  disk}, room: {small, medium, large, gpu}, wslMemory}`
- **`Box`:** `{name, machine, size, gpu, state, throwaway, repo, createdAt, expiresAt, memUsed,
  memLimit, cpus, cpuPct}`
- **`Job`:** `{key, id, box, machine, state: running|done|paused|lost, exit, startedAt, endedAt,
  command, kind, repo, branch, commit, agent: {client, session, host, worktree}, size, gpu, tail}`

### The contract and the devserver

- **The contract** is `internal/dash`: the Go types plus the `kind` table.
- **Fixtures** are generated from those types in a golden test, one per scenario:
  - empty;
  - busy (three boxes, two running jobs);
  - yielding to a game, with a mode request pending;
  - full, with refusals;
  - tray down;
  - a lost job;
  - a week of history.
- **The devserver** is `internal/dash/devserver`, like Murmur's `internal/web/devserver`. It serves
  the page with a chosen fixture, replays SSE events on a timer, and fakes the controls. The page is
  built against it, so the page and `boxd` are built at the same time.

## Install and run

- `setup-distro.sh` installs `boxd` and `agent-box-boxd.service`, and writes `boxd.json`. It also
  runs `tailscale serve --bg --https=443 http://127.0.0.1:7766`; port 7766 was chosen so nothing
  else on the distro takes it.
- The tray's **Open dashboard** item opens `https://agent-box.<tailnet>.ts.net`.
- The laptop's `box ui` and `box app` get that address from the `pc` remote's host name.

## Order of work

0. **A spike first:** `tailscale serve` plus the owner check, with a hello page on the real PC. It
   tests the riskiest unknown first: the header from a laptop browser, from the phone, and a tagged
   device refused.
1. **The contract:** the `internal/dash` types, the kind table, the fixtures and the devserver. Also
   `.meta` recording and refusals in `box run`/`box mcp`.
2. **`boxd`'s read side:** polling Incus, the history files, `/api/state`, `/api/events`, logs and
   the owner check. History is recorded from here on.
3. **The page**, against the devserver, then live. This runs in parallel with 2.
4. **The controls,** with the session cookie, the tray's mode request and the tray's heartbeat.
5. **`box ui`, `box app`,** the tray's **Open dashboard**, and the "PC unreachable" screen.
6. **The history page and its charts.**

## Not now

- Guest hosts (#8). `Machine.id` exists so that a second machine is just a second entry.
- Notifications, for example a job failing or a yield starting. The app can do them as Murmur's
  does once the basics are in.
- A separate Agents page: it is a filter on Jobs for now.
- GPU sampling, p90s and a month range.
