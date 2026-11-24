---
name: box
description: Offload heavy test runs and builds from this laptop to a disposable Linux box on the gaming PC with the box MCP tools (box_status, box_run, box_job, box_pull). Use before Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

The laptop has 15 GB RAM and starves under heavy test runs. The gaming PC runs throwaway Linux boxes for them.

**Offload** Go `-race`, full vitest/jest/pytest suites, Android/Gradle unit tests, anything that takes over about 2 minutes, and several heavy runs at once. Run quick, focused tests locally.

1. Call `box_status` with your worktree as `dir`. Its first line says whether to offload now. If it says run locally, do that.
2. Call `box_run` with `dir` (your worktree's absolute path) and `command`, like `go test -race ./...`. It syncs uncommitted and untracked files, not ignored ones. Set `timeout` for runs that could hang (exit 124 when it fires).
3. **Exit 75 means the PC is busy, paused or out of reach: run the command locally.**
4. A long run returns `detached`. Call `box_job` with the job ID to keep waiting, or with `offset`/`limit` to page through the log. Lost the ID? `box_jobs` lists jobs. `box_cancel` stops one.
5. Fetch reports, coverage or build output with `box_pull` right after the job, into a local directory. The next job on the same repo replaces the checkout.

Boxes are throwaway compute, never storage: idle boxes are deleted after 2 hours, and only package caches in `/cache` survive. Keep your work in git on the laptop.

Boxes are shared. **Never put secrets in `env`**; box refuses names like `*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*`.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS (npm), uv, JDK 17, gh, git, build-essential. Each box gets 2 CPUs (Shared) or 4 (Full) and 4 GiB RAM.
