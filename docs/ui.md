# The dashboard: a page and an app for people

Planned 2026-11-24. Tyler: "a UI & app, like I have with murmur. Something very clean & modern &
light weight that gives me visibility & control into machines, boxes, agents, what they are doing,
historical data & use cases", running on the laptop and on the PC, "both [with] the same level of
control".

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
    [glaze](https://github.com/crgimenes/glaze) as `mur app` does (see Murmur's
    `docs/design/desktop.md`).
  - The tray on the PC gets an **Open dashboard** item, which opens the same window.
  - Any browser on the tailnet, a phone included.

### Why a server on the PC rather than `box ui` serving from the laptop like `mur ui`

History has to outlive the laptop being asleep, and the PC's view has to work when the laptop is
off. A laptop-side server would also need a second copy of every control for the PC. Incus's own
UI stays where it is, on port 8443, for raw admin.

## Who may look and act

- `tailscale serve` adds `Tailscale-User-Login` to each request. `boxd` accepts a request only
  when that login is in `/etc/agent-box/boxd.json` `"owners"` (Tyler's login, written by
  `setup-distro.sh`). Otherwise it answers 403. There are no tokens to leak, and a tagged device
  (a phone app, a Vi device) has no user login, so it is refused.
- Controls are `POST` only, with a JSON body and an `X-Agent-Box: 1` header. Another site can't
  send that without a CORS preflight, which `boxd` never grants.
- The repo is public: the tailnet name and logins live in config on the PC, never in code.

## What it shows

**Overview**, the first screen, answering "what's happening on my PC right now":
- each machine (today the PC; later guest hosts, #8): mode and why (Full, Shared, My PC, or
  "yielding: <game>"), what fits now (`box status`'s room), and the caps against what is
  allocated;
- each box: name, repo, size, state, memory use and CPU use, and its expiry;
- running jobs: command, agent, repo and branch, how long it has run, and a live log tail.

**Boxes**: every box with its full state. **Jobs**: running and finished, filterable by repo, agent,
machine and exit code, each with its whole log while the box still exists. **Agents**: each agent
that has used a box (see Agent identity below), with its jobs. **History**: charts over a day, a
week or a month:
- jobs per day by result;
- box-hours and CPU-hours by size;
- the busiest repos and agents;
- median and p90 run time per repo;
- a mode and yield timeline, so it is plain when games pushed agents off;
- memory use against the cap.

## Controls

The same set everywhere, since everywhere is the same page:

| Control | What `boxd` does |
|---|---|
| Mode: Full / Shared / My PC | writes `user.agent-box.mode-request` (value and time) on the project. The tray applies it, saves it to `boxtray.json` and clears it. A running game still wins: the request is applied when the yield ends. The tray stays the only writer of `user.agent-box.mode`. |
| Cancel a job | what `box cancel` does |
| Delete a box | `incus delete --force`, after a confirm on the page |
| Extend a box | adds 1h, 4h or 24h to `user.expires`, within the box's limits |
| Stop or start a box | `incus stop` / `incus start` (a stopped box keeps its disk) |

There is no create-box or run-a-command in the first version: agents run work, and the page
watches and steers.

## Data

### Agent identity

`box run` records who asked, in `/work/.box/<job>.meta` (JSON) next to `.log` and `.exit`:
- the command, the repo (remote URL without credentials), branch and commit;
- the laptop's hostname and the worktree path;
- the client: `box mcp` takes the MCP `initialize` `clientInfo` name and version, for example
  `claude-code` or `codex`; the CLI says `cli`;
- the session: the Murmur session name when the environment has one, else the T3 thread or the
  agent's own session ID if one is in the environment. Which variables exist is found and
  written down when this is built. The page shows the Murmur name when there is one, else the
  worktree's folder name;
- the size and the GPU flag.

Nothing secret: env values are never recorded, only `--env` names.

### History

- `boxd` records, as JSON lines, one file per day in `/var/lib/agent-box/history/`:
  - job started and ended (with the `.meta`, the exit code and the duration);
  - box created and deleted, with size;
  - mode and yield changes;
  - a sample per box per minute (memory, CPU time), and per machine (allocated memory, CPUs, WSL
    memory, GPU use from `nvidia-smi` when a GPU box exists).
- It keeps 180 days and a few MB a day. A JSON file per day keeps to the standard library:
  SQLite would need cgo or a large pure-Go port. The history page reads the last 30 days into
  memory at start; older days are read on demand.
- Logs are not kept: a box's logs go when the box goes (boxes are compute, never storage). History
  keeps the last 4KiB of each job's log, so a finished job still says why it failed.

### Watching Incus

`boxd` talks to Incus's REST API on its unix socket (`/var/lib/incus/unix.socket`, stdlib
`net/http` with a unix dialer) and follows `/1.0/events` for lifecycle changes, with a full refresh
every 10s as a backstop. It reads job files through the files API. It is the PC's own Incus admin,
not one of the agents' restricted certs, and it only ever touches project `agents`.

## API

JSON over HTTPS, the same origin as the page. The page polls nothing: it opens one
`GET /api/events` server-sent-events stream, and each event is a full object that replaces the old
one.

| Method and path | Returns or does |
|---|---|
| `GET /api/state` | `{machines: [Machine], boxes: [Box], jobs: [Job] (running, plus the last 50 finished), now}` |
| `GET /api/events` | SSE: `machine`, `box`, `job`, `box-gone` events, each with one object |
| `GET /api/jobs?repo=&agent=&machine=&exit=&before=&limit=` | finished jobs from history, newest first |
| `GET /api/jobs/{id}/log?offset=` | the log from `offset`, while the box exists (SSE with `follow=1`) |
| `GET /api/history?range=day\|week\|month` | `{buckets: [...]}`: per hour or day, the counts and sums the charts need |
| `POST /api/mode` | `{mode}` |
| `POST /api/jobs/{id}/cancel` | |
| `POST /api/boxes/{name}/delete` | |
| `POST /api/boxes/{name}/extend` | `{hours}` |
| `POST /api/boxes/{name}/stop`, `/start` | |

The shapes:
- `Machine`: `{id, name, mode, modeReason, yielding, caps: {cpu, memory, instances, disk},
  allocated: {...}, room: {small, medium, large, gpu}, wslMemory, gpu: {name, util, memUsed,
  memTotal} | null, boxdUp, trayUp}`.
- `Box`: `{name, machine, size, gpu, state, throwaway, repo, createdAt, expiresAt, memUsed,
  memLimit, cpus, cpuPct}`.
- `Job`: `{id, box, machine, state: running|done|paused|lost, exit, startedAt, endedAt, command,
  repo, branch, commit, agent: {client, session, host, worktree}, size, gpu, tail}`.

`internal/dash` (shared types) is the contract. The page is built against fixture JSON in
`internal/dash/testdata`, so the page and `boxd` can be built at the same time.

## Install and run

- `setup-distro.sh` builds or copies `boxd`, installs `agent-box-boxd.service`, writes
  `boxd.json`, and runs `tailscale serve --bg --https=443 http://127.0.0.1:7766` (port 7766, chosen
  so nothing else on the distro takes it). The tray's **Open dashboard** item opens
  `https://agent-box.<tailnet>.ts.net`.
- The laptop's `box ui` and `box app` get the address from the `pc` remote's host name.

## Order of work

1. `internal/dash` types and fixtures, plus `.meta` recording in `box run`/`box mcp`.
2. `boxd` read side: watch Incus, history files, `/api/state`, `/api/events`, logs, behind
   `tailscale serve` with the owner check.
3. The page, against the fixtures, then live (in parallel with 2).
4. Controls, and the tray's `mode-request`.
5. `box ui`, `box app`, and the tray's **Open dashboard**.
6. The history page and its charts.

## Not now

- Guest hosts (#8). `Machine.id` exists so a second machine is just a second entry.
- Notifications, for example a job failing or a yield starting. The app can do them as Murmur's
  does once the basics are in.
- Running commands or creating boxes from the page.
