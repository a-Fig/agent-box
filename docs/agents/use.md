# Using agent-box (for coding agents)

Once this machine has joined an agent-box host (`box join`), coding agents have five MCP tools on
`box mcp` (`box_status`, `box_run`, `box_job`, `box_cancel`, `box_pull`), and the `box` CLI works
in any shell. If you don't have the tools yet, ask your user and follow
https://agent-box.afig.dev/join.

Use agent-box to offload heavy work: `go test -race`, full vitest/jest/pytest suites,
Gradle/Android unit tests, builds over about 2 minutes, or several heavy runs in parallel. Quick
checks gain little from offloading.

## Tools

- `box_status`: check first whether offloading makes sense right now. Returns advice (`available` or
  `unavailable`), host mode (`Full`, `Shared`, or `My PC`), free slots and room per size, GPU
  availability, and your repo's box and running jobs.
- `box_run`: snapshot your git worktree (uncommitted and untracked files included, ignored files
  skipped) without touching your index, sync to a box, and run `command` in `/work/<repo>`.
  Requires `reason` (10 to 300 characters) explaining why the run needs a box; the host's owner sees
  it. Returns inline if the job finishes within 30 seconds; otherwise returns `state: running` with
  a job ID (`push: true` pushes the result into your T3 Code thread when done; pass `push: false`
  in subagents and call `box_job` instead). With `push`, runs that can't start now queue cleanly
  (`state: queued`). Use `new: true` for parallel runs on the same repo.
- `box_job`: wait up to 10 minutes for a job's full output (`id`), page logs without waiting
  (`offset` / `limit`), or list recent jobs (omit `id`).
- `box_cancel`: kill a running job's process session (exit 130) or remove a queued job and its
  snapshot.
- `box_pull`: copy test reports, coverage, or build artifacts from a job or box into a local
  directory (`<localDir>/<name>`). Never overwrites existing files and drops symlinks. Pull before
  starting another run on the same repo, which replaces `/work/<repo>`.

## Sizes, modes, and rules

- **Sizes (`size`):** `small` (1/2 CPUs, 2 GiB, 18 GiB), `medium` (default: 2/4 CPUs, 4 GiB, 30 GiB),
  `large` (4/8 CPUs, 8 GiB, 40 GiB), and `gpu` (`gpu: true`, 4/6 CPUs, 8 GiB, 40 GiB + the host's NVIDIA GPU on
  WSL2 hosts, one at a time).
- **Modes (shared Windows PCs):** `Full` (full CPUs, 24 GiB cap), `Shared` (half CPUs, 12 GiB cap), `My PC` (boxes paused
  for up to 15 min, or 4 hours once reclaimed to swap; GPU boxes are stopped). Dedicated Linux hosts size CPU/memory caps from the host machine instead.
- **Exit 75 (`state: refused`):** nothing started (host paused, busy without queue, or unreachable).
  Retry later or pick a smaller `size`. A `done` job with exit 75 is the command's own exit code.
- **Disposable compute:** idle boxes expire after 2 hours; `--new` throwaways stay 30 minutes
  after exit for `box_pull` (unless another run needs room). Only `/cache` survives.
- **Never put secrets in `env`:** boxes are shared and `box` refuses names matching `TOKEN`,
  `SECRET`, `KEY`, `PASSWORD`, `PASSWD`, or `CREDENTIAL`.
