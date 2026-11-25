# agent-box for everyone

_A snapshot from before #149, which renamed `provision/setup-distro.sh` to `setup-host.sh` and did plan items 2 and 3; the file:line references point at the old script._

What stands between a stranger and a working install, as of 2026-11-25 (dev at `c6224c3`). Today
agent-box runs on one setup: Incus in a Windows PC's WSL2 distro, reached over Tailscale, used by
agents on one laptop, mostly through T3 Code.

## What's hardcoded to this setup

**Blocks a stranger**

- **The host must be the WSL distro.** `provision/setup-distro.sh:2` and its messages assume WSL
  (`:69` points at `/etc/wsl.conf`). `:53` refuses anything but Ubuntu. `:57` needs kernel 6.12+,
  which stock Ubuntu 24.04 (6.8) fails, and says to run `wsl --update`. Its only entry point is
  `install-pc.ps1`, though most of it would work on native Ubuntu.
- **No installer for `box`.** `README.md:25` says `go build ./cmd/box`. Releases do ship
  `box-linux-amd64`, `box-darwin-arm64` and `box-windows-amd64.exe` (v0.1.2), but no script
  downloads one, there's no Intel Mac or arm64 Linux build, and `box join` has to run from the
  binary's final path.
- **Tailscale is required for the dashboard and the queue.** boxd trusts only the
  `Tailscale-User-Login` header from `tailscale serve` (`internal/boxd/auth.go:70-75`), names devices
  with `tailscale whois` (`internal/boxd/controls.go:52`), and `box` reaches it at `https://<remote host>/`
  (`cmd/box/meta.go:330`). The trusted certificate comes from `tailscale cert` (`provision/https-cert.sh:13`).
  Without Tailscale, `box run` still works directly through Incus, but a run that doesn't fit is
  refused (exit 75) instead of queued, and there's no dashboard. boxd is only installed with
  `install-pc.ps1 -BoxdPath` (`setup-distro.sh:353`).
- **An existing Incus host is assumed to be empty.** The preseed is skipped if a pool named `default`
  exists (`setup-distro.sh:120`), but the profile still expects that pool to be btrfs and a bridge named
  `incusbr0` (`provision/default-profile.yaml:298-306`). A homelab with a ZFS or LVM pool, or a
  differently named bridge, breaks.

**Works, but wrong for most machines**

- **Sized for one PC.** CPU, disk and box caps (`setup-distro.sh:26-29`: 24 CPUs, 210GiB, 10 boxes), a
  200GiB pool (`provision/incus-preseed.yaml:24`) and memory budgets of 12/24GiB
  (`internal/mode/size.go:40`). A 16GB machine gets a Full budget bigger than its RAM.
- **The GPU is WSL-only.** `--gpu` attaches `/dev/dxg` and `/usr/lib/wsl/*` (`internal/mode/size.go:55-65`;
  `setup-distro.sh:211`). On native Linux, an NVIDIA card needs Incus's `gpu` device instead. Docs
  name this PC's card (`README.md:35,61`).
- **The tray is Windows and WSL only.** It drives `wsl.exe -d agents` (`cmd/boxtray/sensors_windows.go:17`,
  `internal/mode/incus.go:53-56`). Without it the mode stays Shared (`setup-distro.sh:164`),
  which is fine once written down.
- **Push is T3-only.** Results reach the agent only through a T3 thread
  (`cmd/box/meta.go:113` `T3CODE_THREAD_ID`, `cmd/box/push.go:88-90` `t3.cmd`). In plain Claude Code,
  Codex or Cursor, `box_run` returns `push: false` and the agent waits in `box_job` for up to 10
  minutes per call.
- **`box join` covers Claude Code, Codex and Antigravity** (`cmd/box/join.go:63-64,92`), not Cursor.
  It registers `box mcp` with no environment, so `BOX_REMOTE` (`cmd/box/box.go:104-106`) must already be
  set globally, or the remote must be named `pc`.
- **Fixed names.** Tailscale hostname `agent-box` (`setup-distro.sh:258`), project `agents`, trust
  name `laptop-agents` (`:437`), boxd's machine ID `pc` (`internal/boxd/auth.go:59`).
- **Exposure.** Incus listens on `[::]:8443` (`incus-preseed.yaml:10`). Behind WSL's NAT that is only
  reachable through Tailscale. On a native server it is on every interface, guarded only by
  client certificates.

**Words and docs.** Agent-facing text says "the PC", "My PC" and "the PC's owner"
(`cmd/box/mcp.go:59,66,72`; `skills/box/SKILL.md:11,16`). That reads fine on any shared machine, but the
README's first line (`README.md:3`) promises a Windows gaming PC. `CLAUDE.md` and `docs/DESIGN.md:3`
name this setup's machine, tailnet and addresses in a public repo.

## A stranger's first hour

**A Linux server or homelab running Incus.** Installing the host fails at once: no Linux entry
point, and `setup-distro.sh` is in `provision/` with a WSL name. A stranger who runs it anyway on
Ubuntu with a 6.12+ kernel gets most of the way: Incus from Zabbly, the project, profile, cache and
Tailscale. On Debian it stops at `:53`. With Incus already there, it can trip on the pool and bridge.
Building `agent-base` takes a while and needs `--build-base`. Connecting: setup prints the trust
commands, then `incus remote add pc https://<host>:8443 --token …`; the self-signed certificate is
fine, since the token carries its fingerprint. Using it from an agent:
download a release binary by hand, put it on PATH, run `box join`. Claude Code and Codex then have
the tools. Cursor needs the MCP server added by hand. Without T3, every long run blocks the agent in
`box_job`. Without Tailscale there is no queue or dashboard.

**A Mac.** As a host: Incus doesn't run natively on macOS, and nothing here supports a Linux VM
such as Colima's Incus runtime. As a client it works: `brew install incus`, the `box-darwin-arm64`
release (Apple silicon only) and `box join`. `box app` has no macOS window yet.

**A Windows PC like ours.** This is the path that works: `install-pc.ps1` as admin with a Tailscale
auth key, HTTPS certificates turned on in the tailnet's DNS settings, and `-BoxtrayPath`/`-BoxdPath`
pointing at release binaries. It breaks or bends on smaller machines (fixed caps), non-NVIDIA GPUs
(`--gpu` won't work), and a second install on the same tailnet (the name `agent-box` is taken). The
README never puts these steps in order for a newcomer. Clients connect as on Linux, through Tailscale.

## What Murmur does that's worth copying

From a-Fig/murmur's README, `docs/agents/join.md` and `install.sh` (read 2026-11-25):

1. **One-line installers** (`curl … install.sh | sh`, `irm … install.ps1 | iex`). They pick the OS and
   architecture, download from the latest release, check `SHA256SUMS`, install to `~/.local/bin` with
   no sudo, and accept a pinned version and a `GITHUB_TOKEN`.
2. **A page written for the agent.** `docs/agents/join.md` tells the agent to ask its user first and
   lists everything joining changes, including updates and their opt-out. On a yes, the agent installs
   and joins by itself.
3. **The repo tells agents.** `mur init` adds a short section to `AGENTS.md` and `CLAUDE.md`, so an
   agent that reads it knows to offer joining.
4. **Join and leave are symmetric.** `join --leave` and `uninstall` undo everything.
5. **No server, no accounts.** agent-box's counterpart: need only Incus and its client certificates,
   with Tailscale and boxd as extras.
6. **A support table**: what works in which agent, in T3 and in a terminal.

## Plan: Linux server to running tests in about 10 minutes

1. **`install.sh` / `install.ps1` for `box`**, copied from Murmur's. Add `box-linux-arm64` and
   `box-darwin-amd64` to the release. It installs the binary, and `box join` runs from there.
2. **A native Linux host script.** Rename or wrap `setup-distro.sh` as `setup-host.sh`. Drop the WSL
   wording, accept Debian, skip the WSL GPU keys off WSL, and make Tailscale opt-in (`--tailscale`).
   Size caps from `nproc` and RAM (environment overrides already exist), set the memory cap once,
   and say that no tray means Shared. Refuse clearly on a non-btrfs pool or a missing `incusbr0`,
   rather than half-configuring. Try it on a fresh Ubuntu VM with a new kernel.
3. **End setup with the client's one line**: `incus remote add pc https://<host>:8443 --token <t>`.
4. **README quick start for a Linux server**, three blocks: host, client, agent. Leave the Windows PC
   path in `provision/README.md` and link to it.
5. **`docs/agents/join.md`** in Murmur's shape: ask the user, say what changes, then install and
   `box join`. Add a manual Cursor `mcp.json` snippet and a support table; without T3, results
   come through `box_job`.
6. **Measure the `agent-base` build** on that VM. If it eats most of the 10 minutes, publish the image
   as a release asset to import, but only then.

Later, and only if people ask: Tailscale-free boxd auth, an NVIDIA `gpu` device on native Linux, and
push outside T3. Clearing this setup's machine names and addresses out of `CLAUDE.md` and
`docs/DESIGN.md` is a separate small change.
