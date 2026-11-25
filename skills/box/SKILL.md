---
name: box
description: Offload heavy test runs and builds from this laptop to a disposable Linux box on the gaming PC with the box MCP tools (box_status, box_run, box_job, box_pull). Use before Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

**Offload** Go `-race`, full test suites, Gradle/Android tests, anything over about 2 minutes, and several heavy runs at once. Run quick, focused tests locally.

1. `box_status` with your worktree as `dir`: its first line says offload or run locally.
2. `box_run` with `dir`, `command` and `reason` (why it needs a box; the PC's owner reads it). It syncs uncommitted and untracked files. Set `timeout` if it could hang (exit 124). Pipes run with `pipefail`.
3. A run ending within 30 s returns its result; else `running`:
   - `push: true`: **don't wait or poll.** Do other work or end your turn; the result arrives as a turn starting "box: job …".
   - `push: false`: call `box_job` with the ID; it waits up to 10 min.
   - **Subagents pass `push: false`** (a push would wake the parent).
4. **`state: queued`**: the PC is full or paused, so a snapshot waits in line; the result is pushed. Can't wait? Run locally and `box_cancel` it.
5. **`state: refused` (exit 75): the PC is busy, paused or unreachable: run locally.** So is a pushed "lost". A "paused" job may resume; if you run locally instead, `box_cancel` it. `done` with exit 75 is the command's own.
6. `box_job` with `offset`/`limit` pages the log; without `id` it lists jobs.
7. `box_pull` fetches reports or build output, before this repo's next run replaces the checkout. Parallel runs on one repo: `new: true`.

Boxes are throwaway; `/cache` survives.

**Never put secrets in `env`**; names like `*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*` are refused.

**Sizes** (`size`): small for quick tests, medium (default), large for big Gradle builds. **GPU**: `gpu: true` gets the RTX 3070, one job at a time; a game stops it.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS, uv, JDK 17, gh, git, build-essential.
