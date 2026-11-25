# Releasing and updating (plan, 2026-11-25)

Status: decided, being built. Tyler asked for a release process "clean and auto-updating, like
Murmur's" (2026-11-25). Murmur's is in a-Fig/murmur `docs/releasing.md` and `internal/update`.

## Why

On 2026-11-25 alone, agent-box shipped five releases by hand. Each one meant rebuilding
`box.exe` into `C:\Users\smash\bin`, copying the skill into three agent configs, and pushing
boxd to the PC. Two things went wrong that a release process would have prevented:

- The PC's boxd unit file had drifted from the repo, so every job scan failed.
- A setup re-run put back a stale boxd.

The tray has never been updated since it was installed. A second laptop, or the friend's
machine (#8), would multiply all of this.

## What ships

There are four parts, on two machines. Each one updates itself.

| Part | Runs on | Held open by | Updated by |
|---|---|---|---|
| `box.exe` (CLI, MCP server, hook, watcher) | laptop | every agent session's MCP server | `box` itself (below) |
| box skill (`skills/box/SKILL.md`) | laptop, 3 agent configs | nothing | `box`, with the binary |
| `boxtray.exe` (tray and headless enforcer) | PC, Windows | the tray, the `agent-box-enforcer` task | the tray itself |
| `boxd` and its systemd unit | PC, `agents` distro | systemd | a timer in the distro, through `install-boxd.sh` (#46) |

The `agent-base` image changes rarely and takes minutes to rebuild. It stays manual
(`setup-distro.sh --rebuild-base`), and a release says when it needs a rebuild.

## Cutting a release

A release is a pushed tag `vX.Y.Z` on `main`, as in Murmur.

1. Merge the dev→main PR, as today: CI green, and every PR in it reviewed.
2. Tag it. `.github/workflows/release.yml` builds the parts below, runs `govulncheck`, and
   publishes a GitHub release with `SHA256SUMS`:
   - `box-windows-amd64.exe`, `box-linux-amd64`, `box-darwin-arm64`
   - `boxtray-windows-amd64.exe`
   - `boxd-linux-amd64`
   - `agent-box-boxd.service`, `install-boxd.sh`
3. The version is the tag, built in with `-X` (`version.Tag`). Each binary's `--version` says it
   as its second word, `box v0.3.0 (e823842, 2026-11-25 07:12 UTC)`, and a plain `go build` says
   only its commit and never updates itself, as in Murmur. The release checks that box and boxd
   say the tag before it publishes.
4. `govulncheck` stops a release that reaches a known vulnerability. Its first run (2026-11-25)
   found twelve in Go 1.27.1's standard library, so `go.mod` moved to 1.27.4.

**Nightlies are automatic, stable releases are by hand** (Tyler, 2026-11-25).
- A push to `main` that passes CI is tagged `vX.Y.Z-nightly.YYYYMMDD.N` and published as a
  prerelease (`.github/workflows/nightly.yml`, on test.yml's run; it fires only from `main`'s copy
  of the file). X.Y.Z is the latest stable release with its patch raised by one, `v0.0.1` while
  there is none, and N counts that day's nightlies. A commit already out is not tagged again.
  Pruning keeps the newest seven, as in Murmur.
- A stable `vX.Y.Z` is tagged by hand, on a commit a nightly already ran.

**Channels.** Every part follows **stable** (Tyler, 2026-11-25): it takes GitHub's latest
release, which is never a prerelease. A nightly installs only by hand, through
`box update --version <tag>`. The guards are the dev→main PR, the hand-tagged stable release,
and a canary (below).

## How each part updates

All of them follow the same steps:
1. Check GitHub's latest release at most hourly.
2. Download the part's asset and verify it against `SHA256SUMS`.
3. Run the new binary with `--version`, and refuse it unless it prints the tag.
4. Swap it in, keeping the old one beside it as `.old`.

A part never moves to an older version, and `AGENT_BOX_NO_UPDATE=1` turns all of this off.

- **box.exe.** There's no daemon on the laptop. The check runs in `box mcp`:
  - at start, then hourly, under a lock file, with a stamp so 20 sessions don't all ask GitHub;
  - Windows lets a running exe be renamed, so `box.exe` becomes `box.old.exe` and the new file
    takes its name; running servers keep the old code, and new sessions and spawned watchers
    get the new one;
  - the same step installs the release's skill into each config whose skill still matches the
    old release's, and leaves a skill someone edited alone;
  - #34's tolerant schema already lets a session started before an update keep calling the new
    binary.
- **boxtray.exe.** The tray checks hourly. To swap itself it:
  1. stops the `agent-box-enforcer` task;
  2. renames itself to `.old` and moves the new file into its place;
  3. starts the new one and exits.

  The mutex hand-over that already exists between the tray and the headless enforcer covers the
  gap.
- **boxd.** An `agent-box-update.timer` in the distro runs `update-boxd.sh` hourly. It fetches the
  release's `boxd`, unit and `install-boxd.sh`, checks them and probes the new boxd, and runs that
  `install-boxd.sh`, which health-checks the new boxd and rolls back. A boxd that isn't a release
  (`deploy-boxd.sh`'s, or one from before `-version`) is left alone, so a boxd first goes onto
  releases by hand: `agent-box-update-boxd <tag>` as root in the distro. Tried on the PC on
  2026-11-25 against a test prerelease (provision/README.md).

## Safety

- **Canary.** The PC's parts update first. `box.exe` on the laptop waits until boxd reports
  the new version healthy on `/api/state` (#52) for 10 minutes. A release that breaks boxd then
  never reaches the agents.
- **Rollback.** `box update --rollback`, and the tray's menu, swap `.old` back in and pin that
  version until the next release.
- **What the checksum proves.** It catches a damaged download. It doesn't prove who built the
  file, as in Murmur. The repo is public, and only Tyler and his agents can push tags.
- **Visible.** The dashboard already shows each part's commit (#52), and will show its version.
  `box status` will say when the laptop is behind.

## Order of work

1. `release.yml` and `nightly.yml` (tag on a green main), versions from tags, `box update --version`.
2. boxd's update timer: the easiest part, since #46 did most of it.
3. box.exe's self-update and the skill sync, with the canary.
4. The tray's self-update, tried live with the tray running and with only the headless enforcer.
5. Retire the manual steps in the `box-rollout` notes and in this doc.

## Decided (Tyler, 2026-11-25)

1. Merges to main are tagged automatically as nightlies. Stable releases are tagged by hand.
2. Installed parts follow stable only.
3. Image rebuilds stay manual.
