# agent-box: notes for Claude Code

agent-box turns Tyler's Windows gaming PC into on-demand Linux boxes that coding agents on the
laptop use for heavy compute (test suites, `-race`, builds), and games always win. Incus, in a
WSL2 distro on the PC, does the boxes, API, auth and web UI; this repo is the glue: `box` (CLI and
MCP server for agents), `boxtray` (modes and gaming yield on the PC) and `provision/`. Live on the
PC since 2026-11-24. `docs/DESIGN.md` is how it works and why; read it before changing
architecture. `go test ./...` runs everything; CI runs it on Linux and Windows (`docs/testing.md`).

## Writing for whoever comes next

Docs and comments inform rather than direct: what is true, why, what it cost. Directions are
Tyler's rules, marked as his. Facts that can go stale carry a date. Each fact lives in one place:
this file loads into every session, so it stays short and points into `docs/` for detail.

## Words that mean two things

- **PC, laptop.** The PC is the gaming desktop `desktop-l0rvq1v` (Tailscale 100.120.165.107),
  where boxes run. The laptop is where agents and their worktrees live.
- **agents (distro), agents (project).** The WSL2 distro on the PC is `agents`; it joins the
  tailnet as `agent-box` (100.88.172.93). The Incus project the boxes live in is also `agents`.
- **pc (remote).** The laptop's Incus remote for the PC, `https://agent-box.tail051d13.ts.net:8443`,
  with a restricted cert. Incus there serves a Let's Encrypt cert for that name (renewed weekly).
- **box.** One Incus container in project `agents`: throwaway compute, never storage. Also the CLI.
- **mode, yield.** The tray's choice: My PC (boxes frozen), Shared (2 CPUs a box), Full (4).
  Yield is My PC forced while a game runs. The tray writes `user.agent-box.mode` and
  `user.agent-box.reason` on the project; `box` only reads them.

## Traps

- **Never wipe, repartition or dual-boot the PC** (Tyler's rule). Everything runs on top of
  Windows; ask before anything that touches its disks or boot config.
- `incus` create and edit commands read YAML from stdin when it isn't a terminal. Over SSH or in
  a pipe they hang or eat the rest of a script: give them `</dev/null`.
- The tray rewrites the mode key and box CPU limits every few seconds. To change the mode, change
  the tray (`%APPDATA%\agent-box\boxtray.json`, then restart it), not the key.
- Over SSH, `$env:USERDOMAIN` on the PC is `WORKGROUP`; use the Windows identity's name.
- WSL's ext4 has no project quotas, so the storage pool is btrfs: only it enforces disk sizes.
- systemd in WSL counts as a container, so units with `ConditionVirtualization=!container` (such
  as `incus-lxcfs`) are skipped unless a drop-in clears the condition.
- On the laptop, run Go only through `.scratch/golock.sh` (one Go run at a time) and never with
  `-race`: the laptop starves. CI runs `-race`.

## Workflow

Tyler's rules:

- Reuse existing tools and open source before writing code; keep custom code thin.
- Go: standard library first; a dependency needs a written reason. `gofmt`, `go vet ./...` and
  `go test ./...` pass. Code that needs Incus, WSL or Windows sits behind an interface, so the
  rest is tested with fakes.
- Work on a feature branch and fold it into `dev`. `main` changes only through a PR that Tyler
  merges.
- Commit messages and PR bodies explain *why*. PR bodies follow
  `.github/pull_request_template.md`; `gh` doesn't pre-fill it, so write its sections.
- Keep what agents read lean and current: the `box` skill, MCP tool descriptions, `box` help text
  and the README. A behavior change updates them in the same PR. They load into other agents next
  to real work, so each line earns its place.
- Every PR gets an Opus, Fable or Sol review before it is handed over, and its CI is green.
  Docs-only PRs skip both. The PR body says which commits the review covered.
- A change to provisioning or the tray is tried on the real PC before it merges, and the PR says
  what ran (`docs/testing.md` has the live smoke test).
- UI follows Murmur's look (Tyler, 2026-11-23).

## Layout

`cmd/box` CLI + MCP · `cmd/boxtray` tray · `internal/mode` mode logic and reaper ·
`provision/` PC setup · `docs/`.

<!-- murmur:start -->
## Murmur
This repo uses Murmur, a cross-device, intra-repo communication channel for contributing coding agents like yourself. If you have not joined Murmur yet, you should.

- To join Murmur (ask your user first): https://raw.githubusercontent.com/a-Fig/murmur/main/docs/agents/join.md
- To use Murmur (no tools? in T3 Code, `mur who` and `mur send` work in your shell): https://raw.githubusercontent.com/a-Fig/murmur/main/docs/agents/use.md
<!-- murmur:end -->
