# provision

Setup scripts for the gaming PC. Read `../docs/DESIGN.md` first. Nothing here touches disks, partitions or boot config; everything runs on top of Windows. Every script is safe to re-run.

## Order

1. **`bootstrap-ssh.ps1`** (Admin PowerShell on the PC, once). Gives the laptop SSH over the tailnet.
   ```powershell
   .\bootstrap-ssh.ps1 -PublicKey 'ssh-ed25519 AAAA... laptop'
   ```
   Do it from the PC's own console, or from a session that arrives over the tailnet. LAN SSH stops working afterwards.
2. **`install-pc.ps1`** (Admin PowerShell, copy this whole folder to the PC first).
   ```powershell
   $env:TS_AUTHKEY = 'tskey-auth-...'    # optional; or -TailscaleAuthKey
   .\install-pc.ps1 -AcceptVmp [-InstallDir D:\agent-box\wsl\agents] [-BoxtrayPath C:\...\boxtray.exe] [-BuildBase]
   ```
   If Windows features had to be enabled, it asks for a reboot; run it again afterwards.
3. **`setup-distro.sh`** runs by itself at the end of step 2 (inside the distro, as root). To run it again by hand: `wsl -d agents -u root --exec bash /opt/agent-box-provision/setup-distro.sh [--build-base]`.
4. Add client certs (commands are printed at the end of `setup-distro.sh`), then `incus remote add pc https://agent-box.<tailnet>.ts.net:8443 --token ...` on the laptop. Use the tailnet name, not an IP: Incus serves a Let's Encrypt certificate for that name (`https-cert.sh`, needs HTTPS certificates enabled in the tailnet's DNS settings), so the client trusts it without pinning and renewals need no client change.

## Updating boxd

`deploy-boxd.sh`, run from the laptop, builds boxd from the checkout and installs it with `systemd/agent-box-boxd.service` in one go, so the PC's binary and unit come from one commit, and stamps boxd with it (the dashboard's foot shows boxd's and the tray's commits):

```bash
provision/deploy-boxd.sh user@pc-host      # or AGENT_BOX_PC=user@pc-host; boxd is built on a box
```

It sends one tar over SSH and pipes it into the distro with a `.cmd` (the distro has no `/mnt/c`, and cmd keeps a binary stdin intact where PowerShell doesn't). In the distro, `install-boxd.sh` checks the SHA-256 sums and changes nothing on a mismatch. One install runs at a time. A running boxd that is healthy is kept as `boxd.old`, with its unit as `agent-box-boxd.service.old`; an unhealthy one isn't, so a retry never replaces the last good build. Then it restarts the service. After 8 seconds the service must be active, must not have restarted itself, must answer HTTP, and boxd must have logged no WARNING (such as a missing `INCUS_CONF`). If any of that fails, or the script is cut off after swapping files, it puts the old ones back, restarts them, and fails. It was written after the PC's unit was found older than this folder's (2026-11-25), and tried on the PC that day: a deploy, a checksum mismatch, and a rollback from a boxd that exits at once.

Released builds update themselves (docs/releasing.md). `agent-box-update.timer` runs `update-boxd.sh` hourly: it takes GitHub's latest stable release when it is newer than the installed boxd, checks the release's `SHA256SUMS`, refuses a boxd whose `-version` doesn't say the tag, and installs through that release's `install-boxd.sh`. It never moves to an older release, and leaves alone a boxd that isn't one, such as `deploy-boxd.sh`'s. `AGENT_BOX_NO_UPDATE=1` in `/etc/agent-box/update.env` turns it off. To put a boxd on releases, or install a prerelease or an older one by hand, run `agent-box-update-boxd <tag>` as root in the distro.

Files: `default-profile.yaml` (project `agents`'s `default` profile: limits, nesting, cache volume, cloud-init), `incus-preseed.yaml` (Incus init), `wsl.conf` (copied to `/etc/wsl.conf`).

## What changes on the PC, and how to undo it

| Script | Change | Undo |
|---|---|---|
| `bootstrap-ssh.ps1` | OpenSSH Server capability; `sshd` Automatic + running | `Stop-Service sshd; Set-Service sshd -StartupType Disabled; Remove-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0` |
| | line in `%ProgramData%\ssh\administrators_authorized_keys` | delete that line (or the file) |
| | firewall rule `agent-box SSH (tailnet only)`; stock rule `OpenSSH-Server-In-TCP` disabled | `Remove-NetFirewallRule -DisplayName 'agent-box SSH (tailnet only)'; Enable-NetFirewallRule -Name OpenSSH-Server-In-TCP` |
| | registry `HKLM:\SOFTWARE\OpenSSH\DefaultShell` | `Remove-ItemProperty HKLM:\SOFTWARE\OpenSSH -Name DefaultShell` |
| | `standby-timeout-ac` = 0 | `powercfg /change standby-timeout-ac 30` (or your old value) |
| `install-pc.ps1` | Windows features WSL and Virtual Machine Platform, only if they were off. VMP can cost a few percent in games and some anti-cheats object. | `Disable-WindowsOptionalFeature -Online -FeatureName VirtualMachinePlatform` (and `Microsoft-Windows-Subsystem-Linux`), then reboot. Do not do this while other WSL distros matter to you. |
| | `%USERPROFILE%\.wslconfig`: `[wsl2] memory, swap, vmIdleTimeout`, `[experimental] autoMemoryReclaim, sparseVhd`. Other keys are kept; first backup is `.wslconfig.agent-box.bak`. | restore the backup, or delete those five keys; `wsl --shutdown` |
| | WSL distro `agents` (one VHDX under `-InstallDir`, default `<biggest drive>\agent-box\wsl\agents`) | `wsl --unregister agents` (deletes the distro and all boxes), then delete the folder |
| | `/etc/wsl.conf` in the distro | goes with the distro |
| | Scheduled Task `agent-box-wsl-keepalive` | `Unregister-ScheduledTask agent-box-wsl-keepalive -Confirm:$false` |
| | Scheduled Task `agent-box-tray` (only with `-BoxtrayPath`) | `Unregister-ScheduledTask agent-box-tray -Confirm:$false` |
| | Scheduled Task `agent-box-enforcer` (only with `-BoxtrayPath`) | `Unregister-ScheduledTask agent-box-enforcer -Confirm:$false` |
| | `%TEMP%\agent-box-dl` (rootfs download) | delete it |
| `setup-distro.sh` | Zabbly apt repo + key, `incus`, `incus-ui-canonical`, Tailscale (all inside the distro) | `apt remove` them; `rm /etc/apt/sources.list.d/zabbly-incus-stable.sources` |
| | Incus: storage pool `default`, bridge `incusbr0`, HTTPS `:8443`, project `agents` (restricted, caps, `cache` volume), its `default` profile, image `agent-base` | `incus project delete agents` after removing its instances and volume; or just `wsl --unregister agents` |
| | Tailscale node `agent-box` | `tailscale logout` in the distro and remove the node in the admin console |
| | With `-BoxdPath`: `boxd` in `/opt/agent-box/bin`, `agent-box-boxd.service`, `/etc/agent-box/boxd.json` (owners: this node's tailnet login), and `tailscale serve` on 443 to `127.0.0.1:7766` | `tailscale serve --https=443 off`, `systemctl disable --now agent-box-boxd`, delete those files |
| | `/usr/local/sbin/agent-box-update-boxd` (`update-boxd.sh`), `agent-box-update.service` and `.timer` | `systemctl disable --now agent-box-update.timer`, then delete the three |
| `deploy-boxd.sh` | `boxd` and `boxd.old` in `/opt/agent-box/bin`; `agent-box-boxd.service` and its `.old` in `/etc/systemd/system`; `/run/agent-box-deploy.lock` | as for `-BoxdPath` above |

## Details

- **Secrets.** The Tailscale auth key travels only through the environment (`WSLENV`) and `tailscale up --authkey`. It's never written to disk by these scripts. Client-cert tokens are printed by Incus and expire.
- **Re-running** `install-pc.ps1` skips what exists: features, distro, `.wslconfig` keys, `/etc/wsl.conf`, tasks (re-registered with `-Force`). `setup-distro.sh` installs a `boxd` that `-BoxdPath` staged in this run, never over a newer installed one (by mtime), and deletes the staged copy once the step succeeds; `deploy-boxd.sh` updates boxd after setup. With any boxd installed, it (re)applies `boxd.json`, `tailscale serve` and a missing unit. It skips the preseed when the `default` pool exists and re-applies project caps (all but `limits.memory`, which the tray owns), the `restricted.*` keys and the profile every time. `features.profiles` and `features.images` can't change after a project exists: if `agents` has the wrong ones, the script stops and prints how to recreate the project.
- **Modes and limits.** Setup sets `limits.instances=10` (`AGENTS_MAX_BOXES`), `limits.cpu=24` (`AGENTS_CPU_CAP`) and `limits.disk=210GiB` (`AGENTS_DISK_CAP`), and `limits.memory=12GiB` (`AGENTS_MEM_CAP`) only on a new project. From then on the tray sets `limits.memory` by mode (24GiB in Full, 12GiB otherwise, never below what boxes hold), sets each box's `limits.cpu` from its size and the mode, and re-enforces both every tick. A mode check in `box` is advisory; the tray's per-tick enforcement is what holds the limits. Per-box `limits.memory` is set by size at launch (2/4/8GiB). The VM ceiling is installed RAM minus 6 GB (`-MemoryGB`, 26 on the PC) with 24GB of swap (`-SwapGB`), which the tray uses to hand frozen boxes' RAM back to Windows. Why these numbers: `../docs/DESIGN.md` (Box sizes, Memory).
- **Kernel.** Incus 7.x needs Linux 6.12+. `install-pc.ps1` runs `wsl --update`; `setup-distro.sh` fails if `uname -r` is older (then `wsl --shutdown` and re-run). Changing `.wslconfig` applies after `wsl --shutdown`, which stops every WSL distro.
- **Two client identities.** Agents (restricted cert) and the tray (admin) must be different certs, so give the tray its own `INCUS_CONF` directory.
- **VMs.** `setup-distro.sh` reports whether `/dev/kvm` exists. If not, `nestedVirtualization=true` under `[wsl2]` in `.wslconfig` is the usual fix on Windows 11.

## Boxes are compute, not storage

Boxes are throwaway. Nothing on one is kept: push work to git before the box goes.

- **Expiry.** `box` stamps every box with `user.expires` (RFC3339 UTC): 24h from now while a job runs, 2h after the job finishes (so an idle box lives 2h). A job whose exit box never reads (no push, no `box_job`) would keep its 24h, so boxd, which sees every job end, sets the same 2h (30 minutes for a throwaway, as box gives it) once nothing else runs there, in a write Incus refuses if the box changed since boxd read it, and only over the job's own 24h (#53). A throwaway's room can then be taken for a launch; `box job` gives its end from boxd's history once the box is gone (#90).
- **Reaper.** On every tick, in every mode (My PC too, since deleting frees RAM and disk), the tray runs `incus delete --force` on each instance in project `agents` whose `user.expires` is in the past. An instance with no `user.expires`, or one that isn't RFC3339, is skipped (the bad value is reported once). A failed delete is retried next tick.
- **Disk caps.** Root disk 18/30/40GiB per box by size, set by `box` at launch. `default-profile.yaml` gives other launches the 30GiB default. The 210GiB project cap includes the shared 30GiB cache volume. Rerunning setup applies the lower cache quota in place on Btrfs. If usage is already over 30GiB, free space before applying the quota so writes can continue. This WSL ext4 configuration lacks the project quotas needed by an Incus `dir` pool.
- **Only `/cache` survives**, and it is a cache: it can be wiped at any time (Go, npm, Gradle, uv and browser downloads rebuild it).
