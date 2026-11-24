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
4. Add client certs (commands are printed at the end of `setup-distro.sh`), then `incus remote add pc https://agent-box:8443 --token ...` on the laptop.

Files: `agents-profile.yaml` (project profile + cloud-init), `incus-preseed.yaml` (Incus init), `wsl.conf` (copied to `/etc/wsl.conf`).

## What changes on the PC, and how to undo it

| Script | Change | Undo |
|---|---|---|
| `bootstrap-ssh.ps1` | OpenSSH Server capability; `sshd` Automatic + running | `Stop-Service sshd; Set-Service sshd -StartupType Disabled; Remove-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0` |
| | line in `%ProgramData%\ssh\administrators_authorized_keys` | delete that line (or the file) |
| | firewall rule `agent-box SSH (tailnet only)`; stock rule `OpenSSH-Server-In-TCP` disabled | `Remove-NetFirewallRule -DisplayName 'agent-box SSH (tailnet only)'; Enable-NetFirewallRule -Name OpenSSH-Server-In-TCP` |
| | registry `HKLM:\SOFTWARE\OpenSSH\DefaultShell` | `Remove-ItemProperty HKLM:\SOFTWARE\OpenSSH -Name DefaultShell` |
| | `standby-timeout-ac` = 0 | `powercfg /change standby-timeout-ac 30` (or your old value) |
| `install-pc.ps1` | Windows features WSL and Virtual Machine Platform, only if they were off. VMP can cost a few percent in games and some anti-cheats object. | `Disable-WindowsOptionalFeature -Online -FeatureName VirtualMachinePlatform` (and `Microsoft-Windows-Subsystem-Linux`), then reboot. Do not do this while other WSL distros matter to you. |
| | `%USERPROFILE%\.wslconfig`: `[wsl2] memory, vmIdleTimeout`, `[experimental] autoMemoryReclaim, sparseVhd`. Other keys are kept; first backup is `.wslconfig.agent-box.bak`. | restore the backup, or delete those four keys; `wsl --shutdown` |
| | WSL distro `agents` (one VHDX under `-InstallDir`, default `<biggest drive>\agent-box\wsl\agents`) | `wsl --unregister agents` (deletes the distro and all boxes), then delete the folder |
| | `/etc/wsl.conf` in the distro | goes with the distro |
| | Scheduled Task `agent-box-wsl-keepalive` | `Unregister-ScheduledTask agent-box-wsl-keepalive -Confirm:$false` |
| | Scheduled Task `agent-box-tray` (only with `-BoxtrayPath`) | `Unregister-ScheduledTask agent-box-tray -Confirm:$false` |
| | `%TEMP%\agent-box-dl` (rootfs download) | delete it |
| `setup-distro.sh` | Zabbly apt repo + key, `incus`, `incus-ui-canonical`, Tailscale (all inside the distro) | `apt remove` them; `rm /etc/apt/sources.list.d/zabbly-incus-stable.sources` |
| | Incus: storage pool `default`, bridge `incusbr0`, HTTPS `:8443`, project `agents` (caps, profile, `cache` volume), image `agent-base` | `incus project delete agents` after removing its instances and volume; or just `wsl --unregister agents` |
| | Tailscale node `agent-box` | `tailscale logout` in the distro and remove the node in the admin console |

## Details

- **Secrets.** The Tailscale auth key travels only through the environment (`WSLENV`) and `tailscale up --authkey`. It's never written to disk by these scripts. Client-cert tokens are printed by Incus and expire.
- **Re-running** `install-pc.ps1` skips what exists: features, distro, `.wslconfig` keys, `/etc/wsl.conf`, tasks (re-registered with `-Force`). `setup-distro.sh` skips the preseed when the `default` pool exists and re-applies project caps and the profile every time.
- **Memory limits.** Per-box `limits.memory=4GiB`, project cap default `12GiB` (`AGENTS_MEM_CAP`), VM ceiling `16GB`. Changing `.wslconfig` applies after `wsl --shutdown`, which stops every WSL distro.
- **Two client identities.** Agents (restricted cert) and the tray (admin) must be different certs, so give the tray its own `INCUS_CONF` directory.
- **VMs.** `setup-distro.sh` reports whether `/dev/kvm` exists. If not, `nestedVirtualization=true` under `[wsl2]` in `.wslconfig` is the usual fix on Windows 11.
