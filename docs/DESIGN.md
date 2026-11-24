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
| Isolation for agents | Incus project `agents` (`restricted=true`, `features.profiles=false`) + **restricted client certs** (`incus config trust add --restricted --projects agents`); see Isolation below |
| Dashboard | Incus web UI (`incus-ui-canonical`) at `https://agent-box:8443` |
| Client on the laptop | Windows `incus` client (`winget install LinuxContainers.Incus`), `incus remote add pc` |
| Image | `images:ubuntu/24.04/cloud` + cloud-init in the `agents` profile (git, build-essential, Go, Node LTS, uv, JDK 17, gh), published once as `agent-base` |
| Caches | Custom storage volume `cache` mounted at `/cache` (Go, npm, Gradle, uv caches) |
| Keeping WSL up | Scheduled Task at boot: `wsl -d agents --exec sleep infinity` |

The default project's `default` profile (which `agents` uses; see Isolation) sets `limits.cpu=2`/`limits.memory=4GiB` per box, `security.nesting=true`, the cache volume and cloud-init. The Linux kernel must be 6.12+ for Incus 7.x (`install-pc.ps1` runs `wsl --update`; `setup-distro.sh` checks).

## Isolation

Project `agents` is created with `features.profiles=false`, so its boxes use the default project's profiles, and only the admin can edit those. `features.images=false` shares images. Neither can change after creation, so `setup-distro.sh` only checks an existing project and says how to recreate it if they are wrong. The project is `restricted=true` with `restricted.containers.privilege=unprivileged`, `restricted.containers.nesting=allow`, `restricted.devices.disk=managed` (the `cache` volume and root) and `restricted.devices.nic=managed` (`incusbr0`). Project caps are fixed at setup: `limits.instances=3`, `limits.cpu=12`, `limits.memory=12GiB` (3 boxes x 4 CPUs x 4GiB, the Full size). They are allocation caps, not usage caps, and the tray never changes them, because Incus refuses to lower them below what existing instances (stopped ones too) already use.

A restricted cert can still resume an instance and edit its own `limits.cpu`. So the modes are enforced by the tray's admin cert on every tick, not by permissions (see the tray below).

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

| Mode | Per box | Notes |
|---|---|---|
| My PC | paused | Boxes still frozen after 15 minutes are stopped to free RAM. A "Release memory" item runs `wsl --terminate agents`. |
| Shared | `limits.cpu=2`, `limits.cpu.priority=0` | Resumes paused boxes. |
| Full | `limits.cpu=4`, `limits.cpu.priority=10` | Resumes paused boxes. |

- `limits.cpu=N` is the real cap: the box can use at most N CPUs. `limits.cpu.allowance` is only a weight between Linux processes, so it does nothing against Windows or games; the tray removes it if it finds one. Memory stays at the profile's 4GiB in every mode.
- **Every tick** the tray reads each instance's config from `incus list --format json` and sets only the keys that differ, so a limit an agent changed is put back, with no extra calls when nothing changed. In My PC it pauses any running instance on every tick, which also catches an agent resuming one.
- **Gaming yield:** Steam `RunningAppID` != 0, or `SHQueryUserNotificationState` full-screen/busy, behaves like My PC until the game ends.
- It uses an admin client cert, which agents don't have. The mode is stored in the `agents` project config as `user.agent-box.mode`, so `box` can read it. A mode check in `box` is advisory: an agent can ignore it. The tray's per-tick enforcement is what makes games win.
- The tray also holds a Windows power request while any instance is running, so the PC doesn't sleep in the middle of a job.

### 3. Provisioning (`provision/`)
`bootstrap-ssh.ps1`, `install-pc.ps1` (WSL, distro import, `wsl --update`, `.wslconfig` with 16 GB / `autoMemoryReclaim=gradual` / `vmIdleTimeout=-1`, the scheduled task, the tray at logon), `setup-distro.sh` (kernel check, Incus + UI, preseed, project/profile/volume, Tailscale, certs, image).

## Deferred
Wake-on-LAN via homeserver (`wakeonlan`), automatic sleep, GPU (`/dev/dxg` passthrough for the RTX 3070), VMs (needs `/dev/kvm` in WSL), egress allowlists (see pixels), and running whole agent sessions remotely (look at Coder or coi then).

## References
code-on-incus (local only; profile and egress ideas), pixels (remote Incus backend, checkpoints, egress allowlists), Containarium and Coder (heavier control planes; not adopted). Incus docs: projects (limits are allocation caps; `restricted.*` keys), instance options (`limits.cpu` and `limits.memory` update live).
