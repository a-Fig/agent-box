# v0.1.0: go/no-go (2026-11-25)

**Go: tag v0.1.0 on `9ae1fde`** (main, the dev→main merge #126, frozen until the tag; its
nightly is `v0.0.1-nightly.20261125.10`). Then run the three hand steps
below once. Every installed part is a dev build, and a dev build never updates itself, so the tag
alone changes nothing on either machine.

```sh
git fetch origin && git tag v0.1.0 9ae1fde && git push origin v0.1.0
```

release.yml then builds and publishes the release (about 4 minutes). It is the first stable
release: GitHub's `releases/latest` is 404 until then, and nightlies become `v0.1.1-nightly.*`.

## After the tag, once

Each part was checked against a test prerelease of `dc68a60` (`v0.0.0-test.13`, deleted
afterwards), and nothing in these steps changed after it; see Checked.

1. **Laptop, box.exe:** `box update --version v0.1.0`. It swaps `C:\Users\smash\bin\box.exe`
   for the release and keeps `box.old.exe`. Sessions whose `box mcp` started after #74
   (2026-11-25 00:24 local) pick it up at their next tool call. Older sessions keep running
   their own old code until they restart (#107).
2. **PC, tray and headless enforcer:** install while they run, then restart them. A dev build
   doesn't restart into the new file by itself:
   ```powershell
   C:\agent-box\bin\boxtray.exe --update v0.1.0
   Get-CimInstance Win32_Process -Filter "Name='boxtray.exe'" | % { Stop-Process -Id $_.ProcessId -Force }
   Start-ScheduledTask agent-box-enforcer   # and agent-box-tray, if logged on
   ```
3. **PC, boxd:** `agent-box-update-boxd v0.1.0` as root in the `agents` distro. boxd was
   redeployed as a dev build with `deploy-boxd.sh` after this check, and the update timer leaves a
   dev build alone. Once it is on v0.1.0, the timer takes every later stable release, so routine
   releases no longer need `deploy-boxd.sh`.

From then on, all three follow stable releases by themselves: box.exe in `box mcp` and boxtray
hourly, and boxd from its hourly timer.

## What's in it

Everything since agent-box went live on the PC (2026-11-24), in 20 dev→main merges:

- **Boxes:**
  - `box run` syncs the worktree to a box on the gaming PC and runs a command there: named boxes
    per repo, `--new` throwaways, and sizes small, medium, large and gpu.
  - Expiry and a reaper. A throwaway is kept 30 minutes for `box pull`.
  - Caches in `/cache`, Go, Node, Android SDK, Chromium libraries.
- **Agents:**
  - The `box` MCP server and skill.
  - Results pushed back into the agent's T3 thread, plus a Claude Code hook for mid-turn ends.
  - A failure digest for `go test` and vitest.
  - Every run states its reason.
  - Each tool call runs the box.exe on disk (#74).
- **The PC owner:**
  - Tray modes: My PC, Shared and Full. They are manual; game detection was removed (#83).
  - A headless enforcer when nobody is logged on.
  - Frozen boxes' memory goes to swap.
  - The dashboard and its app, at `https://agent-box.<tailnet>/`.
- **Queue:** runs wait on the PC instead of being refused. A queued job evicts finished
  throwaways for room, now also when disk is the cap (#103).
- **Since the full check (`dc68a60`, #119 to #126):** digest fixes for Go and vitest,
  `box join` for Antigravity, `box pull` from a GPU box My PC stopped, "no stable release yet"
  in place of GitHub's raw 404, and clearer My PC and GPU messages.
- **Releases:** versions from tags; nightlies; self-update of box.exe and its skill, boxtray and
  boxd; `--rollback`. This is the first stable release.

## Re-checked (2026-11-25, against `9ae1fde`)

The fast part of the checklist used the rolled-out `box 9ae1fde`, CLI and a fresh `box mcp`. The
PC ran tray `dc68a60` and boxd `eb6e478`. All passed:

- **Smoke:** a new box at 2h; `go test ./internal/...` on it, back to 2h; detached at 24h, then
  reattached with exit 7 and the full log, back to 2h; `rm`.
- **Throwaway:** it runs and is kept 30 minutes.
- **Pushed result:** `push: true`, ended and delivered.
- **Failure digest:** a failing `go test` leads with `FAIL: 1 of 2 tests failed (go test)` and
  `TestBroken  dg_test.go:9: want 3, got 2`, exit 1.
- **GPU refusal:** a second `--gpu` exits 75, "the GPU box is in use (job …); run on CPU or
  wait".
- **My PC:** a CLI run exits 75, "pc is in My PC mode; run it locally".
- **Queue:** `queued`, 1, `mypc`, and it started 2s after the switch back to Full.

## Checked (2026-11-25, against `dc68a60`)

`box` built from main. MCP calls went through a fresh `box mcp` from that build. The PC was in
Full, with Incus 7.6.

| Check (docs/testing.md) | Result |
|---|---|
| Smoke: new box, 2h expiry, `go test ./internal/...` on it, expiry back to 2h | pass |
| Detach and reattach: 24h while running, exit 7 and the full log, back to 2h | pass (a killed `box run` stood in for Ctrl-C) |
| Restricted remote: `project set limits.cpu` refused ("Certificate is restricted") | pass |
| Throwaway: runs and prints; kept 30 minutes for `box pull` | pass (testing.md still says it's deleted at once; outdated since #86) |
| `box status` in the repo and outside Git | pass |
| `--env`; `--timeout` 124; `--timeout` with TERM trapped, 124 after the grace period | pass |
| `box jobs`, log page (`cdef`, next offset 6), cancel 130 with no live child, `box pull` of a file and a directory | pass |
| Small box: size, 2GiB, 18GiB, 2 CPUs in Full; caps 10, 24, 24GiB | pass |
| Dashboard: `/` and `/api/state` 200, History counts | pass |
| Pushed result: `box_run` with `push: true`, end delivered by the watcher with `push-smoke-ok` | pass |
| Hook: `box join` adds, keeps and removes it in a scratch home; the fake job is told once | pass |
| Android unit tests (pi-vi, `SingleFlightTest`), through MCP | pass, `ANDROID-SMOKE-OK` |
| My PC (through `/api/mode`, read back after 4s) | pass: a CLI run exits 75 ("pc is in My PC mode; run it locally") without running the command |
| Queue | pass: an MCP run in My PC returns `queued`, position 1, `why: mypc`, and `/api/queue/<job>` agrees. Back in Full, it started within 2s |
| Paused mid-run | pass: a running MCP job paused 3s after the switch, and `box job` exits 75. It resumed in Full; its pushes arrived as later turns of the thread |
| GPU box (run by lime-sieve) | pass: `nvidia-smi` sees the RTX 3070; a second `--gpu` exits 75, "the GPU box is in use". In My PC (mode read back after 4s) the box was Stopped within 1s, and `box job` exited 75: "the GPU box was stopped because the PC's owner switched it to My PC". `user.box.stopped=mypc`, expiry the stop plus 30 minutes. PyTorch: `torch.cuda.is_available()` True (CUDA 13.2), and a 4096×4096 matmul took 590.7 ms on CPU and 9.6 ms on GPU. The first `uv` install of torch took 7m46s (about 3 GB); the next took 12s from `/cache/uv` |
| Release: release.yml on a `v0.0.0-test.13` tag of `dc68a60` | pass: 8 assets and `SHA256SUMS`, a prerelease |
| box.exe from main's dev build, in a `%TEMP%` copy with its own LOCALAPPDATA and home | pass: `box update` refuses a dev build and says to pass `--version`; `--version v0.0.0-test.13` installs it (`box v0.0.0-test.13 (dc68a60, …)`); a newer nightly installs by hand; `--rollback` goes back and skips that nightly |
| boxtray from main's dev build on the PC (headless, under a throwaway task) | pass: `boxtray --update v0.0.0-test.13` exited 0 with the dev build running, and put the release on disk. The dev build kept running, as designed. After its process was stopped and the task started, the release ran, wrote `tag: v0.0.0-test.13` to the heartbeat, and did its hourly check (404, no stable release yet) |
| boxd's update timer | Missing on the PC: setup-distro.sh last ran before the timer existed. Installed on 2026-11-25 as setup-distro.sh does it (hourly). Its first run said `boxd is a dev build (dc68a60), not a release: not updating` |
| boxd onto releases: `agent-box-update-boxd v0.0.0-test.13` | pass: `checksums OK`, the running boxd kept as `boxd.old`, the new one active and answering, `boxd v0.0.0-test.13 (dc68a60, …)`; `/api/state` 200 |

Earlier the same day: the tray's self-update and rollback, headless, live on the PC against
`v0.0.0-test.9`–`.12` (#106), and boxd's update path against a test prerelease (provision/README.md).

## Not checked

- **The interactive tray:** its hand-over at logon, its self-restart (WM_CLOSE), and whether its
  task allows breakaway from its job object. Nobody was logged on at the PC. The headless path,
  which covers a missing tray, was checked.
- **Memory reclaim to swap in My PC.** Not re-run this time; it passed on 2026-11-24.
- **`deploy-boxd.sh`.** Not re-run here; autumnal-coyote deployed boxd `eb6e478` with it the same day.
- **The hourly check against a real stable release.** It can't be checked before one exists.
  The by-hand install it shares code with was checked (above).

## Known, not blocking

- A box built inside a box reports the box's snapshot commit, not the repo's (`box 35c340e`
  for a build of `dc68a60`), because the snapshot is a commit of its own. Release builds come from
  release.yml and are unaffected.
- Commands in a box run as root, unlike most CI (README). One pi-vi test in about 6,000 depended
  on it.
