---
name: box
description: Offload heavy test runs and builds from this laptop to a disposable Linux box on the gaming PC with the box MCP tools (box_status, box_run, box_job, box_pull). Use before Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

**Offload** Go `-race`, full vitest/jest/pytest suites, Gradle/Android tests, anything over about 2 minutes, and several heavy runs at once. Run quick, focused tests locally.

1. `box_status` with your worktree as `dir`: its first line says offload or run locally.
2. `box_run` with `dir`, `command` and `reason` (why it needs a box; the PC's owner reads it). It syncs uncommitted and untracked files, not ignored ones. Set `timeout` for runs that could hang (exit 124).
3. A run that ends within 30 s of starting returns its result. Otherwise it returns `running`:
   - `push: true`: **don't wait or poll.** Do other work or end your turn; the result arrives as a turn starting "box: job …".   - `push: false`: call `box_job` with the ID; it waits up to 10 min.
   - **Subagents pass `push: false`** (a push would wake your parent) and call `box_job`.
4. **`state: refused` (exit 75) means the PC is busy, paused or out of reach: run locally.** So does a pushed "lost". A "paused" turn means the job may resume; if you run locally instead, `box_cancel` it. A `done` result with exit 75 is the command's own.
5. `box_job` with `offset`/`limit` pages the full log; without `id` it lists jobs.
6. `box_pull` fetches reports or build output. Pull before this repo's next run replaces the checkout. For parallel runs on one repo, set `new: true`.

Boxes are throwaway; only `/cache` survives.

**Never put secrets in `env`**; names like `*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*` are refused.

**Sizes** (`size`): small for quick tests, medium (default), large for big Gradle builds. **GPU**: `gpu: true` gets the RTX 3070, one job at a time; a game stops it. No room or GPU taken: refused.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS, uv, JDK 17, gh, git, build-essential.
