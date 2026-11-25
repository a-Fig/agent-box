# agent-box

Turns a Windows gaming PC on the tailnet into on-demand, disposable Linux boxes for coding agents, without touching Windows.

- **Incus** in WSL2 does the boxes, API, auth and web UI.
- **boxtray** sits in the Windows tray: one click picks *My PC*, *Shared* or *Full power*.
- **box** is a thin sync-and-run CLI + MCP for agents: `box run --new --reason "..." -- go test ./...`.

See [docs/DESIGN.md](docs/DESIGN.md).

## Using `box`

Build it with `go build ./cmd/box`. It needs the Windows `incus` client with a remote named `pc` that can reach the `agents` project.

```sh
box status                            # worth offloading now? PC, mode, room per size, this repo's box
box run --reason "Full test suite for the parser change." -- go test ./...
                                      # sync this worktree to the repo's box and run there
                                      # (--reason is required; left out below)
box run --new -- 'npm ci && npx vitest run'   # throwaway box, deleted afterwards
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
box app                               # the dashboard in a window of its own (Windows)
box --version                         # the commit this box was built from
```

`box run --reason` (required, 10 to 300 characters) says why the run needs a box; `box new --reason` stores one as `user.box.reason`. `box run` records it in `/work/.box/<job>.meta` with the command, repo, branch, commit, size, `--env` names (never values) and who asked, and reports to the dashboard, best effort, a run turned away with exit 75 and how each run ended. Both are self-reported, for display ([docs/ui.md](docs/ui.md#agent-identity)).

`box run` snapshots the worktree, including uncommitted and untracked files, without touching your index. It skips files that are both untracked and ignored. It checks the snapshot out in `/work/<repo>` and runs the command there, logging to `/work/.box/<job>.log` for reattachment. One argument runs as a bash string with `pipefail`, so `go test ./... | tail` exits with `go test`'s code (and `x | head` can exit 141). Several are quoted word by word. When the command runs `go test` or vitest (`vitest`, `npx`/`pnpm`/`yarn vitest`, or `npm test` whose script is plain `vitest`), box adds `-json` or vitest's JSON reporter and keeps the report beside the log, which stays close to the tool's own output ([details](docs/DESIGN.md#failure-digest)); the result then leads with a digest of at most 3KiB: the failed tests, each with file:line and its first assertion or panic line, then the counts. The command returns its exit code, or **75** when the PC is in My PC mode, the box is paused or the PC can't be reached. Run the command locally in those cases. If the box is frozen while a job runs, `box run` and `box job` stop following at once and exit 75 with `job X is paused`. The job resumes if the PC's owner switches back within 15 minutes, or 4 hours once the tray has moved the box's memory to swap; after that the tray stops the box and the job is lost. `box job X` reattaches later. Runs on the same repo and box take turns. The job ID is printed before the job starts. Shallow clones produce snapshots without history. If the repo has checked-out submodules, box warns that their directories will be empty in the box.

Boxes are disposable compute. Idle boxes expire after two hours by default. Only package caches in `/cache` survive deletion. `box new --ttl` sets the initial lifetime. Starting a job refreshes the box's expiry to 24 hours. Reading the job's exit code with `box run` or `box job` resets it to two hours.

Sizes are small (1 CPU in Shared, 2 in Full, 2GiB RAM, 18GiB disk), medium (2/4, 4GiB, 30GiB, the default) and large (4/8, 8GiB, 40GiB). A box records its size in `user.box.size` and is never resized. `--size` picks the size of a box the run launches. If the repo's box exists in another size, the run uses a throwaway box of the requested size and says so. A launch Incus refuses for the project's limits first deletes throwaways kept after their job (30 minutes, for `box_pull`), the first to expire first, and tries again; if it still doesn't fit, it exits 75. `box status` counts kept throwaways as free room. [Box sizes](docs/DESIGN.md#box-sizes)

`--gpu` (`--size gpu`: 4/6 CPUs, 8GiB, 40GiB) gives the box the RTX 3070 through WSL's `/dev/dxg`, for CUDA or PyTorch training and inference and GPU builds. GPU runs always use a throwaway box, and only one GPU box runs at a time: another GPU run exits 75 with `the GPU box is in use (job X)`. Because a frozen CUDA process keeps its VRAM, the tray stops a GPU box instead of pausing it when the PC's owner starts a game or picks My PC; its job is lost and `box job` exits 75 saying so. `box status` has a `gpu:` line: free, in use, or unavailable. [The GPU box](docs/DESIGN.md#the-gpu-box)

`--new` launches a fresh box marked `user.box.throwaway=true` and removes it when the exit code is collected. Use `--box` with a reusable box. If the initial box launch loses its connection, look for the printed box name in `box ls` and remove it with `box rm` if it was created.

`box status` prints one advice line first, like `available: room for 1 large, or 2 medium, or 4 small` or `unavailable: PC is in My PC mode: run locally`, then the mode (and the game it yields to, from `user.agent-box.reason`), free box slots (`limits.instances` minus existing boxes), what fits (the project caps minus existing boxes' Full CPUs, memory and disk; the memory cap is 24GiB in Full and 12GiB otherwise), CPUs and RAM per size now, and this repo's box with its size and running jobs. It exits 75 when offloading isn't possible now.

`--env K=V` (repeatable) passes variables to the command. Boxes are shared and the values are visible in the box, so names containing `TOKEN`, `SECRET`, `KEY`, `PASSWORD`, `PASSWD` or `CREDENTIAL` are refused. `--timeout` returns exit `124` and force-kills a command that ignores TERM for 10 seconds. `box cancel` stops the job's session and writes exit code `130` if the wrapper hasn't already recorded an exit. `box pull <job|box> <remote-path> <local-dir>` copies a file or directory into `<local-dir>/<name>`. A relative remote path starts at the job's checkout. It never replaces an existing file and drops symlinks, so nothing lands outside `<local-dir>`. Pull right after the job, since the next job on the same repo replaces the checkout.

`box mcp` is a stdio MCP server with five tools, `box_status`, `box_run`, `box_job`, `box_cancel` and `box_pull`, which behave like the commands above. Creating, listing and deleting boxes (`new`, `ls`, `rm`) and `--box` are CLI only. Results start with a line that carries the key facts, and most also return `structuredContent`. `box_run` and `box_job` put the job ID, box, exit code and any warnings ahead of the output tail, and `structuredContent` repeats the tail (`tail`, or `log` for a page), since Claude Code shows it in place of the text. `box_job` with `offset` or `limit` returns a page of the log and `nextOffset` without waiting, and without `id` it lists jobs. If a page contains incomplete UTF-8, `logBase64` in `structuredContent` preserves the original bytes. Info-level notifications announce the throwaway box before launch and the job ID before the job starts. Cancelling a call stops its setup, or stops following a started job without killing it.

`box_run` returns the result inline if the job ends within 30 seconds of the command starting. Otherwise it returns `state: running` (`exitCode: null`) and `push`. When `push` is true, box has a T3 thread for this session (`T3CODE_THREAD_ID`, or Codex's `_meta.threadId`) and a detached `box watch` process on the laptop follows the job and sends the result into that thread with `t3 thread send`, as a turn starting `box: job ...`, so the agent can end its turn instead of waiting. The message has the reason, the command, the digest of a test run and the last 20 lines, masked like the dashboard's history. A throwaway box and its log are kept 30 minutes after the job, so `box_pull` still works. Once box has read a job's exit it keeps the exit code and the last 32KiB of output on the laptop, so `box_job` still gives them after the box is gone (a launch short of room can take a kept throwaway early). When `push` is false (no T3 thread, or `push: false`, which subagents pass) the agent calls `box_job`, which waits up to 10 minutes and always returns the full result. Every later `box_*` result starts with `finished since you last looked: ...` until a job's result has reached the agent, and restarts watchers that a laptop sleep or reboot killed. With `push`, a run that can't start now (no room, the GPU taken, My PC or a game) is handed to boxd's queue instead of refused. `box_run` sends the worktree's snapshot, the same git bundle a run pushes, and returns `state: queued` with the job ID, its `position` in line and `why` it waits. The watcher follows it through the queue, pushes one nudge if it has waited 20 minutes, and pushes the result when it has run, or says it left the queue without running (boxd drops a job after 24 hours). `box_cancel` takes a queued job off the queue, and `box_job` says where it is in line. `queue: false` turns the queue off for that run. A GPU run during a game or My PC, a run without push, and a run the queue can't take still come back at once as `state: refused` (exit 75), which is never pushed, nor is a job the agent cancelled. [The queue](docs/queue.md) A duplicate notice is fine, a lost one is not: if `box_job` and the watcher race, the agent may get the result twice. `box_run` takes no `box` or `wait`. A session holding an older box's tool list may still send them, or leave out `reason`: the run goes ahead with a notice, the reason recorded as `(no reason given) <command>`, and the server sends `notifications/tools/list_changed` at startup so clients reload the tools. [Pushing results to the agent](docs/DESIGN.md#pushing-results-to-the-agent)

`box join` registers `box mcp` as a user-scope MCP server with each agent CLI it finds. It installs the [box skill](skills/box/SKILL.md) in `~/.claude/skills/box/` and also in `$CLAUDE_CONFIG_DIR/skills/box/` when set. For Codex it uses `~/.codex/skills/box/` or `$CODEX_HOME/skills/box/` when set. Run `box join` from a built binary at its final path. Where Claude Code is installed, it also adds `box hook` to the `hooks.PostToolUse` of `settings.json` in those config directories: T3 holds a pushed result until the agent's turn ends, so the hook shows a busy agent its ended jobs after its next tool call instead, and the watcher then sends nothing. `box join --leave` unregisters the servers and removes the installed skill files and the hook. A skill file with different contents is kept.

`BOX_REMOTE`, `BOX_PROJECT` and `BOX_IMAGE` override `pc`, `agents` and `agent-base`.
