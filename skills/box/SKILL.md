---
name: box
description: Offload heavy test runs and builds from this laptop to a disposable Linux box on the gaming PC with the box MCP tools (box_status, box_run, box_job, box_pull). Use before Go -race, full vitest/jest/pytest suites, Android/Gradle unit tests, anything over about 2 minutes, or several heavy runs at once.
---

# box: offload heavy runs

The laptop starves under heavy test runs, so the gaming PC runs throwaway Linux boxes for them.

**Offload** Go `-race`, full vitest/jest/pytest suites, Android/Gradle unit tests, anything that takes over about 2 minutes, and several heavy runs at once. Run quick, focused tests locally.

1. Call `box_status` with your worktree as `dir`. Its first line says whether to offload now, or to run locally.
2. Call `box_run` with `dir` (your worktree's absolute path) and `command`, like `go test -race ./...`. It syncs uncommitted and untracked files, not ignored ones. Set `timeout` for runs that could hang (exit 124 when it fires).
3. **Exit 75 means the PC is busy, paused or out of reach: run locally (paused jobs resume: `box_job`).**
4. A long run returns `detached`. Call `box_job` with the job ID to keep waiting, or with `offset`/`limit` to page through the log. `box_jobs` lists jobs; `box_cancel` stops one.
5. Fetch reports or build output with `box_pull` right after the job: the next job on the repo replaces the checkout.

Boxes are throwaway compute, never storage: idle boxes are deleted after 2 hours, and only `/cache` survives.

Boxes are shared. **Never put secrets in `env`**; box refuses names like `*TOKEN*`, `*SECRET*`, `*KEY*`, `*PASSWORD*`.

**Sizes** (`size` on `box_run`/`box_new`): small 1-2 CPUs/2 GiB for quick unit tests, medium 2-4/4 GiB (default), large 4-8/8 GiB for big Gradle/Android builds and memory-hungry suites. `box_status` says what fits now. Exit 75 also means no room: try smaller or run locally.

**GPU**: `gpu: true` on `box_run` gets the RTX 3070 for CUDA/PyTorch training or inference and GPU builds. One at a time, and Tyler starting a game stops it. Both exit 75: rerun later or on CPU.

Toolchain: Ubuntu 24.04, Go 1.27.3, Node LTS (npm), uv, JDK 17, gh, git, build-essential.
