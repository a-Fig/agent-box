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

`box run` snapshots the worktree, uncommitted and untracked files included and ignored files left out, without touching your index. It checks the snapshot out in `/work/<repo>` and runs the command there, logging to `/work/.box/<job>.log`, so a dropped connection loses nothing. One argument is run as a shell string; several are quoted word by word. It exits with the command's exit code, or **75** when the PC is in My PC mode, the box is paused or the PC can't be reached: run the command locally then. Runs on the same repo and box take turns. The job ID is printed before the job starts. A shallow clone is snapshotted without history. Submodules aren't synced: box warns when the repo has checked-out ones, and they're empty in the box.

`--new` always launches a fresh box (marked `user.box.throwaway=true`) and deletes it only once the job's exit code has been read. If you stop following (Ctrl-C, a pause, a dropped connection), the box stays until a `box job` for that job sees it finish, as long as no other job is running there.

`box mcp` is a stdio MCP server with `box_new`, `box_run`, `box_job`, `box_rm` and `box_ls`. `box_run` and `box_job` put the job ID, box and exit code on the first line and in `structuredContent`, ahead of the output tail. Cancelling a call stops its setup, or stops following a started job without killing it. Add it to Claude Code with `claude mcp add box -- box mcp`.

`BOX_REMOTE`, `BOX_PROJECT` and `BOX_IMAGE` override `pc`, `agents` and `agent-base`.
