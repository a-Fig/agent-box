---
name: box
description: Run heavy test suites and builds from your worktree on a disposable remote Linux box with the box MCP tools (box_status, box_run, box_job, box_pull, box_cancel). Use for Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

**Offload** Go `-race`, full suites, Gradle/Android tests, runs over ~2 min, or several heavy runs at once; quick tests gain little.

1. `box_status` with your worktree as `dir`: its first line says whether it fits now.
2. `box_run` with `dir`, `command` and `reason` (the PC's owner reads it). It syncs uncommitted and untracked files, not ignored ones. Set `timeout` if it might hang (exit 124). With `pipefail`, `| head` can exit 141.
3. A run ending within 30 s returns its result; else `running`:
   - `push: true`: **don't wait or poll.** Do other work or end your turn; the result arrives starting "box: job …" (Claude Code: mid-turn): a pass in one line, a failure as its failed tests or last 10 lines. It is enough to act on; `box_job` gives the full result if you need more.
   - `push: false`: call `box_job` with the ID; it waits ≤10 min.
   - **Subagents pass `push: false`** (it would wake the parent).
4. **`state: queued`**: no room, My PC (owner paused boxes), or the GPU taken; a snapshot waits, its result is pushed; `box_cancel` drops it.
5. **`state: refused` (exit 75)**: nothing started (PC busy, paused or unreachable). Retry later, or smaller `size` if no room. A pushed "lost" job didn't finish; a "paused" one may resume (its end is pushed). `done` with exit 75 is the command's own.
6. `go test`/vitest results lead with failures (file:line); `box_job` pages logs (`offset`/`limit`) or lists jobs (no `id`).
7. `box_pull` fetches reports and builds before a later or queued run replaces them. Parallel runs: `new: true`.

Boxes are throwaway; `/cache` survives.

**Never put secrets in `env`** (`*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*` are refused).

**Sizes** (`size`): small (quick), medium (default), large (Gradle). **GPU**: `gpu: true` gets the GPU, one at a time; My PC stops it.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS, uv, JDK 17, Android SDK 34/35 (lazy, ~1 GB first run), gh, git, build-essential, Chromium libraries (Puppeteer: `--no-sandbox`).
