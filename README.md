# agent-box

Turns a Windows gaming PC on the tailnet into on-demand, disposable Linux boxes for coding agents, without touching Windows.

- **Incus** in WSL2 does the boxes, API, auth and web UI.
- **boxtray** sits in the Windows tray: one click picks *My PC*, *Shared* or *Full power*.
- **box** is a thin sync-and-run CLI + MCP for agents: `box run --new -- go test ./...`.

See [docs/DESIGN.md](docs/DESIGN.md).

## Using `box`

Build it with `go build ./cmd/box`. It needs the Windows `incus` client with a remote named `pc` that can reach the `agents` project.

```sh
box status                            # worth offloading now? PC, mode, free slots, this repo's box
box run -- go test ./...              # sync this worktree to the repo's box and run there
box run --new -- 'npm ci && npx vitest run'   # throwaway box, deleted afterwards
box run --box big -- make test        # a box you name
box run --env CI=1 --timeout 30m -- npm test  # env for the command; killed with exit 124 after 30m
box job agent-box.20261123-120000-1a2b  # reattach to a job (the ID is printed at the start)
box job <id> --offset 65536 --limit 32768  # read a piece of the log without waiting
box jobs [--box agent-box]            # jobs, newest first: running, done (exit code), died
box cancel <id>                       # stop a job and everything it started (exit 130)
box pull <id> coverage ./out          # copy /work/<repo>/coverage into ./out/coverage
box new scratch --ttl 2h              # launch a box (sets user.expires)
box ls                                # boxes, status, expiry
box rm scratch
```

`box run` snapshots the worktree, including uncommitted and untracked files, without touching your index. It skips files that are both untracked and ignored. It checks the snapshot out in `/work/<repo>` and runs the command there, logging to `/work/.box/<job>.log` for reattachment. One argument runs as a shell string. Several are quoted word by word. The command returns its exit code, or **75** when the PC is in My PC mode, the box is paused or the PC can't be reached. Run the command locally in those cases. Runs on the same repo and box take turns. The job ID is printed before the job starts. Shallow clones produce snapshots without history. If the repo has checked-out submodules, box warns that their directories will be empty in the box.

Boxes are disposable compute. Idle boxes expire after two hours by default. Only package caches in `/cache` survive deletion. `box new --ttl` sets the initial lifetime. Starting a job refreshes the box's expiry to 24 hours. Reading the job's exit code with `box run` or `box job` resets it to two hours.

`--new` launches a fresh box marked `user.box.throwaway=true` and removes it when the exit code is collected. Use `--box` with a reusable box. If the initial box launch loses its connection, look for the printed box name in `box ls` and remove it with `box rm` if it was created.

`box status` prints one advice line first, like `available: 2 slot(s) free, 2 CPUs/4GiB per box` or `unavailable: PC is in My PC mode: run locally`, then the mode (and the game it yields to, from `user.agent-box.reason`), free box slots (`limits.instances` minus existing boxes), CPUs and RAM per box, and this repo's box with its running jobs. It exits 75 when offloading isn't possible now.

`--env K=V` (repeatable) passes variables to the command. Boxes are shared and the values are visible in the box, so names containing `TOKEN`, `SECRET`, `KEY`, `PASSWORD`, `PASSWD` or `CREDENTIAL` are refused. `--timeout` returns exit `124` and force-kills a command that ignores TERM for 10 seconds. `box cancel` stops the job's session and writes exit code `130` if the wrapper hasn't already recorded an exit. `box pull <job|box> <remote-path> <local-dir>` copies a file or directory into `<local-dir>/<name>`. A relative remote path starts at the job's checkout. It never replaces an existing file and drops symlinks, so nothing lands outside `<local-dir>`. Pull right after the job, since the next job on the same repo replaces the checkout.

`box mcp` is a stdio MCP server with `box_status`, `box_new`, `box_run`, `box_job`, `box_jobs`, `box_cancel`, `box_pull`, `box_rm` and `box_ls`, which behave like the commands above. Results start with a line that carries the key facts, and most also return `structuredContent`. `box_run` and `box_job` put the job ID, box, exit code and any warnings ahead of the output tail. `box_job` with `offset` or `limit` returns a page of the log and `nextOffset` without waiting. If a page contains incomplete UTF-8, `logBase64` in `structuredContent` preserves the original bytes. Info-level notifications announce the throwaway box before launch and the job ID before the job starts. Cancelling a call stops its setup, or stops following a started job without killing it.

`box join` registers `box mcp` as a user-scope MCP server with each agent CLI it finds. It installs the [box skill](skills/box/SKILL.md) in `~/.claude/skills/box/` and also in `$CLAUDE_CONFIG_DIR/skills/box/` when set. For Codex it uses `~/.codex/skills/box/` or `$CODEX_HOME/skills/box/` when set. Run `box join` from a built binary at its final path. `box join --leave` unregisters the servers and removes the installed skill files. A skill file with different contents is kept.

`BOX_REMOTE`, `BOX_PROJECT` and `BOX_IMAGE` override `pc`, `agents` and `agent-base`.
