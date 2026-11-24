# agent-box design (v2: Incus-native)

On-demand compute for coding agents on Tyler's Windows gaming PC (`desktop-l0rvq1v`: Ryzen 5 3600 6c/12t, 32 GB, RTX 3070).
**Never wipe, repartition or dual-boot the PC.** Everything runs on top of Windows.
**Reuse, don't rebuild:** Incus provides the API, auth, dashboard and box lifecycle. We write only glue.

## Goals

1. Agents on the laptop can get disposable Linux boxes.
2. Agents can offload heavy test runs from a laptop git worktree (Go `-race`, vitest, Android unit tests).
3. Tyler picks how much of the PC agents get (My PC / Shared / Full) with one click, and games always win.

## What we use as is

| Need | Existing tool |
|---|---|
| Linux host on Windows | WSL2 distro `agents` (Ubuntu 24.04, systemd on, automount + interop off, VHDX on the biggest drive, `sparseVhd`) |
| Boxes, API, auth | **Incus** from the Zabbly repo (Ubuntu's 6.0.0 has nested AppArmor issues), HTTPS API on `:8443` |
| Network reach | Tailscale **inside** the distro as node `agent-box`; WSL stays on NAT networking |
| Isolation for agents | Incus project `agents` + **restricted client certs** (`incus config trust add --restricted --projects agents`); agents can't touch limits, profiles or other projects |
| Dashboard | Incus web UI (`incus-ui-canonical`) at `https://agent-box:8443` |
| Client on the laptop | Windows `incus` client (`winget install LinuxContainers.Incus`), `incus remote add pc` |
| Image | `images:ubuntu/24.04/cloud` + cloud-init in the `agents` profile (git, build-essential, Go, Node LTS, uv, JDK 17, gh), published once as `agent-base` |
| Caches | Custom storage volume `cache` mounted at `/cache` (Go, npm, Gradle, uv caches) |
| Keeping WSL up | Scheduled Task at boot: `wsl -d agents --exec sleep infinity` |

The `agents` project's default profile sets a per-box `limits.cpu`/`limits.memory` (required so project caps count them), `security.nesting=true`, the cache volume, and cloud-init. Project caps (`limits.instances`, sum of `limits.cpu`/`limits.memory`) bound what agents can provision. They're allocation caps, not usage caps.

## Glue we write

### 1. `box` (Go, one binary, laptop): sync-and-run + a small MCP server
It shells out to the `incus` CLI, using the `pc` remote and the `agents` project.

```
box new [name] [--ttl 8h]        # incus launch agent-base pc:<name> --project agents, with a user.expires label
box run [--box name|--new] -- <cmd...>
box rm <name>
box ls
box mcp                          # stdio MCP with only: box_new, box_run, box_job, box_rm, box_ls
```

`box run`:
1. Snapshots the worktree as a git commit without touching the user's index: a temporary `GIT_INDEX_FILE`, add everything not ignored, then `write-tree` and `commit-tree`. It sends a `git bundle` with `incus file push` and checks the commit out in `/work/<repo>`.
2. Runs the command with `incus exec ... -- bash -lc`, under `nohup` with output going to `/work/.box/<job>.log`, so a dropped connection loses nothing. `box job <id>` reattaches with `tail -f`.
3. Exits with the remote exit code, or 75 when the PC is paused, so callers can fall back to running locally.

By default it reuses one box per repo, which keeps caches and `node_modules` warm. `--new` creates a throwaway box.

### 2. Tray app (Windows): mode switch + gaming yield
- **My PC:** `incus pause` every running instance in `agents`. Boxes still frozen after 15 minutes are stopped to free RAM. A "Release memory" item runs `wsl --terminate agents`.
- **Shared:** resume, and set `limits.cpu.allowance=50%` + `limits.cpu.priority=0` live on each instance; project `limits.instances=2`.
- **Full:** resume, unset the allowance, `limits.cpu.priority=10`; project `limits.instances=3` (the project caps of 12 CPUs / 12GiB at 2 CPUs / 4GiB per box allow 3).
- **Gaming yield:** Steam `RunningAppID` != 0, or `SHQueryUserNotificationState` full-screen/busy, behaves like My PC until the game ends.
- It uses an admin client cert, which agents don't have. The mode is stored in the `agents` project config as `user.agent-box.mode`, so `box` can read it.
- The tray also holds a Windows power request while any instance is running, so the PC doesn't sleep in the middle of a job.

### 3. Provisioning (`provision/`)
`bootstrap-ssh.ps1`, `install-pc.ps1` (WSL, distro import, `.wslconfig` with 16 GB / `autoMemoryReclaim=gradual` / `vmIdleTimeout=-1`, the scheduled task, the tray at logon), `setup-distro.sh` (Incus + UI, preseed, project/profile/volume, Tailscale, certs, image).

## Deferred
Wake-on-LAN via homeserver (`wakeonlan`), automatic sleep, GPU (`/dev/dxg` passthrough for the RTX 3070), VMs (needs `/dev/kvm` in WSL), egress allowlists (see pixels), and running whole agent sessions remotely (look at Coder or coi then).

## References
code-on-incus (local only; profile and egress ideas), pixels (remote Incus backend, checkpoints, egress allowlists), Containarium and Coder (heavier control planes; not adopted). Incus docs: projects (limits are allocation caps), instance options (`limits.cpu.allowance` and `limits.memory` update live).
