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
| Dashboard | Incus web UI (`incus-ui-canonical`) at `https://agent-box.<tailnet>.ts.net:8443`, with a Let's Encrypt certificate from Tailscale (`provision/https-cert.sh`, renewed weekly) |
| Client on the laptop | Windows `incus` client (`winget install LinuxContainers.Incus`), `incus remote add pc` |
| Image | `images:ubuntu/24.04/cloud` + cloud-init in the `agents` profile (git, build-essential, Go, Node LTS, uv, JDK 17, gh), published once as `agent-base` |
| Caches | Custom storage volume `cache` mounted at `/cache` (Go, npm, Gradle, uv caches) |
| Keeping WSL up | Scheduled Task at boot: `wsl -d agents --exec sleep infinity` |

The `agents` project's `default` profile sets `security.nesting=true`, the cache volume, cloud-init and medium-size limits for boxes launched without `box`. The Linux kernel must be 6.12+ for Incus 7.x (`install-pc.ps1` runs `wsl --update`; `setup-distro.sh` checks).

## Isolation

Project `agents` has its own profiles (`features.profiles=true`): Incus refuses `restricted=true` on a project without them. Agents' certs can edit those profiles, but `restricted=true` still validates what goes in them, the project caps bound the totals, and the tray resets each box's own CPU limits every tick. `features.images=false` shares images. Neither can change after creation, so `setup-distro.sh` only checks an existing project and says how to recreate it if they are wrong. The project is `restricted=true` with `restricted.containers.privilege=unprivileged`, `restricted.containers.nesting=allow`, `restricted.devices.nic=managed` (`incusbr0`), and, for the GPU box, `restricted.devices.disk=allow` with `restricted.devices.disk.paths=/usr/lib/wsl/lib,/usr/lib/wsl/drivers` and `restricted.devices.unix-char=allow`. Project caps: `limits.instances=10`, `limits.cpu=24` and `limits.disk=210GiB`, set by `setup-distro.sh`, and `limits.memory`, which the tray sets by mode (see Memory). They are allocation caps, not usage caps. Incus refuses to lower one below what existing instances (stopped ones too) already hold.

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
- Incus counts configured allocations for all boxes, including stopped boxes. With Full's 24GiB of RAM the project fits ten small boxes (the instance cap; memory would allow twelve), six medium, or three large; with 12GiB, half that. The 24-CPU cap leaves room for those mixes in Full mode. Small boxes allocate the most disk per GiB of RAM, so ten small root disks plus the 30GiB cache (210GiB) is the most the caps allow, and `limits.disk` is 210GiB.
- The disk cap is above the btrfs pool's 200GiB on purpose (2026-11-24). Both are allocations, and real use was 3.5GiB: boxes are clones of `agent-base` and write little. What really bounds the disk is the free space on C:, where the distro's sparse VHDX lives (190GB free that day); a pool that grows past that fills C: first, so a bigger pool would buy nothing.
- `box status` works out what fits now from the caps minus the existing boxes' Full CPUs, memory and count, and the disk Incus reports in `incus project info`. A size fits if it fits as things are, or once reclaimable GPU boxes (below) are deleted; in that case the memory cap is the one the tray will set for the boxes left, max(the mode's budget, what they hold), not today's. A launch Incus refuses for project limits exits 75, like a paused PC.

### The GPU box

The PC's RTX 3070 (8GB of VRAM) is a size, `gpu`, so agents ask for it with `box run --gpu` (or `--size gpu`; MCP `gpu: true`). WSL2 exposes the GPU as `/dev/dxg` plus two host folders, and these three devices made `nvidia-smi` and the CUDA driver API work in an unprivileged box built from `agent-base` (proven by hand on 2026-11-24):

- `dxg`: `unix-char`, `path=/dev/dxg`, `mode=0666`
- `wsllib`: `disk`, `/usr/lib/wsl/lib` read-only (libcuda, libnvidia-ml and `nvidia-smi`)
- `wsldrivers`: `disk`, `/usr/lib/wsl/drivers` read-only (Windows' driver store; without it nvidia-smi can't reach the driver)

plus `environment.LD_LIBRARY_PATH=/usr/lib/wsl/lib` and an `environment.PATH` that ends in that folder, so `nvidia-smi` runs by name. `box` adds them at launch (the devices as YAML on `incus launch`'s stdin, since `-d` only overrides devices a profile has). The tables are `GPUDevices` and `GPUEnv` in `internal/mode/size.go`.

- **Throwaway only.** A GPU run always gets a throwaway box, never the repo's reused box, so an idle box doesn't hold the GPU. `box new --gpu` makes a named one for whoever asks; `box run --gpu --box <it>` uses it.
- **One at a time.** Before launching, `box` looks for another instance with `user.box.size=gpu` that is running or frozen. If one exists it exits 75 with `the GPU box is in use (job X); run on CPU or wait`. Throwaway GPU boxes that are done are deleted on the way, since they would hold 8GiB of the memory cap: stopped ones the tray marked, and ones more than 15 minutes old that are stopped or run no job. A younger box may be one another run is starting (Incus creates it stopped, and it runs no job until its PID file is written), so it counts as in use. Two launches racing past that check are caught right after launch: a run that finds another GPU box (running, paused, or still being created) deletes its own and exits 75, as does a run whose own box is gone, so both may back off, but the GPU never has two boxes.
- **Games win, and VRAM comes back.** A frozen CUDA process keeps its GPU memory, so pausing a GPU box would leave a game short of VRAM. In My PC, and whenever it yields to a game, the tray **stops** a GPU box (`incus stop --force`) instead of pausing it. First it writes why in `user.box.stopped` (`mypc`, or the yield reason like `gaming: steam:570`); once the stop worked it shortens `user.expires` to 30 minutes from now (not before, or a box whose stop failed and that later resumed would be reaped mid-job), so the reaper removes the dead box soon but `box job` can still explain it and `box pull` can fetch partial output. Until then it still holds a GPU box's memory and disk against the caps, so `box status` doesn't count it, and any launch the caps refuse deletes it and tries once more (found live on 2026-11-24, when a stopped GPU box left no room for a CPU box). If the stop fails it pauses the box and retries the stop next tick. A stopped GPU box stays stopped when the mode comes back. The job is lost: `box job` exits 75 with `the GPU box was stopped because the PC's owner started a game; rerun later or on CPU` (or `switched it to My PC`).
- `box status` has one `gpu` field and line: `free`, `free, but no room for a gpu box now`, `in use: job X`, or `unavailable: the PC is yielding to a game` / `in My PC mode`. The gpu size is left out of the `room for` advice.

## Glue we write

### 1. `box` (Go, one binary, laptop): CLI + stdio MCP server, shaped like Murmur
It shells out to the `incus` CLI, using the `pc` remote and the `agents` project. Each command is also an MCP tool with the same behaviour.

```
box status                       # box_status: advice line, reachability, mode + yield reason, free slots, room per size, this repo's box (and size) and running jobs
box run --reason "why" [--box name|--new] [--size S|--gpu] [--env K=V] [--timeout d] -- <cmd...>   # box_run
box job <id> [--offset N --limit N]   # box_job: follow a job, or page through its log
box jobs [--box name]            # box_jobs: jobs from /work/.box (*.pid, *.exit, *.log), newest first
box cancel <id>                  # box_cancel: kill the job's session (setsid), write .exit 130
box pull <id|box> <path> <dir>   # box_pull: incus file pull -r into a staging dir inside <dir>
box new [name] [--ttl 8h] [--size S|--gpu]  # box_new: incus launch agent-base pc:<name> --project agents, with user.expires and the size
box rm <name>                    # box_rm
box ls                           # box_ls
box mcp                          # serve the tools on stdio
box join [--leave]               # register box mcp with claude/codex (user scope) and install skills/box/SKILL.md
box ui [--url U]                 # the dashboard (docs/ui.md) in the browser: https://<host>/ of the pc remote
box app [--url U] [--wait]       # the same page in a window of its own (internal/desktop); for people, so not an MCP tool
```

`box status` reads `user.agent-box.mode` and `user.agent-box.reason` (set by the tray while it yields to a game) and the caps from the project, counts slots as `limits.instances` minus existing boxes, and says how many of each size fit (see Box sizes). `box join` uses the agents' own CLIs and never edits their config files. Env names that look like secrets are refused, since boxes are shared.

`box run`:
1. Snapshots the worktree as a git commit without touching the user's index: a temporary `GIT_INDEX_FILE`, add everything not ignored, then `write-tree` and `commit-tree`. It sends a `git bundle` with `incus file push` and checks the commit out in `/work/<repo>`.
2. Runs the command with `incus exec ... -- bash -lc`, under `nohup` with output going to `/work/.box/<job>.log`, so a dropped connection loses nothing. `box job <id>` reattaches with `tail -f`.
3. Exits with the remote exit code, or 75 when the PC is paused, so callers can fall back to running locally.

By default it reuses one box per repo, which keeps caches and `node_modules` warm. `--new` creates a throwaway box. `--size` sets the size of a box the run launches (medium by default); when the repo's box exists in another size, the run uses a throwaway of the requested size and says so.

`box app` is a window around the page `box ui` opens, as `mur app` is in Murmur: the system's web view (WebView2 on Windows) through [glaze](https://github.com/crgimenes/glaze), which loads it at run time with purego, so there is no cgo and `box` still cross-compiles. glaze (and purego under it) is box's one dependency beyond `golang.org/x/sys`. The reason is the same as Murmur's app: a native window needs the platform's web view, and glaze is the cgo-free way to it that Murmur already runs on Windows; it raised `go.mod` to Go 1.27.1. Linux builds get the window only with `-tags desktop`, because purego makes the binary link glibc dynamically; macOS has none yet. When the PC doesn't answer, the window shows the page's "PC unreachable" screen from the copy of `internal/dash/static` built into `box`, served on loopback, whose `/api/state` checks the PC each time the page retries (every 10s) and sends the window there once it answers.

### 2. Tray app (Windows): mode switch + gaming yield

| Mode | Per box | Project memory cap | Notes |
|---|---|---|---|
| My PC | paused, then pushed to swap; a GPU box is stopped (see The GPU box) | 12GiB | Boxes still frozen after 15 minutes are stopped to free RAM; 4 hours once a reclaim moved them to swap (Memory, below). A "Release memory" item runs `wsl --terminate agents`. |
| Shared | `limits.cpu` = the size's Shared CPUs (1/2/4), `limits.cpu.priority=0` | 12GiB | Resumes paused boxes. |
| Full | `limits.cpu` = the size's Full CPUs (2/4/8), `limits.cpu.priority=10` | 24GiB | Resumes paused boxes. |

- `limits.cpu=N` is the real cap: the box can use at most N CPUs. `limits.cpu.allowance` is only a weight between Linux processes, so it does nothing against Windows or games; the tray removes it if it finds one. A box's memory stays as it was launched in every mode; the project's memory cap follows the mode (Memory, below).
- **Every tick** the tray reads each instance's config from `incus list --format json` and sets only the keys that differ, so a limit an agent changed is put back, with no extra calls when nothing changed. In My PC it pauses any running instance on every tick, which also catches an agent resuming one.
- **Gaming yield:** Steam `RunningAppID` != 0, or `SHQueryUserNotificationState` full-screen/busy, behaves like My PC until the game ends.
- It uses an admin client cert, which agents don't have. The mode is stored in the `agents` project config as `user.agent-box.mode`, so `box` can read it. A mode check in `box` is advisory: an agent can ignore it. The tray's per-tick enforcement is what makes games win.
- On start the tray reads `user.agent-box.reason` once, so a reason left by a tray that died mid-yield (the PC crashing during a game) is cleared instead of keeping `box status` at "yielding" until the next game ends. Found live on 2026-11-24.
- The tray also holds a Windows power request while any instance is running, so the PC doesn't sleep in the middle of a job.
- **Logon and logging.** On 2026-11-23 the tray exited with code 1 at logon and nothing recorded why, so no game yield ran. Now it logs to `%LOCALAPPDATA%gent-boxoxtray.log` (start, every exit path, mode changes, yields; capped at 1 MB by dropping the older half). If adding the notification icon fails, the tray retries every 2 s for 2 min while the mode loop keeps running, and it adds the icon again on `TaskbarCreated` (Explorer restart). The `agent-box-tray` task restarts it up to 5 times, a minute apart, if it exits with an error.

#### Memory

The PC has 32 GB (Tyler, 2026-11-24: in Full, leave Windows about 6 GB). `.wslconfig` gives the WSL VM installed RAM minus 6 GB (26GB) and 24GB of swap. The distro itself (Incus, Tailscale, lxcfs) takes 1-2 GB, so boxes get 24GiB in Full. In Shared Tyler uses the PC, so boxes get the 12GiB they always had; My PC launches nothing, so it keeps 12GiB too. The budgets are `MemoryBudgets` in `internal/mode/size.go`.

- **The tray sets the cap.** Every tick it sets the project's `limits.memory` to the mode's budget, or to what existing boxes already hold if that is more (Incus refuses less), and only when the value differs. Going from Full to Shared with 20GiB of boxes leaves the cap at 20GiB; it tightens to 12GiB as those boxes go. Those boxes keep their RAM in Shared until then, which Tyler accepted for Full's sake. `box status` reads the cap, so its room follows the mode. `setup-distro.sh` sets `limits.memory` only on a new project, so a re-run doesn't fight the tray.
- **Games get frozen boxes' RAM back.** A frozen box keeps its memory resident in the VM, so with a 26GB VM a game could be left about 6 GB. After pausing a box for My PC or a game, the tray pushes its memory to swap with cgroup v2 `memory.reclaim`, and WSL's free page reporting hands the RAM back to Windows. Tyler switches to Shared when he sits down and to My PC before he plays (2026-11-24), so the game yield is the backstop and the manual My PC switch is the usual path. The code is `internal/mode/reclaim.go`.
  - **Every box may swap up to its own memory limit.** Incus writes `memory.swap.max=0` whenever it starts a container (seen on 2026-11-24: a restart reset it; `limits.cpu` changes, pause and resume didn't), and a restricted project refuses `limits.memory.swap` ("low-level config"). So on every tick, after pausing, GPU stops and thawing, the tray sets `memory.swap.max` on every running or frozen box to its `limits.memory` (own or from a profile), in one `wsl.exe` call that reports each box separately, writing only values that differ, as it does for `limits.cpu`. A box with no `limits.memory` is logged and left at 0. Reclaim is then a single `memory.reclaim` write with nothing to open or put back, so a reclaim that is cancelled, killed or raced leaves nothing to repair, and resuming a box depends on nothing but its CPU limits. The cost, accepted: a box that goes over its memory limit swaps (up to that limit again) before the OOM killer fires, so a runaway test gets slow before it fails.
  - The cgroup comes from the box's init PID (`/proc/<pid>/cgroup`) and must be exactly `lxc.payload.agents_<name>`. LXC adds a `-N` suffix when a stale directory is in the way, but `box` and `box-1` would then be ambiguous, so a suffixed cgroup is refused: its reclaim retries, and the 15-minute stop covers it. The script checks `frozen 1` in `cgroup.events` first. Reclaim works on a frozen cgroup, because the writing process does the work.
  - **Reclaim is best effort.** Each frozen box gets one `memory.reclaim` write per freeze and identity (name and init PID, so a recreated box starts fresh), in the background, all at once, bounded by `timeout 60` in the distro; every `wsl.exe` call the tray makes has a deadline. Only a write that never happened (WSL, PID or timeout failures) is retried, with backoff. What the box still holds afterwards is logged (`reclaimed <box>: <n>MiB left in RAM`) and picks how long it may stay frozen: 4 hours (`FrozenSwappedStop`) when this freeze's reclaim ran and left 256MiB or less, since a frozen box in swap costs Windows next to no RAM; 15 minutes otherwise (no reclaim yet, it left more, or there was no swap). The timer belongs to the instance (name and init PID), so a box recreated under the same name starts again at 15 minutes. Reclaim never stops a box itself; only that frozen limit does. A box is reclaimed only once this tick's swap batch has confirmed its `memory.swap.max` for that instance: Incus writes 0 when it starts a box, and on 2026-11-24 a box created while the tray was down was reclaimed before its swap was set, kept 2092MiB in RAM, and was stopped at 15 minutes. Waiting costs one tick (5 s). Ticks go on meanwhile, so pausing new boxes, that stop and the reaper aren't held up.
  - **Release memory** runs one `wsl --terminate` at a time (a second click while one runs is ignored). While it runs, the tick in progress is cancelled and waited for, queued and running reclaims are dropped and waited for, and a tick that starts calls no `wsl.exe -d agents`, which would start the distro again.
  - **Swap can run out; accepted.** A running box swaps only when it is over its own memory limit, which test runs rarely are, so running boxes filling the 24GB swap file is unlikely. If they do, a frozen box's reclaim is partial, it keeps the 15-minute limit, and that stop frees its RAM. No reservations or I/O limits.
  - Measured on 2026-11-24 with a box holding 3.26GB: it went to swap in 3.2s, and within 35s vmmem fell from 5.38 to 2.30 GB and Windows' free memory rose from 22.3 to 25.5 GB. The pages come back from swap when the box touches them after it resumes. GPU boxes are stopped, not frozen, so they aren't reclaimed.
- Swap is the default `swap.vhdx` in `%TEMP%` on C: (NVMe), grown only as it is used. 24GB holds every box's memory at once, unless running boxes over their limit are already using some.

### 3. Provisioning (`provision/`)
`bootstrap-ssh.ps1`, `install-pc.ps1` (WSL, distro import, `wsl --update`, `.wslconfig` with installed RAM minus 6 GB / 24GB swap / `autoMemoryReclaim=gradual` / `vmIdleTimeout=-1`, the scheduled task, the tray at logon), `setup-distro.sh` (kernel check, Incus + UI, preseed, project/profile/volume, Tailscale, certs, image).

## Deferred
Wake-on-LAN via homeserver (`wakeonlan`), automatic sleep, VMs (needs `/dev/kvm` in WSL), egress allowlists (see pixels), and running whole agent sessions remotely (look at Coder or coi then).

## References
code-on-incus (local only; profile and egress ideas), pixels (remote Incus backend, checkpoints, egress allowlists), Containarium and Coder (heavier control planes; not adopted). Incus docs: projects (limits are allocation caps; `restricted.*` keys), instance options (`limits.cpu` and `limits.memory` update live).

## Boxes are compute, not storage

Boxes are throwaway compute. Push work to git before a box goes.

- **Expiry.** `box` stamps every box with `user.expires` (RFC3339 UTC): 24h from now while a job runs, 2h after the job finishes (so an idle box lives 2h).
- **Reaper.** On every tick, in every mode (My PC too, since deleting frees RAM and disk), the tray runs `incus delete --force` on each instance in project `agents` whose `user.expires` is in the past. An instance with no `user.expires`, or one that isn't RFC3339, is skipped (the bad value is reported once). A failed delete is retried next tick.
- **Disk caps.** Root disk 18/30/40GiB per box by size, set at launch. Project `limits.disk=210GiB` over all boxes and volumes, including the 30GiB cache.
- **Only `/cache` survives**, and it is a cache: it can be wiped at any time (Go, npm, Gradle and uv rebuild it).
