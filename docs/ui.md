# The dashboard: a page and an app for people

The dashboard shows what machines, boxes and jobs are doing, and gives the owner the same
controls from the laptop, the PC and a phone.
Open it with `box ui` or `box app`; start with [Shape](#shape) for the moving parts, or jump to
[When the PC can't be reached](#when-the-pc-cant-be-reached) when it does not open.

_Origin: planned 2026-11-24, and revised the same day after a Fable review. Tyler: "a UI & app, like I have
with murmur. Something very clean & modern & light weight that gives me visibility & control into
machines, boxes, agents, what they are doing, historical data & use cases", running on the laptop
and on the PC, "both [with] the same level of control"._

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
  build step, like Murmur's `internal/web/static`. It follows Murmur's look (Tyler, 2026-11-23):
  its tokens and components, and its three themes, Dim by default (Tyler's choice) with Light and
  Dark, switched on the Settings page and remembered in the browser's `localStorage`.
- **Ways in:**
  - `box ui` (laptop) opens the page in the browser.
  - `box app` (laptop) opens it in a window of its own, through
    [glaze](https://github.com/crgimenes/glaze), as `mur app` does (see Murmur's
    `docs/design/desktop.md`). On Windows it also keeps an `agent-box` shortcut in the Start
    menu, which carries the AppUserModelID `dev.afig.agentbox` that the window's process takes,
    so the window's taskbar button pins as the shortcut and starts `box app` again
    (`internal/desktop/launcher_windows.go`, ported from Murmur's).
  - The tray on the PC gets an **Open dashboard** item, which opens the same window (not built, 2026-11-25).
  - Any browser on the tailnet, a phone included.

### Why a server on the PC rather than `box ui` serving from the laptop like `mur ui`

- History has to outlive the laptop being asleep.
- The PC's view has to work when the laptop is off.
- A laptop-side server would need a second copy of every control for the PC.

Incus's own UI stays on port 8443, for raw admin.

### When the PC can't be reached

`boxd` lives in the distro, so it is gone whenever the PC is off or asleep, or WSL has stopped. The
tray's **Release memory** does exactly that. The page then shows a clear "PC unreachable" screen
naming the likely reason (asleep, or memory released from the tray) and retries every 10s. A
browser that can't load the page at all shows its own error, so `box ui` only opens the address.
`box app` starts on the page's copy built into `box`, served on loopback, where every `/api` call
fails as the PC's would, and `/api/state` first checks whether the PC answers now: the page's
"Connecting" and "PC unreachable" screens and its 10s retry are the same, and once the PC
answers, the window goes to it (2026-11-24).

## Who may look and act

- **Who gets in.** `tailscale serve` adds `Tailscale-User-Login` to each request and strips any
  copy the client sent. Tagged devices (the Vi phones) and Funnel traffic get no such header.
  `boxd` accepts a request only when that login is in `"owners"` in `/etc/agent-box/boxd.json`;
  `setup-host.sh` writes Tyler's login there.
  - Users from other tailnets who were shared in do get a login header, so the check is an
    allowlist that fails closed when it is empty.
- **The login names Tyler, not a person at a keyboard.** Agents on the laptop also arrive as Tyler,
  so they could call the controls too. This is accepted. Every control is written to the
  history with the caller's tailnet device (from `X-Forwarded-For`, looked up with
  `tailscale whois`), and the page shows it.
- **That includes the mode**, which agents' restricted certs can't change themselves. Tyler is
  fine with that (2026-11-24): the log shows which device switched it.
- **Requests from other sites.** Controls are `POST` only, with a JSON body and an `X-Agent-Box: 1`
  header. Another site can't send that without a CORS preflight, which `boxd` never grants.
- **Agents' text is shown, never run.** Commands, branches and log tails are written by agents.
  The page sets them with `textContent` only, never as HTML, under a strict Content-Security-Policy
  (`default-src 'self'`, no inline script).
- **The spike (step 0) on the PC, 2026-11-24.** `boxd` behind `tailscale serve` answered the
  laptop with its owner's login. A `Tailscale-User-Login` header the laptop sent itself was
  replaced by the real one. A request to `127.0.0.1:7766` with no header got 403 `not-owner`. From
  inside a box, `127.0.0.1:7766`, the bridge gateway's `:7766`, and the node's tailnet address on
  80 and 443 all failed to connect, although the box resolves the `ts.net` name (public DNS
  answers with the tailnet address): `tailscaled` binds its serve listener to `tailscale0`. A
  request from a tagged device wasn't tried (none could run one); `boxd` lets in only listed
  logins, so a tagged device's missing header, or its `tagged-devices` pseudo-login, is refused
  either way (`internal/boxd` tests). Chrome on the laptop (through the Claude in Chrome
  extension) couldn't open the page or Incus's own UI on 8443, while `curl` reached both, so that
  is a browser network setting to look at in step 5, not `boxd`.
- **Boxes can't reach `boxd`.** A test from inside a box checks that neither the distro's
  `127.0.0.1:7766` nor the serve address answers: boxes are on `incusbr0` with no route to the
  host's loopback, and they aren't on the tailnet.
- **The repo is public:** the tailnet name and logins live in config on the PC, never in code.

## What it shows

**Overview**, the first screen, answering "what's happening on my PC right now":
- each machine (today the PC; later guest hosts, #8): its mode (Full, Shared or My PC), a mode
  request waiting to be applied, what fits now (`box status`'s room), the caps against what is allocated, and whether the tray is alive;
- each box: name, repo, size, state, memory and CPU use, and its expiry;
- running jobs: command, agent, repo and branch, how long they have run, and a live log tail.

**Boxes**: every box with its full state. A row opens in place (a click, or Enter on its name) to
show the job running there now, with its log's last lines, or how long the box has been idle, and
the jobs it ran before, each linked to its page (#184, 2026-11-25).

**Jobs**: running and finished, filterable by repo, agent, kind, machine and result. Each shows its
whole log while the box still exists, and the kept tail after.

**Settings** (2026-11-26, #158): the theme, a control that saves at once in the browser; and, read-only,
each host's builds of boxd and the tray, its mode and enforcer, the CPUs a box of each size gets in
Shared and Full, the caps and expiry times, and whether it has a GPU. Nothing there writes to the PC:
the mode stays on Overview and in the tray, the caps in the host's setup. `Machine.Config` carries
the sizes, times and GPU flag (boxd needs `/dev/dxg` and a project that doesn't block GPU devices, as `box` does (`block`, or the key unset on a `restricted=true` project, where Incus defaults it to block); a cap set to 0 reads 0, an unset one "no limit"); a host with no tray or no GPU says so.

**History**: charts over a day or a week:
- jobs per day by result;
- box-hours by size;
- what agents use boxes for: jobs by kind (see Use cases below);
- the busiest repos and agents;
- the median run time per repo;
- a mode timeline, with the runs it turned away (refusals), so it is plain what My PC costs
  agents;
- memory in use against the cap.

## Controls

The same set everywhere, since everywhere is the same page.

| Control | What `boxd` does |
|---|---|
| Mode: Full / Shared / My PC | writes `user.agent-box.mode-request` as `{mode, at, by}` (by = the device) on the project. See the mode handshake below. |
| Cancel a job | what `box cancel` does, except that it doesn't leave the `.cancelled` mark that keeps the agent's watcher quiet, and it adds "box: cancelled from the dashboard by <device>" to the log, so the agent hears that the job ended and why |
| Delete a box | `incus delete --force`. The page confirms only when a job on that box is starting, running or paused, or ended within the last 2 minutes (#247); otherwise the first click deletes. boxd rechecks on an unconfirmed request and refuses with the warning when work appeared after the page's last update |
| Extend a box | adds 1h, 4h or 24h to `user.expires` and holds the box until then (`user.box.hold-until`): a job's end or a launch short of room won't take that time (#198) |
| Move a queued job to the front | reorders boxd's queue (docs/queue.md) |
| Cancel a queued job | takes it out of the queue and deletes its snapshot, after a confirm on the page |

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
- **The chosen mode lives in the project: `user.agent-box.chosen`** (`{mode, at}` JSON), written by
  whichever enforcer is active when a click or a request is applied. The tray and the headless
  enforcer both adopt it when they start enforcing, so a pick made from the page while nobody is
  logged on survives the hand-over in both directions. `boxtray.json` is the tray's local copy: it
  seeds an empty project once, and every adopted pick is saved to it. The headless enforcer's Full is
  only the default until something is chosen.
- **The last choice wins**, whether it was a tray click or a page request. The tray compares the
  request's `at` with the time of its own last click.
- **The tray writes `user.agent-box.tray-seen`** (RFC3339 UTC) every 30s, plus `user.agent-box.enforcer`
  (`tray`, or `headless` while nobody is logged on). That is the dashboard's "tray alive" signal, and it
  explains a request that isn't being picked up. The headless enforcer applies requests too (see
  below), so `enforcer` only says who is in charge.

## Data

### Agent identity

`box run` records who asked, in `/work/.box/<job>.meta` (JSON) next to `.log` and `.exit`:
- the reason: one or two sentences from the agent on why this run needs a box. Tyler asked for it
  (2026-11-24). `box run --reason` and `box_run`'s `reason` are required, from 10 to 300
  characters (a `box_run` from a session with an older tool list that leaves it out is recorded as
  `(no reason given) <command>`, with a notice), and `box new` takes one too (stored as `user.box.reason`). The page shows it as the
  job's headline;
- the command, and its `kind` (see Use cases below);
- the repo (remote URL without credentials), branch and commit;
- the laptop's hostname and the worktree path;
- the client: `box mcp` takes the MCP `initialize` `clientInfo` name and version, for example
  `claude-code` or `codex`; the CLI says `cli`;
- the session, as `session` and `sessionSource`. Murmur has no variable for its session name (its
  daemon gives names out; read 2026-11-24), so none is recorded. In order: `T3CODE_THREAD_ID`
  (`t3`), the T3 thread, which Claude Code and the commands Codex runs see; a tool call's
  `_meta.threadId` (`t3`), since Codex starts MCP servers with a bare environment; then
  `CLAUDE_CODE_SESSION_ID` (`claude-code`) and `CODEX_THREAD_ID` (`codex`);
- `agent.display`, the name the page shows: the Murmur name when there is one, else the
  worktree's folder name (so, for now, always the folder);
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
(`dash.RefusalBody`: `{why, reason, size, gpu, repo, agent}`, where `reason` is the agent's and
`agent` its display name), as a best effort with a 2s timeout, ignoring errors. `why` is a cause
code: `mypc`, `full` (Incus refused the launch for the project's caps) or `gpu-busy`. A refusal that fits none, such as an unreachable PC
or a frozen box outside My PC, isn't sent. Without it the history couldn't show what My PC and
full caps cost.

### Jobs that end with their box

`box` keeps a throwaway box 30 minutes after the run's exit, but a launch short of room can delete
it sooner (#90), even inside `boxd`'s 3s poll: `boxd` would then see the box vanish with no `.exit`
read and record the job as `lost`. So after reading the exit, `box` sends `POST /api/jobs/ended`
(`{box, id, exit, endedAt, tail}`, `dash.JobEndedBody`), as a best effort with a 2s timeout,
ignoring errors, like refusals. `boxd` takes it only for a job it knows is running (404
otherwise), masks the tail as it masks every tail, records the end, and answers 204.

### History

`boxd` writes JSON lines to `/var/lib/agent-box/history/`, in two kinds of files:
- **`events-YYYY-MM-DD.jsonl`**, kept 180 days:
  - job started and job ended, with the `.meta`, exit code and duration;
  - box created and box deleted;
  - mode changes and controls (with the device; a mode request is a control);
  - refusals.
- **`samples-YYYY-MM-DD.jsonl`**, kept 14 days: a line for the machine each minute (memory used,
  the cap, what boxes hold of it, WSL's memory). Files from before 2026-11-25 also have a line per
  box, which nothing reads.

How the files are written and read:
- Every line has `"v": 1`. A reader skips a truncated last line, so a crash mid-write loses one
  line, not the file.
- After a restart, `boxd` reconciles jobs it recorded as started but never as ended. It checks the
  box: a job whose box is gone is recorded as `lost`.
- JSON lines keep to the standard library. SQLite would need cgo or a large pure-Go port.
- `boxd` reads every events file into memory at start and keeps them there. A busy day is a few
  hundred lines, so 180 days is a few MB. Samples are read per request; a finished day's hourly
  means are cached (built 2026-11-24).

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
- **Job files are read with `exec`, bounded.** Exec over REST needs a WebSocket too, so `boxd`
  runs the `incus` command for it, with a 3s timeout. Each poll runs one scan in each running box:
  every job's state, exit code and log size, plus the `.meta` and the last 2KiB of the log for
  jobs not yet recorded as ended. A log page is `tail -c +<offset> <job>.log | head -c 65536`. A
  frozen box can't run either, so its jobs are paused and its log is shown as paused, not an
  error. The unit points `INCUS_CONF` at boxd's state directory, since `ProtectHome` hides
  `/root`.
- **A throwaway can go sooner than a poll** when a launch needs its room (#90). So `box` reports each run's
  end (`POST /api/jobs/ended`), and boxd records the job from that; a job whose box went without a
  report is `lost`.
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
  - 403 when the caller isn't an owner, or a `POST` lacks `X-Agent-Box: 1`;
  - 404 for something that's gone;
  - 409 for a request that can't be applied now: cancelling a job whose box is frozen
    (`code: "frozen"`, since a frozen box can't run the kill),
    or one that has already ended (`"not-running"`); and a box control for a box whose name a
    newer box now has (`"gone"`, see below);
  - 502 when Incus failed.
  - The codes are constants in `internal/dash` (`CodeFrozen` and the rest).
- **In a path**, a job's key is one segment, escaped (`encodeURIComponent`), since it holds `/`.
- **A job's box** is the box of its name made no later than 10 minutes after the job started
  (`createdAt <= startedAt + 10m`, `dash.JobBoxGrace`): `box run --new` makes the job's ID and
  start before it launches the box, so a throwaway's `createdAt` is a few seconds after its job's
  `startedAt`. A box of the name made later is a reuse of the name, not the job's: its log is gone,
  and the page shows the kept `tail`.
- **A box's jobs** are the ones whose `boxUid` is its `uid` (`dash.Job.RanOn`). boxd records the
  `uid` (Incus's `volatile.uuid`, else `createdAt` to the nanosecond) with a job when it first finds
  the job's files on a box, so no clock decides it. A time rule can't be right both ways: one loose
  enough for `box run --new` (the ID is stamped before the bundle and launch, and the laptop's clock
  may be off) lets a replacement box made seconds later list its predecessor's jobs, and a tight one
  hides the box's own. Jobs recorded before 2026-11-25 have no `boxUid`; for them alone the old
  rule stands: a start no earlier than `createdAt` less one minute (`dash.BoxJobSkew`); boxd's
  watcher, log endpoint and reaper keep their looser ten minutes (`dash.JobBoxGrace`), since they act
  on running jobs. A scan whose box was replaced after the poll listed it is dropped. The page
  asks boxd with `box`, `machine`, `uid` and `created`, and merges the stream's jobs by the same
  rule. After a reconnect it asks again, since the new snapshot holds only the last 50, and keeps
  showing what it had until boxd answers.
- **A box's idle time** comes from the job of it that ended last, which boxd returns as the page's
  `last` (jobs overlap, so a page ordered by start may not hold it), or a later one from the stream.
  When the history can't load, the row says so with a retry, and states an idle time only from a job
  of the box it knows (from the stream, or the `last` of an earlier answer).
- **A box control names its box** by name and `createdAt`, for the same reason: delete and extend
  send `{createdAt}` (the box the page saw), `boxd` compares it with the box's own under the same
  lock as the change, and answers 409 `code: "gone"` when the name now belongs to another box. The
  page also closes a confirm whose box went or was replaced while it was open.

### Live updates

The page polls nothing. It opens one `GET /api/events` server-sent-events stream.
- The first event is `state`, the full `/api/state`.
- After that, each event carries one whole object, which replaces the old one: `machine`, `box`,
  `job`, `box-gone` and `job-gone`.
- On reconnect the page starts over from `state`. There is no replay. If `/api/state` fails too, the
  page shows "PC unreachable" and tries again every 10s.
- A running job's `job` events carry its `tail`, so Overview's log tails are live without a call.
- The one exception is an open log: the page asks `/log` again from its `offset` every 2s while the
  job runs and the answer said `eof`, and every 5s while it says `paused`.

### Endpoints

| Method and path | Returns or does |
|---|---|
| `GET /api/state` | `{machines, boxes, jobs, now}`. `jobs` holds the running, paused and queued jobs plus the last 50 finished. |
| `GET /api/events` | SSE, as above |
| `GET /api/jobs?repo=&agent=&kind=&machine=&result=&box=&uid=&created=&q=&before=&limit=` | `{jobs, more, last}`: finished jobs from history, newest first. `repo` is the URL or its last element, `agent` the session, `box` with `machine`, `uid` and `created` (its `createdAt`) one box: its jobs as above, and `last`, the one of them that ended last (400 without all four), `q` a substring of the reason, the command or the branch (each field on its own), with both sides lower-cased by Go's `strings.ToLower` (Unicode's simple mapping, code point by code point; the page folds the same way), `limit` 1 to 200 (50). Jobs are ordered by `startedAt` and then key, newest first, and `before` is the last job's key: the page after it. Start times have one-second resolution, so a time alone would skip jobs that started together. `more` says there are more. |
| `GET /api/jobs/{key}` | one `Job`, running or from history, for a job the page has let go |
| `GET /api/jobs/{key}/log?offset=` | `{offset, data, eof, paused}`, at most 64KiB per call, cut at a UTF-8 character's start. `offset` is where to ask next; `eof` means nothing more for now. 404 once the box is gone: the page shows the job's kept `tail`. |
| `GET /api/history?range=day\|week` | `{range, buckets, segments}`, where each bucket is `{start, jobs: {byResult, byKind, byRepo, byAgent}, boxHours: {bySize}, refusals: {byWhy}, medianSeconds: {byRepo}, memUsed, memCap}`, oldest first. A day has 24 hourly buckets, a week 7 daily ones, each a UTC day. `jobs` counts by start; `memUsed` is the mean of the minute samples. The mode timeline is `segments: [{start, end, mode}]`. |
| `POST /api/mode` | `{mode}`. 202 with the `Machine`, its `modeRequested` set until the tray applies it. |
| `POST /api/jobs/{key}/cancel` | 200 with the `Job`, ended with exit 130 |
| `POST /api/boxes/{name}/delete` | `{createdAt, confirmed?}`. 204. Its running jobs become `lost`. Without `confirmed`, boxd scans a Running or Frozen box fresh and classifies the scan for that request (read-only; a job can start or freeze after the last poll), rechecks identity after the scan, and answers 409 `needs-confirmation` with `{error, code, warning:{active, recent}}` when that box instance has a starting, running or paused job, or one that ended within 2 minutes. When the scan cannot establish the box's state and no work is known, it answers 502 `incus` and deletes nothing, so the page retries rather than confirming past an unknown; with `confirmed` it deletes as before. Incus deletes by name, so the window left after the final recheck is the delete call itself (#217) |
| `POST /api/boxes/{name}/extend` | `{hours, createdAt}`, `hours` one of 1, 4 or 24, added to the later of now and `user.expires`, written to `user.expires` and `user.box.hold-until`. 200 with the `Box`. |
| `POST /api/refusals` | from `box`, as above |
| `POST /api/jobs/ended` | from `box`, as above: `{box, id, exit, endedAt, tail}`; 204, or 404 for a job not running |
| `POST /api/queue/{job}/cancel` | `{}`, `job` being `<box>.<id>`. 204. 409 `not-running` unless the job is waiting. |
| `POST /api/queue/{job}/front` | `{}`. 200 with the `Job` at place 1; the others' new places follow as `job` events. 409 `not-running` unless the job is waiting. |
| `GET /api/queue/{job}`, `POST /api/queue` | the queue itself, for `box` (docs/queue.md) |

### Shapes

- **`Machine`:** `{id, name, reachable, mode, modeRequested: {mode, at, by} | null,
  trayLastSeen, enforcer: tray|headless|"", caps: {cpu,
  memory, instances, disk}, allocated: {cpu, memory, instances, disk}, room: {small, medium, large,
  gpu}, wslMemory, boxd: Version | null, tray: Version | null}`.
  `Version` is `{commit, time, modified, tag, since}`, the commit and release a binary was built
  from (`internal/version`). `boxd` is the serving boxd's; `tray` is `user.agent-box.tray-version`,
  which the active enforcer writes when it starts enforcing. `since` is when boxd first saw that build
  run: its own start, or when the tray's key took its value. The sidebar's foot shows both, with a
  note when they differ: on 2026-11-25 the PC twice ran a boxd hours older than the repo, and nothing
  showed it. Both update hourly to the latest stable release, so the note says which one updates
  ("tray updates at its hourly check"), and it turns to a warning only when the two have differed
  for over 2 hours. Which one is behind is decided by release tag, as the updaters decide
  (`update.After`): a nightly of v0.3.0 is behind v0.3.0 on the same commit. A dev build never
  updates itself, nothing updates to a nightly, and a stopped tray checks for nothing; the note says
  so instead (2026-11-25).
  `trayLastSeen` is the active enforcer's heartbeat, and `enforcer` says which one wrote it; `""` is a
  key never written, or one holding a value boxtray doesn't write.
- **`Box`:** `{name, machine, size, gpu, state, throwaway, repo, createdAt, uid, expiresAt, memUsed,
  memLimit, cpus, cpuPct}`. `uid` tells it from other boxes of its name.
- **`Job`:** `{key, id, box, boxUid, machine, state: running|done|paused|lost|queued, exit, startedAt, endedAt,
  reason, command, kind, repo, branch, commit, agent: {client, session, host, worktree}, size, gpu, tail,
  queuedAt, position, why}`. `queuedAt` is set on a job that went through the queue. While it waits,
  `startedAt` is `queuedAt`, `position` is its place from 1, and `why` is one of the refusal causes;
  every change to either comes as a `job` event. When it starts, `job-gone` takes its waiting key and
  the running job comes under its own key with the same box and ID.

### The contract and the devserver

- **The contract** is `internal/dash`: the Go types plus the `kind` table.
- **Fixtures** are generated from those types in a golden test, one per scenario
  (`internal/dash/fixture` builds them; `go test ./internal/dash -run Golden -update` writes
  `internal/dash/testdata/<scenario>.json`, built at 2026-11-24T17:30:00Z):
  - empty;
  - busy (three boxes, two running jobs);
  - My PC, with a job paused and two jobs queued;
  - full, with refusals and three jobs queued;
  - tray down;
  - headless: nobody logged on, the headless enforcer holding Full, a mode request waiting for the
    tray;
  - a lost job;
  - a week of history.
- **The devserver** is `internal/dash/devserver`, like Murmur's `internal/web/devserver`. It serves
  the page with a chosen fixture, replays SSE events on a timer, and fakes the controls. The page is
  built against it, so the page and `boxd` are built at the same time.
  - `go run ./internal/dash/devserver -scenario busy` (on 127.0.0.1:7767). `-static
    internal/dash/static` serves the page from disk so an edit shows on reload; `-still` stops the
    replay; `-unreachable` starts with the API down, and `GET /dev/down` and `/dev/up` switch it;
    `-fail` makes every control answer 502.
  - The page is `internal/dash/static`, embedded; `static.Handler` serves it with its
    Content-Security-Policy, which `boxd` reuses.

## Install and run

- `setup-host.sh` installs `boxd` and `agent-box-boxd.service`, and writes `boxd.json`. It also
  runs `tailscale serve --bg --https=443 http://127.0.0.1:7766`; port 7766 was chosen so nothing
  else on the distro takes it.
- The tray's **Open dashboard** item opens `https://agent-box.<tailnet>.ts.net` (not built, 2026-11-25).
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
4. **The controls,** with the tray's mode request and the tray's heartbeat.
5. **`box ui`, `box app`,** the tray's **Open dashboard** (not built, 2026-11-25), and the "PC unreachable" screen.
6. **The history page and its charts.**

## Not now

- Guest hosts (#8). `Machine.id` exists so that a second machine is just a second entry.
- Notifications, for example a job failing or the mode changing. The app can do them as Murmur's
  does once the basics are in.
- A separate Agents page: it is a filter on Jobs for now.
- GPU sampling, p90s and a month range.
