# Project Fano

Turns a spare Linux server or Windows gaming PC into on-demand, disposable Linux boxes that coding agents run heavy work on: test suites, `-race`, and builds. Named for Robert Fano (1917–2016), who founded MIT's Project MAC in 1963 and pioneered time-sharing.

> Early work in progress — built for daily use on one setup first and not ready for outside users yet. The repository and CLI still use `agent-box` and `box`. [Project Fano website](https://fano.afig.dev/)

- **Incus** on the host does the boxes, API, auth and web UI.
- **box** is a thin sync-and-run CLI + MCP for agents: `box run --new --reason "..." -- go test ./...`.
- **boxtray**, on a Windows host, sits in the tray: one click picks *My PC*, *Shared* or *Full*. Without it a host stays in Shared.

See the [documentation map](docs/README.md) for the right page, or [docs/DESIGN.md](docs/DESIGN.md) for how it works.

## Quick start: a Linux server

**Host** (Ubuntu or Debian, kernel 6.12+, 6GiB RAM and 75GiB free disk, as root). `provision/setup-host.sh` sets up Incus and the `agents` project, builds the `agent-base` image boxes start from, then prints the line a client adds the host with. Ubuntu 24.04 ships 6.8: first `sudo apt install linux-generic-hwe-24.04` and reboot.

```sh
sudo apt install -y git
git clone https://github.com/a-Fig/agent-box && sudo agent-box/provision/setup-host.sh --build-base
```

Setup takes about 10 minutes, most of it the image build. The pool is 80% of the free disk under `/var/lib` and needs 60GiB: the 30GiB cache and one medium box ([provision/README.md](provision/README.md#a-linux-server) has the caps).

**Client** (each machine your agents run on: Linux, macOS or Windows). Install `box` and the `incus` client, then add the host with the line setup printed.

```sh
curl -fsSL https://raw.githubusercontent.com/a-Fig/agent-box/main/install.sh | sh
# Windows PowerShell: irm https://raw.githubusercontent.com/a-Fig/agent-box/main/install.ps1 | iex
incus remote add pc https://<host>:8443 --token <token>
```

**Agent.** `box join` gives Claude Code, Codex and Antigravity the box tools, where they are installed. Or hand your agent [docs/agents/join.md](docs/agents/join.md), which also covers Cursor.

```sh
box join
```

A Windows gaming PC as the host, with the tray, is set up with [provision/README.md](provision/README.md).

## How fast

Measured runs, through `box run`:

| Run | Box | Time | Date |
|---|---|---|---|
| Murmur's `go test -race ./internal/daemon` (913 tests) | large | 420s, against 1,013s on the laptop | 2026-11-25 |
| pi-vi's daemon, UI and vm/provision suites (5,716 tests) | medium | about 4 min | 2026-11-25 |
| pi-vi's `:vi:testDebugUnitTest --rerun` (Android, Robolectric), caches cold | medium | 6m18s | 2026-11-24 |
| the same, caches warm, in a new throwaway box | medium | 1m05s (80s with launch and sync) | 2026-11-24 |
| PyTorch 4096×4096 matmul, CPU against the RTX 3070 | gpu | 590ms against 10ms | 2026-11-25 |

## Using `box`

Install it with the quick start above, or build it with `go build ./cmd/box`. It needs the `incus` client with a remote named `pc` that can reach the `agents` project.

```sh
box status                            # worth offloading now? PC, mode, room per size, this repo's box
box hosts                             # list configured hosts and their current capacity
box run --reason "Full test suite for the parser change." -- go test ./...
                                      # sync this worktree to the repo's box and run there
                                      # (--reason is required; left out below)
box run --new -- 'npm ci && npx vitest run'   # throwaway box, kept up to 30 minutes for box pull
box run --box big -- make test        # a box you name
box run --size large -- ./gradlew test  # size of a box the run launches: small, medium (default), large
box run --gpu -- python train.py      # throwaway box with the PC's RTX 3070 (CUDA); one at a time
box run --env CI=1 --timeout 30m -- npm test  # env for the command; killed with exit 124 after 30m
box job agent-box.20261123-120000-1a2b  # reattach to a job (the ID is printed at the start)
box job <id> --offset 65536 --limit 32768  # read a piece of the log without waiting
box jobs [--box agent-box]            # jobs, newest first: running, done (exit code), died
box cancel <id>                       # stop a job and everything it started (exit 130)
box pull <id> coverage ./out          # copy /work/<repo>/coverage into ./out/coverage
box new scratch --ttl 2h --size small # launch a box (sets user.expires and its size)
box ls                                # boxes, status, expiry
box rm scratch
box ui                                # the PC's dashboard in the browser
box app                               # the dashboard in a window of its own (Windows; pinnable)
box --version                         # the release and commit this box was built from
box update                            # update box now (box mcp checks hourly); --rollback swaps back
```

`box run --reason` (required, 10 to 300 characters) says why the run needs a box; `box new --reason` stores one as `user.box.reason`. `box run` records it in `/work/.box/<job>.meta` with the command, repo, branch, commit, size, `--env` names (never values) and who asked, and reports to the dashboard, best effort, a run turned away with exit 75 and how each run ended. Both are self-reported, for display ([docs/ui.md](docs/ui.md#agent-identity)).

`box run` snapshots the worktree, including uncommitted and untracked files, without touching your index. It skips files that are both untracked and ignored. It checks the snapshot out in `/work/<repo>` and runs the command there, logging to `/work/.box/<job>.log` for reattachment. One argument runs as a bash string with `pipefail`, so `go test ./... | tail` exits with `go test`'s code (and `x | head` can exit 141). Several are quoted word by word. When the command runs `go test` or vitest (`vitest`, `npx`/`pnpm`/`yarn vitest`, or `npm test` / `npm run <script>`, including single-directory `-w`/`--prefix`, whose script is plain `vitest`), box adds `-json` or vitest's JSON reporter and keeps the report beside the log, which stays close to the tool's own output ([details](docs/DESIGN.md#failure-digest)); the result then leads with a digest of at most 3KiB: the failed tests, each with file:line and its first assertion or panic line, then the counts. A run that can't start now (no room, the GPU taken, My PC) waits in the PC's queue, and `box run` follows it there and then in its box. Ctrl-C leaves it queued, and `box job` follows it again. `--no-queue` exits 75 at once instead, and a GPU run doesn't wait out My PC. The command returns its exit code, or **75** when the box is paused, the PC can't be reached or the run wasn't queued: box couldn't run it now, and nothing started. If the box is frozen while a job runs, `box run` and `box job` stop following at once and exit 75 with `job X is paused`. The job resumes if the PC's owner switches back within 15 minutes, or 4 hours once the tray has moved the box's memory to swap; after that the tray stops the box and the job is lost. `box job X` reattaches later. Runs on the same repo and box take turns. The job ID is printed before the job starts. Shallow clones produce snapshots without history. If the repo has checked-out submodules, box warns that their directories will be empty in the box.

Commands run as root, unlike most CI. Root can change a test's result, for example a test that expects a permission error, or code that does something extra as root. Such a test can run as the box's `ubuntu` user (uid 1000): `chown -R ubuntu /work/<repo> && runuser -u ubuntu -- bash -lc 'cd /work/<repo> && <test>'`. One pi-vi test failed as root and passed this way (2026-11-25).

Boxes are disposable compute. Idle boxes expire after two hours by default. Only package caches in `/cache` survive deletion. `box new --ttl` sets the initial lifetime and holds the box that long. In project `agents`, the host protects a running job even when the client disconnects. Custom projects keep a 24-hour job expiry. Once the job ends, the box is kept for two hours (30 minutes for a `--new` throwaway). Set `--timeout` to limit the command's runtime. None of these cut short a hold: an extend from the dashboard, or `box new --ttl`, keeps the box until the time it set (`user.box.hold-until`).

Sizes are small (1 CPU in Shared, 2 in Full, 2GiB RAM, 18GiB disk), medium (2/4, 4GiB, 30GiB, the default) and large (4/8, 8GiB, 40GiB). A box records its size in `user.box.size` and is never resized. `--size` picks the size of a box the run launches. If the repo's box exists in another size, the run uses a throwaway box of the requested size and says so. A launch Incus refuses for the project's limits first deletes throwaways kept after their job (30 minutes, for `box_pull`), the first to expire first, then repo boxes idle since their last job, the longest idle first, but only if deleting them would make the box fit, and never a held box (an extend or `box new --ttl`); the run's output names each one (`deleted idle box murmur to make room`). If it still doesn't fit, `box run` waits in the PC's queue (`--no-queue`: exit 75) and `box new` exits 75. `box status` counts kept throwaways and idle repo boxes as free room. [Box sizes](docs/DESIGN.md#box-sizes)

`--gpu` (`--size gpu`: 4/6 CPUs, 8GiB, 40GiB) gives the box the RTX 3070 through WSL's `/dev/dxg`, for CUDA or PyTorch training and inference and GPU builds. GPU runs always use a throwaway box, and only one GPU box runs at a time: another GPU run waits in the queue (with `--no-queue` it exits 75: `the GPU box is in use (job X)`). Because a frozen CUDA process keeps its VRAM, the tray stops a GPU box instead of pausing it when the PC's owner picks My PC; its job is lost and `box job` exits 75 saying so. `box status` has a `gpu:` line: free, in use, or unavailable. PyTorch needs nothing in the image: `uv run --no-project --with torch python x.py` works. Its first install downloads about 3GB (7m46s on 2026-11-25); after that the shared uv cache brings it to about 12s. My PC keeps a stopped GPU box for 30 minutes, and `box pull` gets its files. [The GPU box](docs/DESIGN.md#the-gpu-box)

`--new` uses a box marked `user.box.throwaway=true`. CPU runs can claim an idle throwaway for the same HTTP(S) Git origin and size when boxd supports reuse. Use `--fresh` (`fresh: true` in MCP) for a clean box or to keep a separate checkout for artifacts. Once the job ends the box is kept for up to 30 minutes, then the reaper deletes it. If the host enables idle cooling, boxd can stop finished CPU throwaways during that window to release RAM. Log paging and `box pull` still work while their files are available. A queued job or another launch that needs the room deletes the box sooner, and `box pull` and `box job` then say so. `box rm` deletes it at once. Use `--box` with a reusable box. Box retries a launch once if the Incus image lookup ends with EOF. If the launch still fails after losing its connection, look for the printed box name in `box ls` and remove it with `box rm` if it was created.

`box status` prints one advice line first, like `available: room for 1 large, or 2 medium, or 4 small` or `unavailable: PC is in My PC mode: box run, or box_run with push, would wait in the PC's queue (a GPU run doesn't wait)`, then the mode, free box slots (`limits.instances` minus existing boxes), what fits (the project caps minus existing boxes' Full CPUs, memory and disk; the memory cap is 24GiB in Full and 12GiB otherwise), CPUs and RAM per size now, and this repo's box with its size and running jobs. It exits 75 when offloading isn't possible now.

Job results and completion notices show the source HEAD, whether the synced tree differed from it, and the bundled snapshot commit. Queued jobs report the snapshot sent when they entered the queue. Older records can show `source unknown`. MCP results also carry these details in `structuredContent.source`.

`--env K=V` (repeatable) passes variables to the command. Boxes are shared and the values are visible in the box, so names containing `TOKEN`, `SECRET`, `KEY`, `PASSWORD`, `PASSWD` or `CREDENTIAL` are refused. `--timeout` returns exit `124` and force-kills a command that ignores TERM for 10 seconds. `box cancel` stops the job's session and writes exit code `130` if the wrapper hasn't already recorded an exit. `box pull <job|box> <remote-path> <local-dir>` copies a file or directory into `<local-dir>/<name>`. A relative remote path starts at the job's checkout; an absolute path starts with `/` (in Git Bash, set `MSYS_NO_PATHCONV=1` so `/...` isn't rewritten to a Windows path). It never replaces an existing file and drops symlinks, so nothing lands outside `<local-dir>`. Pull right after the job (or run with `--fresh`): if a later job replaced the checkout, pulling by the earlier job ID refuses instead of returning the wrong files.

`box mcp` is a stdio MCP server with five tools, `box_status`, `box_run`, `box_job`, `box_cancel` and `box_pull`, which behave like the commands above. Creating, listing and deleting boxes (`new`, `ls`, `rm`) and `--box` are CLI only. Results start with a line that carries the key facts, and most also return `structuredContent`. `box_run` and `box_job` put the job ID, box, exit code and any warnings ahead of the output tail, and `structuredContent` repeats the tail (`tail`, or `log` for a page), since Claude Code shows it in place of the text. `box_job` with `offset` or `limit` returns a page of the log and `nextOffset` without waiting, and without `id` it lists jobs. If a page contains incomplete UTF-8, `logBase64` in `structuredContent` preserves the original bytes. Info-level notifications announce the throwaway box before launch and the job ID before the job starts. Cancelling a call stops its setup, or stops following a started job without killing it.

`box_run` returns the result inline if the job ends within 30 seconds of the command starting. Otherwise it returns `state: running` (`exitCode: null`) and `push`. When `push` is true, box has a T3 thread for this session (`T3CODE_THREAD_ID`, or Codex's `_meta.threadId`) and a detached `box watch` process on the laptop follows the job and sends the result into that thread with `t3 thread send`, as a turn starting `box: job ...`, so the agent can end its turn instead of waiting. The message is short, since the full result is a `box_job` call away and the agent may read it there too: a first line (job ID, exit code, how long, the run's reason cut to 80 characters, and a passing test run's digest such as `PASS: 2325 tests passed, 28 skipped (go test)`), then for any exit 0 the log's last 3 lines, since a digest can miss a failure; a test failure is its digest (the failed tests with file:line and first assertion or panic) and the log's last 3 lines; any other failure is the digest, if any, and the last 10 lines. It is masked like the dashboard's history. A throwaway box and its log are kept up to 30 minutes after the job, so `box_pull` still works unless another run needed the room. Once box has read a job's exit it keeps the exit code and the last 32KiB of output on the laptop, so `box_job` still gives them after the box is gone (a launch short of room can take a kept throwaway early). When `push` is false (no T3 thread, or `push: false`, which subagents pass) the agent calls `box_job`, which waits up to 10 minutes and always returns the full result. When a T3 profile launches Box MCP again, Box retries its pending notices after any old leases expire. A later `box_*` call still shows `finished since you last looked: ...` for that thread and can recover older records. With `push`, a run that can't start now (no room, the GPU taken, My PC) is handed to boxd's queue instead of refused. `box_run` sends the worktree's snapshot, the same git bundle a run pushes, and returns `state: queued` with the job ID, its `position` in line and `why` it waits. The watcher follows it through the queue, pushes one nudge if it has waited 20 minutes, and pushes the result when it has run, or says it left the queue without running (boxd drops a job after 24 hours). `box_cancel` takes a queued job off the queue, and `box_job` says where it is in line. `queue: false` turns the queue off for that run. A GPU run during My PC, a run without push, and a run the queue can't take still come back at once as `state: refused` (exit 75), which is never pushed, nor is a job the agent cancelled. [The queue](docs/queue.md) A notice can arrive twice if `box_job` and the watcher race or if the watcher stops after T3 accepts a notice and before Box saves the acknowledgement. `box_run` takes no `box` or `wait`. A session holding an older box's tool list may still send them, or leave out `reason`: the run goes ahead with a notice, the reason recorded as `(no reason given) <command>`, and the server sends `notifications/tools/list_changed` at startup so clients reload the tools. [Pushing results to the agent](docs/DESIGN.md#pushing-results-to-the-agent)

`box join` sets up only the agents it finds: one whose CLI is on PATH or whose config folder exists (`~/.claude` or `$CLAUDE_CONFIG_DIR`, `~/.codex` or `$CODEX_HOME`, the Antigravity homes below). It says which it set up and which it skipped; with none, it changes nothing and prints the server entry to add by hand. It registers `box mcp` as a user-scope MCP server: Claude Code and Codex via their CLIs (one whose folder exists but whose CLI isn't on PATH gets the skill but not the server), and Antigravity via `config/mcp_config.json` in `$GEMINI_HOME`, `~/.gemini`, and T3 Code's Antigravity provider homes (`~/.t3/userdata/providers/antigravity/*`, or `$T3CODE_HOME`). It installs the [box skill](skills/box/SKILL.md) in `skills/box/` of each Claude Code and Codex folder it found, and `config/skills/box/` in each Antigravity home. Run `box join` from a built binary at its final path. In each Claude Code folder it also adds `box hook` to the `hooks.PostToolUse` of `settings.json`: T3 holds a pushed result until the agent's turn ends, so the hook shows a busy agent its ended jobs after its next tool call instead, and the watcher then sends nothing. `box join --leave` unregisters the servers and removes the installed skill files and the hook. A skill file with different contents is kept.

`BOX_REMOTE`, `BOX_PROJECT` and `BOX_IMAGE` override `pc`, `agents` and `agent-base`.

For multiple hosts, create `agent-box/hosts.json` in your user config directory (`%APPDATA%` on Windows, `$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application Support` on macOS). Incus remotes must already be configured in Incus.

```json
{
  "hosts": [
    {"name": "primary", "remote": "pc", "priority": 10},
    {"name": "overflow", "remote": "spare", "project": "agents", "dashboard_url": "https://spare.example.com"},
    {"name": "cloud", "kind": "modal", "endpoint": "https://runner.example.com", "token_file": "modal-token.json", "mode": "function"}
  ]
}
```

`project` defaults to `BOX_PROJECT`. A host owns its scheduling `priority` (default `0`, higher wins) in `/etc/agent-box/boxd.json` and reports it via `/api/state`, so every client agrees on ranking; `boxd`-capable (`agents` project) hosts are always probed together in the initial tier so no client hint can hide a host-reported priority. For hosts without `boxd` (custom-project hosts, cloud hosts, or as a fallback when `boxd` is unavailable or omits `priority`), `hosts.json` `priority` sets the fallback priority and orders lower non-`boxd` probe tiers (negative priority alongside `boxd` hosts, or descending priority when no `boxd`-capable host is configured), stopping once a host can start so overflow still starts on a lower-priority host before queueing on a higher-priority one. Cloud hosts set `"kind": "modal"`, `endpoint` (`https://`, or `http://` on loopback for tests), `token_file` (JSON `{"headers": {...}}`, resolved relative to the `agent-box` config dir), and optional `mode` (`"function"` default, or `"sandbox"`), with `remote` and `project` omitted and `priority` defaulting to `-1` so owned hardware runs before metered cloud overflow. Without `dashboard_url`, `box` looks for the dashboard at the remote's hostname. `box hosts`, `box status`, and `box_status` show each host's reachability, mode, room, GPU, and queued jobs (and `priority` when any host has a non-zero effective priority). A host's queue shows as `unknown` if its dashboard is unavailable.

With a host registry, `box run` and `box_run` rank hosts by `priority` (higher wins), preferring a host that can start without waiting on another checkout within a tier; repo affinity and registry order break ties. If no host can start, runs that allow queuing wait on an eligible host (ranking by `priority` first). Results name the host and include a qualified job ID for follow-up commands. `box run --host <name>` pins a host for debugging. `--box` uses the default host unless pinned. `BOX_REMOTE` and `BOX_PROJECT` set the default host. An absent or empty registry keeps single-host behavior and bare job IDs.

`box jobs` and `box_job` without an ID list jobs on the default and configured hosts, including waiting jobs and recent runs reported by configured queues. Fleet listings qualify IDs for configured hosts and honor existing bare records on the default host. `box jobs --box` looks up a box on the default host.

When capacity blocks a run, the CLI and MCP report the limiting resource and whether it queued or was refused. A suggested smaller size is an estimate for the selected host. A queued run keeps the requested size and GPU requirement.

## Website previews

Run the preview command from a trusted checkout of `main`, with `gh` and Wrangler signed in. Pass the PR number while staying on `main`. The command prints a public preview link and a link pinned to this deployment.

```sh
npm ci --prefix site
npm --prefix site run preview -- 202
```

The preview serves committed static files from the selected PR through the `fano-site-preview` Worker. Run the same command after a new push to refresh its link.
