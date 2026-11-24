# agent-box

Turns a Windows gaming PC on the tailnet into on-demand, disposable Linux boxes for coding agents, without touching Windows.

- **Incus** in WSL2 does the boxes, API, auth and web UI.
- **boxtray** sits in the Windows tray: one click picks *My PC*, *Shared* or *Full power*.
- **box** is a thin sync-and-run CLI + MCP for agents: `box run --new -- go test ./...`.

See [docs/DESIGN.md](docs/DESIGN.md).

## Using `box`

Build it with `go build ./cmd/box`. It needs the Windows `incus` client with a remote named `pc` that can reach the `agents` project.

```sh
box run -- go test ./...              # sync this worktree to the repo's box and run there
box run --new -- 'npm ci && npx vitest run'   # throwaway box, deleted afterwards
box run --box big -- make test        # a box you name
box job agent-box.20261123-120000-1a2b  # reattach to a job (the ID is printed at the start)
box new scratch --ttl 2h              # launch a box (sets user.expires)
box ls                                # boxes, status, expiry
box rm scratch
```

`box run` snapshots the worktree, including uncommitted and untracked files, without touching your index. It skips files that are both untracked and ignored. It checks the snapshot out in `/work/<repo>` and runs the command there, logging to `/work/.box/<job>.log` for reattachment. One argument runs as a shell string. Several are quoted word by word. The command returns its exit code, or **75** when the PC is in My PC mode, the box is paused or the PC can't be reached. Run the command locally in those cases. Runs on the same repo and box take turns. The job ID is printed before the job starts. Shallow clones produce snapshots without history. If the repo has checked-out submodules, box warns that their directories will be empty in the box.

Boxes are disposable compute. Idle boxes expire after two hours by default. Only package caches in `/cache` survive deletion. `box new --ttl` sets the initial lifetime. Starting a job refreshes the box's expiry to 24 hours. Reading the job's exit code with `box run` or `box job` resets it to two hours.

`--new` launches a fresh box marked `user.box.throwaway=true` and removes it when the exit code is collected. Use `--box` with a reusable box. If the initial box launch loses its connection, look for the printed box name in `box ls` and remove it with `box rm` if it was created.

`box mcp` is a stdio MCP server with `box_new`, `box_run`, `box_job`, `box_rm` and `box_ls`. `box_run` and `box_job` put the job ID, box, exit code and any warnings ahead of the output tail and in `structuredContent`. Info-level notifications announce the throwaway box before launch and the job ID before the job starts. Cancelling a call stops its setup, or stops following a started job without killing it. Add it to Claude Code with `claude mcp add box -- box mcp`.

`BOX_REMOTE`, `BOX_PROJECT` and `BOX_IMAGE` override `pc`, `agents` and `agent-base`.
