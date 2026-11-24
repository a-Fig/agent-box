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
| Isolation for agents | Incus project `agents` (`restricted=true`, its own profiles) + **restricted client certs** (`incus config trust add --restricted --projects agents`); see Isolation below |
| Dashboard | Incus web UI (`incus-ui-canonical`) at `https://agent-box:8443` |
| Client on the laptop | Windows `incus` client (`winget install LinuxContainers.Incus`), `incus remote add pc` |
| Image | `images:ubuntu/24.04/cloud` + cloud-init in the `agents` profile (git, build-essential, Go, Node LTS, uv, JDK 17, gh), published once as `agent-base` |
| Caches | Custom storage volume `cache` mounted at `/cache` (Go, npm, Gradle, uv caches) |
| Keeping WSL up | Scheduled Task at boot: `wsl -d agents --exec sleep infinity` |

The `agents` project's `default` profile sets `security.nesting=true`, the cache volume, cloud-init and medium-size limits for boxes launched without `box`. The Linux kernel must be 6.12+ for Incus 7.x (`install-pc.ps1` runs `wsl --update`; `setup-distro.sh` checks).

## Isolation

Project `agents` has its own profiles (`features.profiles=true`): Incus refuses `restricted=true` on a project without them. Agents' certs can edit those profiles, but `restricted=true` still validates what goes in them, the project caps bound the totals, and the tray resets each box's own CPU limits every tick. `features.images=false` shares images. Neither can change after creation, so `setup-distro.sh` only checks an existing project and says how to recreate it if they are wrong. The project is `restricted=true` with `restricted.containers.privilege=unprivileged`, `restricted.containers.nesting=allow`, `restricted.devices.nic=managed` (`incusbr0`), and, for the GPU box, `restricted.devices.disk=allow` with `restricted.devices.disk.paths=/usr/lib/wsl/lib,/usr/lib/wsl/drivers` and `restricted.devices.unix-char=allow`. Project caps are fixed at setup: `limits.instances=6`, `limits.cpu=24`, `limits.memory=12GiB`, `limits.disk=150GiB` (see Box sizes). They are allocation caps, not usage caps, and the tray never changes them, because Incus refuses to lower them below what existing instances (stopped ones too) already use.

What the GPU keys open up (checked in the Incus reference and source on 2026-11-24):

- **Disks.** Pool volumes (root, `cache`) pass as they did under `managed`. A host-path disk is allowed only if its `source` is inside one of the two WSL folders, matched on whole path components and opened with `openat2`, so a symlink can't lead outside them; `shift=true` is refused. No key enforces `readonly=true`, but boxes are unprivileged and those files belong to host root, so a box can read them and not write them. They are NVIDIA's user-space libraries (libcuda, libnvidia-ml, nvidia-smi) and the driver files Windows' driver store gives WSL: nothing about Tyler or his files.
- **Character devices.** Incus has no per-path key: `unix-char` is allow or block for every character device. So an agent's cert can attach `/dev/dxg` to any box, not only a GPU box, and can attach other host character devices too (`/dev/kmsg`, `/dev/net/tun`, ...). The box stays unprivileged, so devices that need host capabilities (`/dev/mem`, device-mapper control) still refuse it. Attaching `/dev/dxg` by hand bypasses the one-GPU-box rule and the tray's stop for games, because the tray finds GPU boxes by `user.box.size=gpu`. We accept that: agents use `box`, which never does it. A narrower setup, if it is ever needed, is a second project for the GPU box with `limits.instances=1`.

A restricted cert can still resume an instance and edit its own `limits.cpu`. So the modes are enforced by the tray's admin cert on every tick, not by permissions (see the tray below).

## Box sizes

The table lives in `internal/mode/size.go`; `box`, the tray and `box help` read it there.

| size | CPUs Shared / Full | RAM | root disk |
|---|---|---|---|
| small | 1 / 2 | 2GiB | 18GiB |
| medium (default) | 2 / 4 | 4GiB | 30GiB |
| large | 4 / 8 | 8GiB | 40GiB |
| gpu | 4 / 6 | 8GiB | 40GiB, plus the RTX 3070 |

- `box` launches every box with `user.box.size`, `limits.memory`, a root disk `size` and the Shared `limits.cpu` on the instance, so no extra profiles are needed. A box without the key (one launched before sizes, or by hand) is medium. Boxes are never resized: memory and disk are fixed at launch.
- The tray sets each box's `limits.cpu` from its size and the mode on every tick. An agent could rewrite `user.box.size` on its own box to get more CPUs; memory stays as launched, and the project caps still bound the total.
- Incus counts configured allocations for all boxes, including stopped boxes. With 12GiB of RAM, the project can fit six small boxes, three medium boxes, or one large box and two small boxes. The 24-CPU cap leaves room for those mixes in Full mode. Six small root disks and the 30GiB cache allocate 138GiB, leaving 12GiB for other allocations under the 150GiB disk cap.
- `box status` works out what fits now from the caps minus the existing boxes' Full CPUs, memory and count, and the disk Incus reports in `incus project info`. A launch Incus refuses for project limits exits 75, like a paused PC.

### The GPU box

The PC's RTX 3070 (8GB of VRAM) is a size, `gpu`, so agents ask for it with `box run --gpu` (or `--size gpu`; MCP `gpu: true`). WSL2 exposes the GPU as `/dev/dxg` plus two host folders, and these three devices made `nvidia-smi` and the CUDA driver API work in an unprivileged box built from `agent-base` (proven by hand on 2026-11-24):

- `dxg`: `unix-char`, `path=/dev/dxg`, `mode=0666`
- `wsllib`: `disk`, `/usr/lib/wsl/lib` read-only (libcuda, libnvidia-ml and `nvidia-smi`)
- `wsldrivers`: `disk`, `/usr/lib/wsl/drivers` read-only (Windows' driver store; without it nvidia-smi can't reach the driver)

plus `environment.LD_LIBRARY_PATH=/usr/lib/wsl/lib` and an `environment.PATH` that ends in that folder, so `nvidia-smi` runs by name. `box` adds them at launch (the devices as YAML on `incus launch`'s stdin, since `-d` only overrides devices a profile has). The tables are `GPUDevices` and `GPUEnv` in `internal/mode/size.go`.

- **Throwaway only.** A GPU run always gets a throwaway box, never the repo's reused box, so an idle box doesn't hold the GPU. `box new --gpu` makes a named one for whoever asks; `box run --gpu --box <it>` uses it.
- **One at a time.** Before launching, `box` looks for another instance with `user.box.size=gpu` that is running or frozen. If one exists it exits 75 with `the GPU box is in use (job X); run on CPU or wait`. Throwaway GPU boxes that are stopped or run no job are deleted on the way, since they would hold 8GiB of the 12GiB memory cap. Two launches racing past that check are caught right after launch: a run that finds another GPU box deletes its own and exits 75, so both may back off, but the GPU never has two boxes.
- **Games win, and VRAM comes back.** A frozen CUDA process keeps its GPU memory, so pausing a GPU box would leave a game short of VRAM. In My PC, and whenever it yields to a game, the tray **stops** a GPU box (`incus stop --force`) instead of pausing it. First it writes why in `user.box.stopped` (`mypc`, or the yield reason like `gaming: steam:570`) and shortens `user.expires` to 30 minutes from now, so the reaper removes the dead box soon but `box job` can still explain it and `box pull` can fetch partial output. Until then it still holds a GPU box's memory and disk against the caps, so `box status` doesn't count it, and any launch the caps refuse deletes it and tries once more (found live on 2026-11-24, when a stopped GPU box left no room for a CPU box). If the stop fails it pauses the box and retries the stop next tick. A stopped GPU box stays stopped when the mode comes back. The job is lost: `box job` exits 75 with `the GPU box was stopped because the PC's owner started a game; rerun later or on CPU` (or `switched it to My PC`).
- `box status` has one `gpu` field and line: `free`, `free, but no room for a gpu box now`, `in use: job X`, or `unavailable: the PC is yielding to a game` / `in My PC mode`. The gpu size is left out of the `room for` advice.

## Glue we write

### 1. `box` (Go, one binary, laptop): CLI + stdio MCP server, shaped like Murmur
It shells out to the `incus` CLI, using the `pc` remote and the `agents` project. Each command is also an MCP tool with the same behaviour.

```
box status                       # box_status: advice line, reachability, mode + yield reason, free slots, room per size, this repo's box (and size) and running jobs
box run [--box name|--new] [--size S|--gpu] [--env K=V] [--timeout d] -- <cmd...>   # box_run
box job <id> [--offset N --limit N]   # box_job: follow a job, or page through its log
box jobs [--box name]            # box_jobs: jobs from /work/.box (*.pid, *.exit, *.log), newest first
box cancel <id>                  # box_cancel: kill the job's session (setsid), write .exit 130
box pull <id|box> <path> <dir>   # box_pull: incus file pull -r into a staging dir inside <dir>
box new [name] [--ttl 8h] [--size S|--gpu]  # box_new: incus launch agent-base pc:<name> --project agents, with user.expires and the size
box rm <name>                    # box_rm
box ls                           # box_ls
box mcp                          # serve the tools on stdio
box join [--leave]               # register box mcp with claude/codex (user scope) and install skills/box/SKILL.md
```

`box status` reads `user.agent-box.mode` and `user.agent-box.reason` (set by the tray while it yields to a game) and the caps from the project, counts slots as `limits.instances` minus existing boxes, and says how many of each size fit (see Box sizes). `box join` uses the agents' own CLIs and never edits their config files. Env names that look like secrets are refused, since boxes are shared.

`box run`:
1. Snapshots the worktree as a git commit without touching the user's index: a temporary `GIT_INDEX_FILE`, add everything not ignored, then `write-tree` and `commit-tree`. It sends a `git bundle` with `incus file push` and checks the commit out in `/work/<repo>`.
2. Runs the command with `incus exec ... -- bash -lc`, under `nohup` with output going to `/work/.box/<job>.log`, so a dropped connection loses nothing. `box job <id>` reattaches with `tail -f`.
3. Exits with the remote exit code, or 75 when the PC is paused, so callers can fall back to running locally.

By default it reuses one box per repo, which keeps caches and `node_modules` warm. `--new` creates a throwaway box. `--size` sets the size of a box the run launches (medium by default); when the repo's box exists in another size, the run uses a throwaway of the requested size and says so.

### 2. Tray app (Windows): mode switch + gaming yield

| Mode | Per box | Notes |
|---|---|---|
| My PC | paused; a GPU box is stopped (see The GPU box) | Boxes still frozen after 15 minutes are stopped to free RAM. A "Release memory" item runs `wsl --terminate agents`. |
| Shared | `limits.cpu` = the size's Shared CPUs (1/2/4), `limits.cpu.priority=0` | Resumes paused boxes. |
| Full | `limits.cpu` = the size's Full CPUs (2/4/8), `limits.cpu.priority=10` | Resumes paused boxes. |

- `limits.cpu=N` is the real cap: the box can use at most N CPUs. `limits.cpu.allowance` is only a weight between Linux processes, so it does nothing against Windows or games; the tray removes it if it finds one. Memory stays as the box was launched in every mode.
- **Every tick** the tray reads each instance's config from `incus list --format json` and sets only the keys that differ, so a limit an agent changed is put back, with no extra calls when nothing changed. In My PC it pauses any running instance on every tick, which also catches an agent resuming one.
- **Gaming yield:** Steam `RunningAppID` != 0, or `SHQueryUserNotificationState` full-screen/busy, behaves like My PC until the game ends.
- It uses an admin client cert, which agents don't have. The mode is stored in the `agents` project config as `user.agent-box.mode`, so `box` can read it. A mode check in `box` is advisory: an agent can ignore it. The tray's per-tick enforcement is what makes games win.
- The tray also holds a Windows power request while any instance is running, so the PC doesn't sleep in the middle of a job.

### 3. Provisioning (`provision/`)
`bootstrap-ssh.ps1`, `install-pc.ps1` (WSL, distro import, `wsl --update`, `.wslconfig` with 16 GB / `autoMemoryReclaim=gradual` / `vmIdleTimeout=-1`, the scheduled task, the tray at logon), `setup-distro.sh` (kernel check, Incus + UI, preseed, project/profile/volume, Tailscale, certs, image).

## Deferred
Wake-on-LAN via homeserver (`wakeonlan`), automatic sleep, VMs (needs `/dev/kvm` in WSL), egress allowlists (see pixels), and running whole agent sessions remotely (look at Coder or coi then).

## References
code-on-incus (local only; profile and egress ideas), pixels (remote Incus backend, checkpoints, egress allowlists), Containarium and Coder (heavier control planes; not adopted). Incus docs: projects (limits are allocation caps; `restricted.*` keys), instance options (`limits.cpu` and `limits.memory` update live).

## Boxes are compute, not storage

Boxes are throwaway compute. Push work to git before a box goes.

- **Expiry.** `box` stamps every box with `user.expires` (RFC3339 UTC): 24h from now while a job runs, 2h after the job finishes (so an idle box lives 2h).
- **Reaper.** On every tick, in every mode (My PC too, since deleting frees RAM and disk), the tray runs `incus delete --force` on each instance in project `agents` whose `user.expires` is in the past. An instance with no `user.expires`, or one that isn't RFC3339, is skipped (the bad value is reported once). A failed delete is retried next tick.
- **Disk caps.** Root disk 18/30/40GiB per box by size, set at launch. Project `limits.disk=150GiB` over all boxes and volumes, including the 30GiB cache.
- **Only `/cache` survives**, and it is a cache: it can be wiped at any time (Go, npm, Gradle and uv rebuild it).
