---
name: box
description: Run heavy test suites and builds from your worktree on a disposable remote Linux box with the box MCP tools (box_status, box_run, box_job, box_pull, box_cancel). Use for Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

**Offload** Go `-race`, full suites, Gradle/Android tests, runs over ~2 min, and parallel builds.

1. `box_status` with `dir` reports room per host.
2. `box_run` takes `dir`, `command`, and `reason` (owner-visible). Syncs uncommitted/untracked files, excluding ignored. Fleet results include host and qualified job ID. `timeout` limits runtime (124). With `pipefail`, `| head` can exit 141.
3. A run ending within 30 s returns its result; else `running`:
   - `push: true`: **don't wait or poll.** Expect "box: job …" (Claude Code: mid-turn). Notices retry on MCP restart in the same T3 profile. Details in `box_job`.
   - `push: false`: call `box_job` with ID; it waits ≤10 min.
   - **Subagents pass `push: false`** (it would wake the parent).
4. **`state: queued`**: no room, My PC (owner paused boxes), or the GPU taken; snapshot waits for the host; `box_cancel` drops it.
5. **`state: refused` (exit 75)**: nothing started. Capacity results name the limit and may suggest smaller sizes. Paused jobs may resume. Lost jobs never finished. `done` with exit 75 is the command's own.
6. `go test`/vitest show failures first (file:line). `box_job` pages logs (`offset`/`limit`) or lists fleet jobs (no `id`).
7. `box_pull` fetches artifacts (refuses if a later run replaced the checkout; cloud requires `sandbox` mode). Parallel: `new: true`.

CPU throwaways can be reused. `fresh: true` launches clean.
Results/notices show HEAD, dirty state, snapshot commit, and cloud cost lines.
EOF on Incus image lookup gets one retry.

**Never put secrets in `env`** (`*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*` are refused).

**Sizes** (`size`): small, medium (default), large (Gradle). **GPU**: `gpu: true` (one at a time; My PC stops it).

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS, uv, JDK 17, Android SDK 34/35 (lazy, ~1 GB first run), gh, git, build-essential, Chromium libraries (Puppeteer: `--no-sandbox`).
