---
name: box
description: Offload heavy test runs and builds from this laptop to a disposable Linux box on the gaming PC with the box MCP tools (box_status, box_run, box_job, box_pull). Use before Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

**Offload** Go `-race`, full vitest/jest/pytest suites, Gradle/Android tests, anything over about 2 minutes, and several heavy runs at once. Run quick, focused tests locally.

1. `box_status` with your worktree as `dir`: its first line says offload or run locally.
2. `box_run` with `dir`, `command` and `reason` (a sentence on why it needs a box; the PC's owner reads it). It syncs uncommitted and untracked files, not ignored ones. Set `timeout` for runs that could hang (exit 124).
3. A run that ends within 30 s returns its result. Otherwise it returns `running`:
   - `push: true`: **don't wait or poll.** Do other work or end your turn; the result arrives as a turn starting "box: job …". If you need it before continuing, end your turn.
   - `push: false`, or you are a subagent: call `box_job` with the ID; it waits up to 10 min.
4. **Exit 75 means the PC is busy, paused or out of reach: run locally.** A "paused" turn means the job may resume; if you run locally instead, `box_cancel` it.
5. `box_job` with `offset`/`limit` pages the full log; without `id` it lists jobs.
6. `box_pull` fetches reports or build output. Pull before this repo's next run, which replaces the checkout. For parallel runs on one repo, set `new: true`.

Boxes are throwaway, never storage; only `/cache` survives.

**Never put secrets in `env`**; names like `*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*` are refused.

**Sizes** (`size`): small for quick tests, medium (default), large for big Gradle builds. **GPU**: `gpu: true` gets the RTX 3070, one job at a time; a game stops it. No room or GPU taken is exit 75.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS, uv, JDK 17, gh, git, build-essential.
