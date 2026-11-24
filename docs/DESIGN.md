# agent-box design

On-demand compute for coding agents, on Tyler's Windows gaming PC (`desktop-l0rvq1v`, Tailscale 100.120.165.107).
Windows stays as it is: **never wipe, repartition or dual-boot the PC.** Everything runs on top of Windows.

## Goals

1. Agents on the laptop (Claude Code, Codex, ...) can get an isolated Linux box, run work in it, and throw it away.
2. Heavy jobs (Go `-race` tests, full vitest suites, Android unit tests, builds) can be offloaded from the laptop, which is short on RAM, CPU and disk.
3. Tyler decides, with one click on the PC, how much of the PC agents get: none, a share, or all of it. Games always win.

Non-goals: multi-user, public internet exposure, Kubernetes, GPU scheduling.

## v1 scope

In: boxd (status, mode, yield, boxes create/list/delete, jobs), `box` CLI (`status`, `mode`, `new`, `ls`, `rm`, `exec`, `run`, `job`), `box mcp`, boxtray (mode + gaming yield + release memory), boxhost (WSL keepalive), a read-only dashboard, one provisioning script.

Deferred (v2): Wake-on-LAN and automatic sleep (the PC stays on at first), VMs / nested virt, SSH into boxes (`box ssh`, jump host), Server-Sent Events (clients poll every 5 s), dashboard actions, GPU passthrough, Windows CI.

## Topology

```
laptop (Windows) ─┐  tailnet                   gaming PC (Windows 11, unchanged)
  box CLI         │                             ├─ boxtray.exe  tray UI: mode, gaming yield (runs after logon)
  box mcp (stdio) ├────────┐                    ├─ boxhost.exe  scheduled task at boot: keeps WSL up, holds power requests
other agents ─────┘        │                    ├─ OpenSSH :22 (admin access for setup, tailnet only)
                           │                    └─ WSL2 distro "agents" (Ubuntu 24.04, default NAT networking)
                           └──────────────────────▶ tailscaled  → its own tailnet node "agent-box"
                                                    ├─ boxd :7070 (API + dashboard)
                                                    └─ Incus: box-<name> containers on incusbr0
```

- The distro runs **its own Tailscale node** (`agent-box`), so it is reachable at `agent-box:7070` regardless of Windows networking. WSL stays on default NAT networking (mirrored mode breaks bridge NAT and needs Hyper-V firewall rules).
- boxd binds `0.0.0.0:7070` and rejects any source outside `100.64.0.0/10`, `fd7a:115c:a1e0::/48` and loopback. boxtray reaches it at `127.0.0.1:7070` through WSL's localhost forwarding.
- `/etc/wsl.conf` in the distro: `[boot] systemd=true`, `[automount] enabled=false`, `[interop] enabled=false`, so a compromised box host can't touch Windows files or run Windows programs.
- The distro's VHDX is imported onto the PC's largest drive, with `sparseVhd=true` in `.wslconfig`.

## Components and repo layout

Go module `github.com/a-Fig/agent-box`, few dependencies.

| Path | What | Runs on |
|---|---|---|
| `cmd/boxd` | daemon: Incus wrapper, modes, CPU budget, jobs, awake lease, HTTP API, dashboard | WSL distro (linux/amd64) |
| `cmd/box` | CLI + `box mcp` stdio MCP server | laptop / any agent machine (windows, darwin, linux) |
| `cmd/boxtray` | tray UI: mode switch, game detection → yield, release memory | PC after logon (windows/amd64) |
| `cmd/boxhost` | headless: keeps the distro running, holds a Windows power request while boxd says awake | PC, Scheduled Task at boot as Tyler (windows/amd64) |
| `internal/api` | the contract below as Go types + a client | everywhere |
| `internal/incus` | wrapper over the `incus` CLI (`--format json`) | boxd |
| `provision/` | `install-pc.ps1` (WSL distro, wsl.conf, .wslconfig, tasks, firewall), `setup-distro.sh` (Incus, Tailscale, boxd, image), `build-image.sh` | PC |
| `docs/` | this file, runbooks | |

## Modes

boxd owns the mode (persisted in `/var/lib/boxd/state.json`). The **effective mode** is `mypc` while yield is active, else the chosen mode.

| Mode | Boxes | CPU / RAM |
|---|---|---|
| `mypc` | running boxes are frozen (`incus pause`); new boxes and jobs are refused with 409 `mode_paused`. After `mypc_grace` (default 15 min) frozen boxes with no running job are stopped to free RAM; boxes are disposable. | n/a |
| `shared` (default) | run | budget = half the cores and half of WSL RAM, shared by all running boxes; boxd sets each box's `limits.cpu.allowance` to its share of the budget and `limits.cpu.priority=0`, and caps running boxes at `max_boxes_shared` (default 2) |
| `full` | run | no limits beyond the WSL cap; `limits.cpu.priority=10`; `max_boxes_full` (default 6) |

- **Yield**: boxtray detects gaming (Steam `HKCU\Software\Valve\Steam\RunningAppID` != 0, `SHQueryUserNotificationState` reporting a full-screen D3D app or busy, or a configurable process list) and calls `POST /v1/yield`. Clearing yield resumes frozen boxes.
- **Release memory** (tray item): stop all boxes, then `wsl --terminate agents`; boxhost restarts the distro on the next request or within 1 min if mode isn't `mypc`.
- WSL's cap in `%UserProfile%\.wslconfig`: `memory` = 50% of RAM until hardware is known, `autoMemoryReclaim=gradual`, `vmIdleTimeout=-1`.
- No Windows-side process priority changes (`vmmemWSL` is protected).

## Awake lease

Without a power request, an unattended Windows box goes back to sleep. boxd keeps `awake_until`: every authenticated API call and every running job extends it to now + 10 min. boxhost polls `/v1/status` every 30 s and holds `SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED)` while `awake_until` is in the future. boxhost also keeps the distro alive by holding `wsl -d agents --exec sleep infinity` open and restarting it if it exits.

## Box model

- A box is an unprivileged Incus container `box-<name>` from image alias `agent-base` (default), with `security.nesting=true` (Docker works inside).
- `agent-base`: Ubuntu 24.04 + git, build-essential, curl, jq, ripgrep, unzip, Go, Node LTS + npm, Python 3 + uv, OpenJDK 17, gh, user `agent` (passwordless sudo). Android SDK goes in a later `agent-android` image.
- Every box mounts the Incus volume `cache` at `/cache` (shared Go build/mod cache, npm cache, Gradle home, uv cache; env vars point there).
- Labels: `user.agent-box.owner`, `.created`, `.expires`, `.keep`, `.repo`.
- TTL: default 8h, max 7d; boxd deletes expired boxes unless `keep`.
- Busy = a job running. `idle_since` is when the last job ended.

## Workspaces (`box run`)

`box run` syncs the caller's working tree as **git objects**, not a tar of files: it stages tracked + untracked-not-ignored files into a temporary index (`GIT_INDEX_FILE`), `write-tree` + `commit-tree` (parent = HEAD), makes a `git bundle` of that commit (incremental against what the box already has), and uploads it. boxd fetches it in `/work/<repo>` inside the box and checks out the commit detached. Git normalizes line endings and keeps exec bits, and tools that call git work.

Default target: a reused box per repo, named `<repo>-<owner-host>` (created on first use, TTL refreshed by use), so caches and `node_modules` stay warm. `--new` uses a throwaway box deleted after the job.

## HTTP API (boxd)

Auth: `Authorization: Bearer <token>`. Two tokens in `/etc/boxd/tokens.json`: **admin** (everything) and **agent** (everything except `PUT /v1/mode` and `POST /v1/yield`). `GET /healthz` and the dashboard's static files need no token; the dashboard keeps the token in localStorage and sends it with `fetch`.
Errors: `{"error":"message","code":"...","retry_after_seconds":N}` with codes `bad_request` 400, `unauthorized` 401, `forbidden` 403, `not_found` 404, `conflict` 409, `mode_paused` 409, `capacity` 429, `timeout` 504, `internal` 500. `retry_after_seconds` is set for `mode_paused` and `capacity`.

| Method + path | Body / query | Response |
|---|---|---|
| `GET /healthz` | | `{"ok":true,"version":"..."}` |
| `GET /v1/status` | | `Status` |
| `PUT /v1/mode` (admin) | `{"mode":"mypc|shared|full"}` | `Status` |
| `POST /v1/yield` (admin) | `{"active":bool,"reason":"steam:570"}` | `Status` |
| `GET /v1/boxes` | | `[]Box` |
| `POST /v1/boxes` | `CreateBoxRequest` | `Box` 201 |
| `GET /v1/boxes/{name}` | | `Box` |
| `PATCH /v1/boxes/{name}` | `{"ttl":"24h","keep":true}` | `Box` |
| `DELETE /v1/boxes/{name}` | | 204 (kills its jobs) |
| `POST /v1/boxes/{name}/sync` | body: git bundle; query `repo=<name>&commit=<sha>` | `{"path":"/work/<repo>","commit":"<sha>"}` |
| `GET /v1/boxes/{name}/have?repo=<name>` | | `{"commits":["<sha>",...]}` recent commits the box has, for incremental bundles |
| `POST /v1/boxes/{name}/jobs` | `JobRequest` | `Job` 201 |
| `GET /v1/jobs` | `?box=` | `[]Job` |
| `GET /v1/jobs/{id}` | | `Job` |
| `GET /v1/jobs/{id}/logs` | `?offset=<byte>&follow=1` | `application/x-ndjson` lines: `{"stream":"stdout|stderr","offset":N,"data":"<text>"}`, `{"heartbeat":true}` every 15 s, `{"event":"paused|resumed"}`, final `{"exit":code}` |
| `DELETE /v1/jobs/{id}` | | 204 (kills it) |
| `GET /` | | dashboard |

Job logs are kept on disk in boxd (`/var/lib/boxd/jobs/<id>.log`, combined stream with offsets) for 24h, so a client that disconnects (laptop sleep, tailnet blip, MCP timeout) reconnects with `offset` and loses nothing.

```go
type Status struct {
    Version       string    `json:"version"`
    Mode          string    `json:"mode"`           // chosen: mypc|shared|full
    EffectiveMode string    `json:"effective_mode"` // mypc while yielding
    Yield         *Yield    `json:"yield,omitempty"`
    Available     bool      `json:"available"`      // can a new job start now?
    RetryAfterSeconds int   `json:"retry_after_seconds,omitempty"`
    Capacity      Capacity  `json:"capacity"`
    Host          HostInfo  `json:"host"`
    Boxes         int       `json:"boxes"`
    RunningJobs   int       `json:"running_jobs"`
    AwakeUntil    time.Time `json:"awake_until"`
}
type Yield    struct { Active bool `json:"active"`; Reason string `json:"reason"`; Since time.Time `json:"since"` }
type Capacity struct { CPUs float64 `json:"cpus"`; MemoryBytes int64 `json:"memory_bytes"`; MaxBoxes int `json:"max_boxes"`; RunningBoxes int `json:"running_boxes"` }
type HostInfo struct { CPUs int `json:"cpus"`; MemoryBytes int64 `json:"memory_bytes"`; CPUPercent float64 `json:"cpu_percent"`; MemoryUsedBytes int64 `json:"memory_used_bytes"`; DiskFreeBytes int64 `json:"disk_free_bytes"` }
type Box struct {
    Name            string    `json:"name"`
    State           string    `json:"state"` // starting|running|frozen|stopped|error
    Image           string    `json:"image"`
    IPv4            string    `json:"ipv4,omitempty"`
    Owner           string    `json:"owner"`
    Repo            string    `json:"repo,omitempty"`
    CreatedAt       time.Time `json:"created_at"`
    ExpiresAt       time.Time `json:"expires_at"`
    Keep            bool      `json:"keep"`
    Busy            bool      `json:"busy"`
    IdleSince       time.Time `json:"idle_since,omitempty"`
    CPUPercent      float64   `json:"cpu_percent"`
    MemoryUsedBytes int64     `json:"memory_used_bytes"`
}
type CreateBoxRequest struct {
    Name   string            `json:"name,omitempty"`   // [a-z0-9-]{1,40}; generated adjective-noun if empty
    Image  string            `json:"image,omitempty"`  // default agent-base
    TTL    string            `json:"ttl,omitempty"`    // Go duration, default 8h, max 168h
    Owner  string            `json:"owner,omitempty"`  // free text, e.g. "claude@laptop:murmur"
    Env    map[string]string `json:"env,omitempty"`    // set for every job in this box, e.g. GH_TOKEN
    Wait   bool              `json:"wait,omitempty"`   // block (up to 10 min) instead of 409/429 while paused or full
}
type JobRequest struct {
    Command []string          `json:"command"`           // argv; use ["bash","-lc","..."] for shell
    Cwd     string            `json:"cwd,omitempty"`     // default /work/<repo> if synced, else /home/agent
    Env     map[string]string `json:"env,omitempty"`
    Timeout string            `json:"timeout,omitempty"` // Go duration, default 1h, max 12h
    Wait    bool              `json:"wait,omitempty"`
}
type Job struct {
    ID        string     `json:"id"`
    Box       string     `json:"box"`
    Command   []string   `json:"command"`
    State     string     `json:"state"` // running|paused|exited|killed|timeout
    ExitCode  *int       `json:"exit_code,omitempty"`
    StartedAt time.Time  `json:"started_at"`
    EndedAt   *time.Time `json:"ended_at,omitempty"`
    LogBytes  int64      `json:"log_bytes"`
}
```

## CLI (`box`)

Config: `~/.config/agent-box/config.json` → `{"url":"http://agent-box:7070","token":"<agent token>"}`; env `AGENT_BOX_URL` / `AGENT_BOX_TOKEN` override. Windows: `%UserProfile%\.config\agent-box\config.json`.

```
box status [--json]
box mode [mypc|shared|full]          # needs the admin token
box new [name] [--image] [--ttl] [--owner] [--gh-token] [--wait]
box ls [--json]
box rm <name>
box keep <name> [--ttl 24h]
box exec <name> [--cwd] [--detach] [--wait] -- <cmd...>   # streams, reconnects on drop, exits with the remote code
box run [--box <name> | --new] [--detach] [--wait] -- <cmd...>  # sync working tree, run in /work/<repo>
box job <id> [--follow] [--kill]
box mcp                              # stdio MCP server
```

`box run -- go test -race ./...` from a laptop worktree is the main offload path. Exit codes: the remote exit code; 75 (EX_TEMPFAIL) when the PC is paused or full and `--wait` wasn't given, so a script can fall back to running locally; 69 when boxd is unreachable.

## MCP server (`box mcp`)

Stdio JSON-RPC (MCP protocol 2025-06-18, newline-delimited), standard library only. Tools:
`box_status`, `box_create`, `box_list`, `box_destroy`, `box_extend`, `box_run` (path of a local git worktree + command; syncs then runs), `box_exec` (box + command), `box_job` (id; returns state and output since an offset), `box_job_kill`.
`box_run` / `box_exec` take `wait_seconds` (default 120, max 600): they return the exit code and output tail (last 30 KB) if the job finishes in time, else the job id and the output so far, for `box_job` to continue.
When the PC is unavailable, tools return a clear non-error result ("PC is in My PC mode, retry in N s or run locally").

## boxtray (Windows tray UI)

Go + `fyne.io/systray` (no cgo), built with `-H windowsgui`. Started at logon (Startup shortcut or a logon task). UI only: it never keeps anything alive.
- Icon: grey `mypc`, amber `shared`, green `full`, red-outlined when boxd is unreachable; a small pause badge while yielding. Tooltip: mode, boxes, running jobs.
- Menu: status line ("Shared · 2 boxes · 1 job running"), three mode items (checkmark on the chosen one), "Pause agents while gaming" (toggle, saved in `%ProgramData%\agent-box\tray.json`), "Release memory", "Open dashboard", Exit.
- Every 5 s: game detection → `POST /v1/yield` on change; poll `GET /v1/status` and update the icon.
- Uses the admin token from `%ProgramData%\agent-box\admin-token` (written by the installer, ACL: Administrators + Tyler).

## boxhost (Windows, headless)

Scheduled Task "agent-box host", trigger at startup, runs as Tyler whether logged on or not (distros are registered per user), restart on failure. It:
1. keeps `wsl.exe -d agents --exec sleep infinity` running (restarts it with backoff; skips while a "Release memory" marker file is newer than 1 min);
2. polls `http://127.0.0.1:7070/v1/status` every 30 s and holds `SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED)` while `awake_until` is in the future;
3. logs to `%ProgramData%\agent-box\boxhost.log` (rotated at 5 MB).
If the task can't run without a logged-on user (Hello-only or Microsoft account without a password), the fallback is auto-logon + the task at logon.

## Dashboard

boxd serves one embedded static page at `/` (vanilla JS, no build step): mode and effective mode, host CPU/RAM/disk, capacity, boxes table (state, owner, repo, CPU, RAM, idle, expires) and running jobs. Read-only in v1; polls every 5 s.

## Security

- boxd accepts only tailnet and loopback sources; Windows OpenSSH's firewall rule is narrowed to `100.64.0.0/10` after setup.
- Admin vs agent tokens: agents can't change the mode.
- Boxes: unprivileged containers, no host mounts except `/cache`, no credentials unless passed per box (`--gh-token`). The distro has Windows automount and interop off.
- Note for Tyler: enabling WSL2 turns on the Virtual Machine Platform, which runs Windows under the hypervisor. It can cost a few percent of game performance and a few anti-cheat systems object. It is reversible (turn the feature off).

## Build

- `go build ./...`; cross-compile `boxd` linux/amd64, `boxtray` and `boxhost` windows/amd64, `box` windows/darwin/linux (amd64 + arm64).
- `provision/install-pc.ps1` copies binaries to `C:\Program Files\agent-box\` and sets everything up idempotently; it never touches partitions or boot settings.
- CI: GitHub Actions, `go vet ./...` and `go test ./...` on ubuntu-latest.
