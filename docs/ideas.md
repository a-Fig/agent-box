# What agent-box could steal

Research done 2026-11-24 and 2026-11-25. Read-only: nothing in the repo, on the PC or on GitHub was changed. I read the repo (README, CLAUDE.md, DESIGN.md, and ui.md, queue.md and releasing.md from their branches), Murmur's README, VISION, releasing, testing, updating and agent docs and its scripts, and the docs of the outside products named below. Two helper agents covered CI caches and test tooling, and sandboxes, batch and spare-capacity compute. I read their reports and checked several of their claims myself (Namespace, E2B, Nx, Buildkite, gotestsum, Coder, Incus metrics). A few things are not verified; they are marked.

## 1. Where agent-box stands

agent-box does the hard part already. A laptop agent can say `box_run`, the worktree (dirty files included) is snapshotted, a throwaway Incus container on the gaming PC runs the command, and the result is pushed back into the agent's thread so it never sits blocked. The PC stays Tyler's: the tray freezes boxes in My PC mode, yields to a game, swaps frozen memory out, and a headless enforcer covers an unattended reboot. There is a queue with a fairness rule, size classes, a GPU box, restricted certs, a reaper, a dashboard with history, and a release and self-update system in progress. Murmur's habit of "a duplicate notice is fine, a lost one is not" is already in the push design.

The weak spots are not in the plumbing. They are in what happens between "the job ended" and "the agent did something useful", and in what happens before the job starts.

1. **The result is a tail of a log.** The pushed message carries the exit code and the last 20 lines. For a Go or vitest run, the useful answer is "these three tests failed, here is the assertion and file:line", and an agent has to page the log to get it. That costs tokens and turns, and agents often guess wrong from a tail.
2. **Every failure looks the same.** Exit 75 means "run it locally", exit 124 means timeout, anything else is the command's. Nothing separates "the test failed" from "the PC froze the box for a game", "the box ran out of memory" or "the daemon restarted". Only the first deserves the agent's attention. The rest deserve a quiet retry.
3. **Cold starts are paid every time.** A repo's box keeps `node_modules` and caches warm, but a `--new` box, a new repo, a different size, or a box the reaper took after two idle hours starts from `agent-base` and installs everything again. The `/cache` volume helps, but nothing is prepared per repo.
4. **`/cache` is one shared directory.** A job that is killed, frozen too long, or hits the OOM killer can leave half-written cache entries that every later box reads.
5. **Tyler can see state but not causes.** The dashboard shows machines, boxes, jobs and history, but a box's death, a freeze or a refusal doesn't always carry a reason, and there is no per-job peak memory or CPU, so nobody can tell whether a size was right.
6. **The yield is a single switch.** A game freezes everything. That is the safe default, but it is blunt: a GPU-only app doesn't need the CPU boxes frozen, and a freeze has no warning, so a job hears nothing before it is stopped in its tracks.
7. **Onboarding and trust are manual.** Murmur installs with one line and ships its own agent docs inside the binary; agent-box is "build it with `go build`", a skill file copied into three configs by hand, and no `doctor` that says why `box status` can't reach the PC. The release plan fixes updating, not first install or diagnosis.

Everything below is aimed at those seven. I have ranked by value for the effort, and I have left out ideas that only pay at fleet scale.

## 2. Make results something an agent can act on

This is the highest-value theme and the cheapest to start.

**A failure digest instead of a tail.** gotestsum already turns `go test -json` (test2json) into a JUnit file or a JSON file, with `--post-run-command` receiving `TESTS_FAILED` and friends, and it ships `tool slowest` for the top slow tests ([github.com/gotestyourself/gotestsum](https://github.com/gotestyourself/gotestsum)). Vitest has a JSON reporter and `--silent=passed-only`, and GitHub Actions' annotation format (`file:line: message`, counts first, raw log folded) is a well-worn way to show failures ([docs.github.com, workflow commands](https://docs.github.com/actions/reference/workflow-commands-for-github-actions)). `box` already knows the command's kind (go-test, js-test, pytest, gradle in `internal/dash/kind.go`), so it can pick the right wrapper. The job's end report would then lead with something like "FAIL: 3 of 412 tests. `TestQueueFair` queue_test.go:88 expected 2 got 3 (and two more)", then the counts, then where the full log is. The full log stays behind `box_job` and `box_pull`. The point is saved tokens and fewer wrong guesses. The kind table already exists, so the new work is a small parser per kind plus a wrapper that adds `-json` and the reporter flag without changing the command's own exit code. I would do Go and vitest first, since those are Tyler's most common runs.

**Rerun only what failed, and say so.** `gotestsum --rerun-fails` reruns failed tests until each passes once (default two attempts, skipped above ten failures) and has `--rerun-fails-abort-on-data-race`, because a data race is a real finding and rerunning it just hides it. Murmur does a version of this in CI (`scripts/go-test-retry.sh`: retry only a test that failed by name, never after a race, never when the binary hung, annotate what was retried, count retries over many runs with `retried-tests.sh`). For agent-box the lesson is the reporting. If a test failed and then passed, the result should say "flaky, passed on attempt 2", not just "pass". An agent that sees a silent pass learns nothing; one that sees "flaky" can leave it alone or file it.

**Track flakiness by input hash, and keep a muted list.** Nx Cloud calls a task flaky when the same input hash has both failed and passed ([nx.dev](https://nx.dev/ci/features/flaky-tasks)). That works because the inputs are pinned, so a code change isn't mistaken for a flake; the flag expires after two weeks without incident. `box run` already builds a git tree for every snapshot, so the tree hash is free. Storing (repo, command, tree hash, outcome) in boxd's history gives Tyler a real flaky-test list and gives the agent "this test has failed and passed on the same code before". Buildkite's quarantine adds the right behaviour: a muted test still runs and still reports, it just doesn't fail the build. Skipping a flaky test is worse than muting it, because you stop learning about it. A per-repo muted list shown as "2 muted tests failed" in every result is cheap once the digest exists.

**Replay an identical result.** Bazel and Turborepo hash inputs and replay a cached action's output. Go's own test cache does this per package, and `-count=1` silently turns it off. Two small things follow. `box_run` could warn when an agent adds `-count=1` to a command whose point is just "run the tests", and `GOCACHE` (which includes the test cache) must stay on `/cache`. Beyond that, an exact replay at the box level (same tree hash, same command, same size, last result a clean pass) could answer in a second: "same result as 6 minutes ago, tree unchanged". Agents re-run the same command after a flake or a context reset often enough that this is worth a look, but it is a later step.

## 3. Fail less, and fail for the right reasons

**Classify failures, retry only the infrastructure ones.** Buildkite retries by exit status and, in its own examples, retries `-1`, `143` and `255` (killed, terminated, SSH failure) while leaving exit 1 alone ([buildkite.com docs](https://buildkite.com/docs/pipelines/configure/step-types/command-step)). AWS Batch's `evaluateOnExit` is an ordered list of RETRY/EXIT rules on the exit code or the failure reason, with an attempt counter exposed to the job ([docs.aws.amazon.com](https://docs.aws.amazon.com/batch/latest/userguide/job_retries.html)). Kubernetes goes further: a `podFailurePolicy` can `Ignore` a disruption so it doesn't consume the retry budget at all ([kubernetes.io](https://kubernetes.io/docs/concepts/workloads/controllers/job/)). agent-box has exactly these categories hiding in its own logic: lost to a game freeze that ran too long, stopped by the 15-minute frozen limit, OOM killed, disk full, boxd restarting under a job, the PC unreachable. A job that dies for one of those should be retried once by boxd without bothering the agent, and the final message should say "retried once because: yield", with the attempt count. A test failure, a nonzero exit from the command itself, and a cancel are never retried.

**Time that the PC froze isn't the job's time.** Kubernetes doesn't count a suspended job's time against its deadline. agent-box's `--timeout` is wall clock, so a job frozen for forty minutes by a game can be killed for being slow the moment it thaws. The timer should pause with the freeze. Related, AWS Batch gives `STARTING` its own state apart from `RUNNING`. A box launching or unpacking a snapshot is not running the command, and the timeout shouldn't tick through it. This also gives "why is it queued" a place to live (waiting, starting, running, frozen, finished, died) instead of inferring it.

**Supersede stale work.** Buildkite's concurrency groups with `cancel_in_progress` let a new build cancel the older one. Agents do this to themselves: they start a run, edit, and start another while the first is still using four CPUs. A rule that a new job from the same session for the same repo and command can cancel the old one (opt-in, and told to the agent) frees capacity and avoids stale answers. Murmur-style honesty applies: say it did it.

## 4. Start faster

This is the single biggest lever on time-to-result, and Incus on btrfs is built for it.

**A prepared box per repo.** After a repo's first successful run, publish its box as an image (`incus publish <box> --alias <repo>-warm`) or keep a snapshot of it. On btrfs a launch from an image or a copy of a snapshot is copy-on-write, so a new box with `node_modules`, the Gradle daemon's inputs and compiled objects in place costs seconds, not minutes. The helper's two variants are both sound. A published image is shared and survives deletion of the box, but publishing writes a compressed tarball, so do it rarely (after a dependency file changes, or on a timer). A snapshot kept beside the box, then `incus copy box/snap job-N`, skips the publish and is the faster path for throwaway `--new` runs. Snapshots live in the same pool and are cheap; `snapshots.expiry` handles retention ([linuxcontainers.org](https://linuxcontainers.org/incus/docs/main/howto/instances_backup/)). Namespace's devboxes work this way: a warm per-project instance, leased by tag and reset to a git ref before each job ([namespace.so changelog](https://namespace.so/blog/changelog-020)). Coder's prebuilt workspaces keep an unclaimed pool topped up by a reconcile loop with a TTL ([coder.com docs](https://coder.com/docs/admin/templates/extending-templates/prebuilt-workspaces)). On one PC the pool idea is marginal (a pool of one per size, and zero while yielding), but the prepared-image idea is the point. The snapshot also pairs with the queue: a queued job could start on a clone, not on a box that has to be built first.

Two checks to make before building: how long a CoW clone takes here (WSL's btrfs, not a datacenter's), and whether `box run`'s git bundle is incremental. If the box already has the base commit, the upload could carry only the difference and its prerequisites, which would shrink the sync from the whole history to a few kilobytes. I did not read `snapshot.go` closely enough to say it isn't already.

**Don't snapshot memory.** Fly, Modal and E2B can pause with memory (E2B: about 4 seconds per GiB to pause, about 1 second to resume). Incus containers can't do this dependably (stateful snapshots lean on CRIU, which Incus's docs call not fully supported for containers). Skip it. The one lesson worth keeping is that sockets break across a freeze, so anything a job holds open should tolerate a reset after a thaw, which the game yield already causes today.

## 5. Make `/cache` safe, bounded and per-tool

Namespace's cache volumes give each job a private copy-on-write fork of the last committed cache, and commit its writes as a new generation only if the job exits 0 ([namespace.so docs](https://namespace.so/docs/architecture/storage/cache-volumes)); failed jobs' writes are dropped. Blacksmith's sticky disks are one disk per cache key (a Go module cache, a Gradle home, npm), cloned per job and evicted after seven days idle. Both are the same idea, and btrfs gives it to agent-box nearly free: make `/cache` a btrfs subvolume, snapshot it when a box starts, mount the snapshot, and promote it back only on exit 0 (and only if no other job promoted first, otherwise merge by just keeping the newer). Splitting it into one subvolume per tool (go, npm, gradle, uv, android-sdk) makes the 50 GiB cap and the reset per tool, so a corrupt Gradle cache doesn't cost the Go test cache. Add an age and size cap (Bazel's disk cache has garbage collection for this) so it stays a cache.

Gradle specifics for the Android runs: `GRADLE_USER_HOME` on `/cache`, `org.gradle.caching` and the configuration cache turned on, and a warm per-repo box so the Gradle daemon is reused ([docs.gradle.org, build cache](https://docs.gradle.org/current/userguide/build_cache.html)). Robolectric's jars already go to `/cache/robolectric-m2`, so that part is done.

`GOCACHEPROG` (a JSON get/put helper that Go can delegate its cache to) and sccache are only worth it with more than one host, since one host shares the cache by sharing the volume. Revisit when #8 happens.

## 6. See and trust the system

**Postmortems.** Modal can snapshot a sandbox when it exits and E2B keeps a seven-day history of lifecycle events with a `kill_reason`. agent-box deletes a failed or OOM-killed throwaway box on a timer and tells the agent the tail. A cheaper and more useful policy: when a job dies abnormally (OOM, disk full, lost), snapshot the box before the reaper takes it, keep it a few hours with `snapshots.expiry`, and give the dashboard a "reopen failed box" action that starts a clone. Pair that with a recorded `reason` on every state change (frozen: game `x.exe` since 21:14; stopped: 15 minute limit; deleted: reaper, expired; refused: no room). Tyler's trust in the system comes from being able to answer "why did that happen" without reading logs.

**Peak usage, per job.** Incus serves `/1.0/metrics` per instance (CPU seconds, memory, disk, network, processes), behind a metrics-only certificate that can't manage instances ([linuxcontainers.org metrics](https://linuxcontainers.org/incus/docs/main/metrics/)). Sampling it at job end, or the cgroup's `memory.peak` directly, gives "peak 11.2 GB of 12; try `--size large`" in the result and a right-sizing chart in History. It also makes the OOM retry smart: retry an OOM once on the next size up and tell the agent. This is a small change with a large effect on both "agents fail less" and "Tyler sees what it costs".

**Events instead of polling.** Incus has a `/1.0/events` websocket (what `incus monitor` uses) for lifecycle events. boxd could react to a box starting or a freeze instead of polling the list. One caveat from Incus's docs: a slow listener can be dropped, so after every reconnect do one full list to reconcile. This is a refactor more than a feature, so do it when the dashboard next needs to feel more live.

**Small honest signals.** Salad's pause screen names why it paused. Every queued job's "why queued" can say "paused: game `x.exe` since 21:14", and the tray can offer "pause until idle". Daytona separates auto-stop from auto-delete and resets the idle timer on activity; agent-box has one expiry that is refreshed to 24 hours at job start and cut to two hours at read. That works, but splitting "stop when idle" (frees RAM, keeps the disk and warm state) from "delete" (frees the disk) would keep warm repo boxes much longer at almost no cost. That ties directly to section 4.

## 7. Be a gentler neighbour

BOINC is the best prior art for computing on someone else's desktop. Its client has an `exclusive_app` list (any process that suspends everything) and a separate `exclusive_gpu_app` list (suspends GPU work only), a "suspend when the user is active" threshold, a setting for whether suspended work stays in memory, and a checkpoint interval ([BOINC client configuration](https://github.com/BOINC/boinc/wiki/Client-configuration)). Folding@home's idle detection famously misfires inside games, which is a reason to keep the tray's detection by process and GPU rather than by input idle time alone (I did not verify the details of that).

For agent-box, the ideas that fit:

- **Tiers in the tray config.** A `boxtray.json` list of process names that freeze everything, and a second list that stops only the GPU box. Today a full freeze is the one tool. A GPU-only app (a video encode, an AI tool) shouldn't stop `go test`.
- **Hysteresis on thaw.** Wait a minute or two after a game ends before thawing. Steam's `RunningAppID` flicks to zero between launchers and the game; a flapping yield thaws and freezes boxes repeatedly, and each freeze costs a swap-out.
- **A grace period before the freeze (marginal).** Vast.ai's interruptible instances get a short notice. A SIGTERM to the job's process group 10 to 20 seconds before the freeze would let a test runner flush its output. The freeze must still win on time, so this is a small, optional courtesy. I inferred it; I did not verify Vast's exact notice.
- **The reason on every queued job** as in section 6, because the person who has to trust the system is also the person who started the game.

## 8. Scheduling: a couple of cheap ones

The queue already has first-in-first-out with "fewest running jobs first". Slurm's classic fair share adds decay: each agent's recent CPU-seconds count against it with a half-life of a few hours, so a session that ran a hundred jobs this morning doesn't outrank one that has run none, forever or never ([slurm.schedmd.com](https://slurm.schedmd.com/classic_fair_share.html)). That's about a screenful of Go in the scheduler and replaces "fewest running" with a better measure. Kueue's admission checks suggest the same shape for "should this start": no game, enough disk, enough RAM. agent-box has these as ad hoc checks; naming them as gates would make "why queued" a list of failed gates. Both are small. Priorities, preemption between jobs, and quotas per team are fleet ideas, left out.

## 9. What Murmur does that agent-box doesn't

Most of the release and update story is already planned from Murmur. The gaps that remain:

1. **One-line install and a diagnosis command.** Murmur's `install.sh` and `install.ps1` download a checksummed release, put the binary on PATH and need no admin. agent-box has no installer, and `box join` must run "from a built binary at its final path". Once releases exist (the plan's `release.yml`), an `install.ps1` that does the same costs little and is what a second laptop would use. Alongside it, a `box doctor` that checks, in order: the `incus` client, the `pc` remote and its cert, the project, boxd's `/api/state` and version, the tray's heartbeat, the skill and hook installed versus this binary, and prints the first thing that is wrong and the fix. Today a failure of any of these reads as "exit 75, PC unreachable" and Tyler or an agent has to work out which.
2. **Agent docs inside the binary.** `mur docs [page]` prints the agent pages that a sandboxed agent can't fetch from GitHub. `box docs` could print the skill and troubleshooting page, and `box help <tool>` per tool. This matters for Codex and sandboxed agents, and it keeps the skill and the binary in step.
3. **A repo-level handshake.** `mur init` adds a short section to a repo's CLAUDE.md and AGENTS.md so any agent that opens the repo learns about Murmur and asks to join. The same for box: a `box init` that writes a short section ("heavy runs go to box; here is this repo's setup command, default size, and which command to use for the full suite") and a tiny `.box.json` or `.box.toml` per repo: setup command (run once into the prepared image of section 4), default size, cache paths to keep, timeout, muted tests. That file is also where section 2's muted list and section 3's retry rules would live, so the per-repo knowledge is in the repo, versioned, and visible to every agent.
4. **CI discipline.** Murmur's `ci-level.sh` maps the files a PR touches to how much CI to run (docs-only runs nothing, page-only runs Linux, Go runs everything), with a test of the rules; its `go-test-retry.sh` and `retried-tests.sh` retry a failed test once and count retries so a flaky test is visible; and `docs/testing.md` records timings with run numbers. agent-box's CI is small and runs on Linux and Windows, so this is only worth doing when the Windows job gets slow. Do copy the habit of recording baselines with dates.
5. **A session grace and an honest delivery rule.** Murmur's 60-second grace and "pushed again, not refused" are the model for the push watcher, which already follows them. Nothing to add beyond keeping that discipline.
6. **A page for versions and updates.** Murmur's About row ("Up to date", "X available", "Checked 5 minutes ago") is already in the release plan as "dashboard shows each part's version", so nothing new.

## 10. Multi-host (issue #8)

Nothing here should be built now; a few earlier choices make it cheaper later. Keep the tray's mode and the queue's "gates" per host. Keep the image and snapshot approach host-local, with the same alias on every host, so a job picks a host and the host builds its own warm image. The shared cache across hosts is the one place where GOCACHEPROG, sccache or a Bazel-style remote cache would start to matter; with one host the shared `/cache` volume is enough. Timing-based test splitting (CircleCI's and Buildkite's splitting by recorded durations, with a median for tests never seen) needs more than one box running at once to pay; mark it as the multi-host or Full-mode item.

## Appendix A. The ten to do first

Ranked by value for the effort. Size: S is a day or less, M a few days, L a week or more.

| # | Idea | From | Why it fits | Size | Touches |
|---|---|---|---|---|---|
| 1 | **Failure digest**: wrap `go test -json` and vitest's JSON reporter; lead the result with failed tests, assertion and file:line; keep the log behind `box_job` | [gotestsum](https://github.com/gotestyourself/gotestsum), [GitHub annotations](https://docs.github.com/actions/reference/workflow-commands-for-github-actions) | Agents get the answer without paging logs; kind detection already exists; saves tokens on every run | M | `cmd/box/runner.go` (wrapper), new parsers beside `internal/dash/kind.go`, `internal/jobrun`, `cmd/box/mcp.go` (`structuredContent`), `skills/box/SKILL.md`, the push message |
| 2 | **Prepared box per repo**: publish or snapshot a repo's box after a good run, launch new boxes as CoW clones | [Incus snapshots](https://linuxcontainers.org/incus/docs/main/howto/instances_backup/), [Namespace devboxes](https://namespace.so/blog/changelog-020), [Coder prebuilds](https://coder.com/docs/admin/templates/extending-templates/prebuilt-workspaces) | Biggest cut to time-to-result on btrfs; makes `--new` and queued jobs start in seconds | M to L | `cmd/box/box.go` (launch path), `internal/boxd` (queue starter), the reaper and `snapshots.expiry`, `provision/`, DESIGN.md |
| 3 | **Retry classes and honest timeouts**: retry infrastructure failures once (yield, OOM, disk, restart), never test failures; frozen time doesn't count; a separate `starting` state | [AWS Batch evaluateOnExit](https://docs.aws.amazon.com/batch/latest/userguide/job_retries.html), [Buildkite retry](https://buildkite.com/docs/pipelines/configure/step-types/command-step), [Kubernetes podFailurePolicy](https://kubernetes.io/docs/concepts/workloads/controllers/job/) | A game freeze should not read as a failed job or a timeout; fewer wasted agent turns | M | `internal/jobrun` (wrapper, exit file, timeout), `internal/boxd` scheduler, `cmd/box/runner.go`, `internal/dash/types.go` (states), push text |
| 4 | **`/cache` as per-tool btrfs subvolumes, promote on exit 0**, with size and age caps | [Namespace cache volumes](https://namespace.so/docs/architecture/storage/cache-volumes), Blacksmith sticky disks | Stops killed or frozen jobs from poisoning the cache every box reads; makes the 50 GiB bound real per tool | M | `provision/setup-host.sh`, `provision/default-profile.yaml`, job wrapper in `internal/jobrun`, reaper |
| 5 | **Peak usage per job, and the OOM retry on a bigger size** from Incus metrics or cgroup `memory.peak` | [Incus metrics](https://linuxcontainers.org/incus/docs/main/metrics/) | Tyler sees what jobs cost; agents stop guessing `--size`; makes idea 3's OOM case smart | S to M | `internal/boxd` (sampling, history), `internal/dash` (History chart), `cmd/box/runner.go` (message line), `internal/mode/size.go` |
| 6 | **Postmortem snapshot and a reason on every state change**, with "reopen failed box" on the dashboard | [Modal exit snapshots](https://modal.com/changelog/exit-snapshots-resume-sandbox-preserve-state), [E2B lifecycle events](https://docs.e2b.dev/sandbox/lifecycle-events-webhooks) | "Why did that die" answerable by looking; fits the reaper and the dashboard already planned | S to M | reaper, `internal/boxd` (history, controls), `internal/dash/static`, `internal/mode/manager.go` (reasons) |
| 7 | **Flaky tracking and a muted list**, keyed by tree hash; `--rerun-fails` with "flaky, passed on attempt 2"; never rerun after a race | [Nx flaky tasks](https://nx.dev/ci/features/flaky-tasks), gotestsum `--rerun-fails`, Buildkite quarantine, Murmur's `go-test-retry.sh` | Shows Tyler which tests waste his agents' time; agents learn to ignore noise safely | M | `internal/boxd/history.go`, digest from #1, per-repo config (#9), skill text |
| 8 | **Yield tuning**: process lists for "freeze all" and "GPU only", a thaw delay, a grace SIGTERM, and the game's name on every queued job | [BOINC client configuration](https://github.com/BOINC/boinc/wiki/Client-configuration), [Salad](https://support.salad.com/article/380-temporary-workload-block) | Fewer needless freezes, no flapping, and queued jobs say why they wait | S to M | `cmd/boxtray/detect.go`, `config.go`, `internal/mode/manager.go`, queue "why" text, `docs/ui.md` |
| 9 | **Onboarding kit**: `install.ps1`, `box doctor`, `box docs`, and `box init` with a per-repo `.box` config (setup command, default size, cache paths, muted tests) | Murmur's `install.ps1`, `mur docs`, `mur init`, [Murmur releasing](https://github.com/a-Fig/murmur/blob/main/docs/releasing.md) | A second laptop (or Tyler after a reinstall) gets a working box in a minute, and a failure says which part is broken; holds the per-repo knowledge the other ideas need | M | new `cmd/box/doctor.go`, `docs.go`, `init.go`; `install.ps1` at root; `internal/version`; `skills/skills.go`; release workflow assets |
| 10 | **Supersede and replay**: a new job can cancel the same session's older job for the same repo and command; an identical tree, command and size can answer from the last clean result; warn on `-count=1` | [Buildkite concurrency groups](https://buildkite.com/docs/pipelines/configure/step-types/command-step), Bazel and Turborepo caching | Frees capacity and gives instant answers for repeat runs; cheap given tree hashes already exist | S to M | `cmd/box/runner.go`, `internal/boxd/queue.go`, history store, skill text |

Next after these: Slurm-style fair-share with decay (S), driving the dashboard from Incus events (M), splitting idle-stop from delete so warm boxes live longer (S), Gradle cache settings for Android runs (S).

## Appendix B. Smaller ideas, by theme

**Results and tests**
- Put `file:line: message` failures first and fold the raw log, as GitHub step summaries do (part of #1).
- Vitest: `--silent=passed-only` plus `--bail` on a first run, `--last-failed` on reruns; `--retry.condition` for a known-flaky pattern. Fold these into the wrapper for #1.
- `gotestsum tool slowest` to show the top five slow tests per run in History, and per-repo median run time (the dashboard already plans the median).
- Timing-based test splitting across boxes (CircleCI, Buildkite) is for Full mode with several boxes free. Marginal until then.
- Replaying the stored log for an identical commit and command (Turborepo): see #10.

**Speed and caches**
- A warm pool of one box per size and zero while yielding (Coder prebuilds). Marginal; the prepared image gets most of the value.
- A published image should record the `agent-base` version it came from and rebuild when that changes.
- A Gradle daemon kept warm by a warm repo box (see section 5).
- A caching package proxy (npm, Go modules, apt) on the PC, so even a cold box pulls from the LAN. Marginal because the shared `/cache` mostly covers it.

**Reliability**
- Backoff after a failed box launch (Coder). Cheap.
- A `starting` state and a "why queued" string made of named gates (Kueue admission checks).
- After an unattended PC reboot, the headless enforcer already holds the line. Wake-on-LAN is already deferred in DESIGN.md.

**Visibility**
- Incus events for the dashboard, reconciling with a list after each reconnect.
- Export `/1.0/metrics` to Prometheus and the existing Grafana dashboard (ID 19727) only if Tyler wants it outside boxd's own History. Marginal; boxd's own view is better matched to jobs.
- Splitting idle-stop from delete with an activity-refreshed idle timer (Daytona).

**Security**
- Default-deny egress with an allowlist (the package registries, GitHub, the tailnet) and secrets injected at a proxy instead of in `env` (Cloudflare Sandbox). Marginal for one trusted user and trusted agents, but the DESIGN doc already lists egress allowlists as deferred. It protects against a poisoned dependency running in a box that has the cache volume.
- Keep the restricted certs and project caps as they are.

**Agent ergonomics**
- Surface retries, flakiness and muted tests as separate lines in the result so an agent can skim.
- `box help <tool>`; per-tool examples in the MCP descriptions kept short.

## Appendix C. Considered and rejected

- **Memory or CRIU snapshots of running boxes (Fly, Modal, E2B, Firecracker).** Incus containers can't do it dependably, and freezing with swap-out already covers the real need.
- **A warm pool of idle boxes.** One PC, shared with games; idle boxes hold RAM and caps for no gain. A prepared image gets the speed without it.
- **Kubernetes, Kueue, Nomad, Slurm as a scheduler.** Right ideas, wrong weight. Steal the fair-share formula and the failure-policy shape, not the system.
- **Quotas, priorities between teams, preemption of running jobs.** Fleet scale. The yield already preempts the only thing that matters.
- **Bazel or a remote cache service (Depot cache, Nx Cloud, Turborepo remote).** The shared volume does the job for one host. Revisit with #8.
- **GOCACHEPROG and sccache now.** Only useful across hosts, and sccache is for C and C++ and Rust, which Tyler's repos mostly aren't.
- **Self-hosted GitHub Actions runners or Actions Runner Controller on the PC.** Tempting, but boxes serve agents on a laptop, not CI, and it would put a public repo's jobs on the gaming PC. Blacksmith and Namespace are useful as a source of ideas only.
- **BOINC-style work units and checkpointing inside the job.** The jobs are arbitrary commands. Freezing is the checkpoint.
- **Vast.ai or Salad style marketplace, billing, and per-agent cost accounting in money.** There is nothing to bill. Peak resources per job is the useful version.
- **Remote whole-agent sessions (Coder, Gitpod).** Already listed as deferred in DESIGN.md. Different product.
- **Census-style usage telemetry (Murmur).** A single-user tool has nothing to count that boxd's history doesn't.
- **Murmur's per-OS CI sharding for the Windows job.** Not needed until agent-box's own Windows tests take longer than a few minutes.
- **Skipping flaky tests.** Muting keeps the signal. Skipping loses it.
- **Rerunning after a data race.** A race is a finding; rerunning hides it.

## Not verified

Salad's detection internals, Vast.ai's preemption notice, E2B's lifecycle event payloads, and Fly and Morph timings came from the helper's reading and were not checked. The `go test` cache flag rules and the Nx, Buildkite, gotestsum, AWS Batch, E2B pause timings, Coder and Incus metrics points were read from the docs linked above. I did not read `cmd/box/snapshot.go` closely, so whether the bundle is already incremental is open.
