---
name: box
description: Offload heavy test runs and builds from this laptop to a disposable Linux box on the gaming PC with the box MCP tools (box_status, box_run, box_job, box_pull). Use before Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

**Offload** Go `-race`, full suites, Gradle/Android tests, runs over ~2 min, or several heavy runs at once. Run quick tests locally.

1. `box_status` with your worktree as `dir`: its first line says whether to offload.
2. `box_run` with `dir`, `command` and `reason` (the PC's owner reads it). It syncs uncommitted and untracked files, not ignored ones. Set `timeout` if it might hang (exit 124). With `pipefail`, `| head` can exit 141.
3. A run ending within 30 s returns its result; else `running`:
   - `push: true`: **don't wait or poll.** Do other work or end your turn; the result arrives starting "box: job …" (Claude Code: mid-turn).
   - `push: false`: call `box_job` with the ID; it waits ≤10 min.
   - **Subagents pass `push: false`** (it would wake the parent).
4. **`state: queued`**: no room, My PC, or the GPU taken; a snapshot waits in line and the result is pushed. Can't wait? Run locally and `box_cancel` it.
5. **`state: refused` (exit 75): not queued (PC busy, paused or unreachable): run locally.** So is a pushed "lost". A "paused" job may resume; run locally instead? `box_cancel` it. `done` with exit 75 is the command's own.
6. `go test`/vitest results lead with failures (file:line); `box_job` pages logs (`offset`/`limit`) or lists jobs (no `id`).
7. `box_pull` fetches reports or build output before the repo's next run replaces it. Parallel runs: `new: true`.

Boxes are throwaway; `/cache` survives.

**Never put secrets in `env`** (`*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*` are refused).

**Sizes** (`size`): small (quick), medium (default), large (Gradle). **GPU**: `gpu: true` gets the RTX 3070, one at a time; My PC stops it.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS, uv, JDK 17, Android SDK 34/35 (lazy, ~1 GB first run), gh, git, build-essential, Chromium libraries (Puppeteer: `--no-sandbox`).
