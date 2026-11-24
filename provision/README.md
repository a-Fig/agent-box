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
| | `%TEMP%\agent-box-dl` (rootfs download) | delete it |
| `setup-distro.sh` | Zabbly apt repo + key, `incus`, `incus-ui-canonical`, Tailscale (all inside the distro) | `apt remove` them; `rm /etc/apt/sources.list.d/zabbly-incus-stable.sources` |
| | Incus: storage pool `default`, bridge `incusbr0`, HTTPS `:8443`, project `agents` (restricted, caps, `cache` volume), its `default` profile, image `agent-base` | `incus project delete agents` after removing its instances and volume; or just `wsl --unregister agents` |
| | Tailscale node `agent-box` | `tailscale logout` in the distro and remove the node in the admin console |

## Details

- **Secrets.** The Tailscale auth key travels only through the environment (`WSLENV`) and `tailscale up --authkey`. It's never written to disk by these scripts. Client-cert tokens are printed by Incus and expire.
- **Re-running** `install-pc.ps1` skips what exists: features, distro, `.wslconfig` keys, `/etc/wsl.conf`, tasks (re-registered with `-Force`). `setup-distro.sh` skips the preseed when the `default` pool exists and re-applies project caps (all but `limits.memory`, which the tray owns), the `restricted.*` keys and the profile every time. `features.profiles` and `features.images` can't change after a project exists: if `agents` has the wrong ones, the script stops and prints how to recreate the project.
- **Modes and limits.** Setup sets `limits.instances=10` (`AGENTS_MAX_BOXES`), `limits.cpu=24` (`AGENTS_CPU_CAP`) and `limits.disk=210GiB` (`AGENTS_DISK_CAP`), and `limits.memory=12GiB` (`AGENTS_MEM_CAP`) only on a new project. From then on the tray sets `limits.memory` by mode (24GiB in Full, 12GiB otherwise, never below what boxes hold), sets each box's `limits.cpu` from its size and the mode, and re-enforces both every tick. A mode check in `box` is advisory; the tray's per-tick enforcement is what makes games win. Per-box `limits.memory` is set by size at launch (2/4/8GiB). The VM ceiling is installed RAM minus 6 GB (`-MemoryGB`, 26 on the PC) with 24GB of swap (`-SwapGB`), which the tray uses to hand frozen boxes' RAM back to Windows. Why these numbers: `../docs/DESIGN.md` (Box sizes, Memory).
- **Kernel.** Incus 7.x needs Linux 6.12+. `install-pc.ps1` runs `wsl --update`; `setup-distro.sh` fails if `uname -r` is older (then `wsl --shutdown` and re-run). Changing `.wslconfig` applies after `wsl --shutdown`, which stops every WSL distro.
- **Two client identities.** Agents (restricted cert) and the tray (admin) must be different certs, so give the tray its own `INCUS_CONF` directory.
- **VMs.** `setup-distro.sh` reports whether `/dev/kvm` exists. If not, `nestedVirtualization=true` under `[wsl2]` in `.wslconfig` is the usual fix on Windows 11.

## Boxes are compute, not storage

Boxes are throwaway. Nothing on one is kept: push work to git before the box goes.

- **Expiry.** `box` stamps every box with `user.expires` (RFC3339 UTC): 24h from now while a job runs, 2h after the job finishes (so an idle box lives 2h).
- **Reaper.** On every tick, in every mode (My PC too, since deleting frees RAM and disk), the tray runs `incus delete --force` on each instance in project `agents` whose `user.expires` is in the past. An instance with no `user.expires`, or one that isn't RFC3339, is skipped (the bad value is reported once). A failed delete is retried next tick.
- **Disk caps.** Root disk 18/30/40GiB per box by size, set by `box` at launch. `default-profile.yaml` gives other launches the 30GiB default. The 210GiB project cap includes the shared 30GiB cache volume. Rerunning setup applies the lower cache quota in place on Btrfs. If usage is already over 30GiB, free space before applying the quota so writes can continue. This WSL ext4 configuration lacks the project quotas needed by an Incus `dir` pool.
- **Only `/cache` survives**, and it is a cache: it can be wiped at any time (Go, npm, Gradle and uv rebuild it).
