# Setting up agent-box (for coding agents)

You are reading this because your user asked you to set up agent-box. This page tells you how to
guide them through either joining an existing host as a client or setting up a new host machine.

## 1. Ask your user first, and wait for the answer

Don't install or run anything yet. Ask your user whether they are:

1. **Connecting this machine as a client** to a host that is already set up (or that a teammate
   runs). If so, follow [Joining agent-box](https://agent-box.afig.dev/join) (also in
   `docs/agents/join.md`).
2. **Setting up a new host machine** (a spare Linux server or a Windows gaming PC) to run boxes.

If they want to set up a new host, explain what changes before running anything, and wait for a
clear yes.

## 2. Setting up a Linux host

Requirements: Ubuntu 24.04+ or Debian with Linux kernel 6.12 or newer (Ubuntu 24.04 needs
`sudo apt install linux-generic-hwe-24.04` and a reboot), at least 6 GiB RAM, and at least
75 GiB free disk under `/var/lib`.

What `provision/setup-host.sh` installs and configures (as root):

- Zabbly's Incus apt repository, `incus`, and `incus-ui-canonical`.
- A `default` btrfs storage pool (80% of free disk under `/var/lib`, capped at 200 GiB, min 60 GiB)
  and `incusbr0` managed bridge.
- A restricted Incus project `agents`, its `default` profile, a shared 30 GiB `/cache` volume, and
  the `agent-base` Ubuntu 24.04 image (Go, Node LTS, uv, JDK 17, gh, git, build-essential,
  Chromium libraries, lazy Android SDK hook).
- With `--tailscale`: joins Tailscale as `agent-box`, sets up weekly Let's Encrypt TLS certificates for `:8443`, and — when a Linux `boxd` binary is staged beside `setup-host.sh` (or already installed) — installs `boxd` (`agent-box-boxd.service`), `tailscale serve` on `:443`, and automatic hourly updates (`agent-box-update.timer`, disabled via `AGENT_BOX_NO_UPDATE=1` in `/etc/agent-box/update.env`). Without a `boxd` binary staged, the queue and `box ui` dashboard are skipped and updates stay inactive; after setup, push `boxd` from a client with `provision/deploy-boxd.sh <user@host>` (or stage `provision/boxd` before running `--tailscale`). See `provision/README.md`.

Run from a clone of `https://github.com/a-Fig/agent-box`:

    sudo agent-box/provision/setup-host.sh --build-base

Or with Tailscale TLS (and `boxd` when staged beside `setup-host.sh`, `TS_AUTHKEY` is optional if already logged in):

    TS_AUTHKEY=tskey-auth-... sudo --preserve-env=TS_AUTHKEY agent-box/provision/setup-host.sh --tailscale --build-base

To add the queue and `box ui` dashboard after a fresh Linux clone, deploy `boxd` over SSH (`provision/deploy-boxd.sh <user@host>`), or run `setup-host.sh --tailscale` with a Linux `boxd` binary staged beside it.

Setup takes about 10 minutes (mostly building `agent-base`) and ends by printing the single-use
`incus remote add` command for clients. Give that exact line to your user.

## 3. Setting up a Windows gaming PC host

Follow `provision/README.md` in `https://github.com/a-Fig/agent-box`:

- **Never wipe, repartition, or dual-boot Windows.** Everything runs in a WSL2 distro (`agents`).
- `provision/install-pc.ps1` (Admin PowerShell) enables WSL and Virtual Machine Platform if needed,
  imports the `agents` Ubuntu distro, configures `.wslconfig` (backing up to `.wslconfig.agent-box.bak`),
  creates keep-alive, logon tray (`boxtray`), and headless enforcer scheduled tasks, and runs
  `setup-host.sh --tailscale`.

## 4. Connecting clients and agents

Once a host is running and prints its `incus remote add` line, follow
[Joining agent-box](https://agent-box.afig.dev/join) on each laptop or workstation where coding
agents run: install the `incus` client and `box`, add the `pc` remote, and run `box join`.
