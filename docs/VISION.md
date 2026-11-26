# Vision

_Draft, 2026-11-26, from Tyler's description of the project. It records what the project is meant to
become; `DESIGN.md` records how today's version works._

## What it is

The best way to connect coding agents to compute.

Agents run in many places: on a laptop, on a PC, in a cloud sandbox. Compute exists in many forms: a
gaming PC, an old laptop in a drawer, a home server, a VPS, a cloud VM, a serverless container. Each
agent and each machine has its own setup, limits and quirks, but every pairing can be made to work.
This project is the bridge: an agent asks for work to run, and it runs on compute its owner chose to
give it, on the owner's terms.

## Why

- **Agents are cheap, and compute where they live is scarce.** One person can run several agents at
  once. Each wants the full test suite, `-race`, a build. A laptop starves under two of them
  (measured: Murmur's `-race` suite took 1,013s on the laptop and 420s on a large box, 2026-11-25).
  Cloud agent sandboxes are small and short-lived.
- **Most people already own idle compute.** A gaming PC sits idle most of the day. Old laptops and
  desktops sit in drawers. Rented machines are paid for by the hour whether or not they're busy.
- **Nothing joins the two simply.** Each pairing today is a project of its own: SSH keys, a VPN,
  syncing the worktree, getting the result back in a form an agent can act on, and making sure
  agents don't take over a machine someone is using.

## The two sides

**Agents**, by where they run:

| Where | What the bridge must handle | Status (2026-11-26) |
|---|---|---|
| A laptop (Claude Code, Codex, Cursor, Antigravity, through T3 or not) | MCP tools, a worktree snapshot, results pushed back | Works. Push needs T3; elsewhere the agent waits in `box_job` |
| The same machine as the compute | Nothing to sync; still the owner's limits | Not tried |
| A cloud sandbox (Claude Code on the web, Codex cloud, CI) | The sandbox's network is allowlisted and it can't join a tailnet, so the compute needs a public, authenticated endpoint and the sandbox a credential | Not supported. The hardest agent case |

**Compute**, by kind:

| Kind | How it's set up | Status (2026-11-26) |
|---|---|---|
| Windows PC in daily use | WSL2 distro + Incus, the tray picks how much agents get | Works, live since 2026-11-24 |
| Linux server or home server | `setup-host.sh` | Script and README quick start exist; no recorded run on a fresh machine found in `docs/` |
| Old laptop or PC, wiped | Install Linux, then the Linux server path | Works through that path; no guide yet |
| Old laptop or PC, still used | A VM or WSL on top of the OS, shared and spun down when the owner needs it | Windows only (WSL). No macOS or desktop-Linux path |
| Mac | Incus doesn't run on macOS; it would need a Linux VM (Lima, Colima) | Not supported |
| VPS or cloud VM (AWS, Hetzner, ...) | The Linux server path; costs money while up, so it should start on demand and stop when idle | Linux path works; no start/stop or cost awareness |
| Machines other people lend | The owner's terms, and isolation from the borrower | In use privately; not designed for |
| Serverless (Cloudflare) | A Worker runs JS/WASM in an isolate with no Linux shell, so it can't run `go test`. Cloudflare Sandbox, a Linux container, is the closer fit | A rented sandbox as a host is #192 ([prior-art.md](prior-art.md)) |

## What the bridge has to get right

These are the hard parts, whatever the agent and the compute are. Each is where today's version spent
its effort, and where a newcomer would have to start from nothing.

1. **Reach.** Getting from the agent to the machine across NAT, home networks and cloud sandboxes.
   Today: Tailscale.
2. **Trust.** Agent-run code on someone's machine must stay inside its box, and an agent's credential
   must do nothing else. Today: unprivileged Incus containers in a restricted project, restricted
   client certs.
3. **The machine stays its owner's.** A shared machine yields when its owner wants it back, without
   the owner thinking about it. Today: the tray's three modes, freezing boxes, swapping their memory
   out.
4. **Getting the work there.** The agent's worktree as it is, uncommitted files included, without
   touching the agent's git state. Today: a git bundle snapshot.
5. **Bringing back an answer.** Not a log tail, but what the agent needs to act: which tests failed and
   where, delivered without the agent sitting blocked. Today: the failure digest and T3 push.
6. **Starting warm.** Caches and toolchains already there. Today: the shared `/cache` volume and the
   `agent-base` image.
7. **Choosing where.** With more than one machine, picking the one that's free, fits and is cheapest.
   Today: one host. Several hosts is the fleet (#159, #161-#171; [prior-art.md](prior-art.md)).
8. **Setup is part of the product.** Every kind of compute in the table above should be a short,
   tested recipe, from "I have this machine" to "my agents use it", in about ten minutes.

## Dedicated or shared

A machine can be given to agents in two ways, and the owner picks:

- **Dedicated.** Wipe it, install Linux, and it is a server. Simplest and fastest; the old laptop's
  best second life.
- **Shared.** Keep the machine as it is and run the boxes in a VM (or WSL) on top. The owner decides
  how much agents get and when, and the boxes freeze or stop when the owner needs the machine. The
  gaming PC is this case.

Both should feel the same to an agent. Only the owner sees the difference.

## What stays true

These hold today and are meant to keep holding as the project grows.

- **One shape for agents.** "Run this command against my worktree and tell me what happened." An agent
  doesn't need to know whether it ran on a gaming PC or a cloud VM.
- **The owner is in charge.** Of how much, when, and who.
- **Boxes are compute, not storage.** Throwaway; only caches survive.
- **Thin glue over existing tools.** Incus, Tailscale, WSL and git do the heavy lifting. This project
  is what joins them (Tyler's rule).

## Not the goal, for now

Recorded as of 2026-11-26; each could change.

- **Running whole agent sessions on the compute.** The agent stays where it is and sends work out.
  Coder, Perry and others run the agent remotely; `DESIGN.md` lists it as deferred.
- **A marketplace or billing.** Lending compute to others is in scope; selling it is not
  (`ideas.md`, Appendix C).
- **A CI system.** Boxes serve agents mid-task, not pull requests.

## Similar projects, and what to steal

Read 2026-11-26 from each project's own README, docs or blog; none was tried, and no claim was checked
against source code. "Already in ideas.md" marks an idea that doc already has, now with a second
source. [prior-art.md](prior-art.md) covers the systems the fleet takes ideas from (Bazel remote
execution, Buildbarn, Cloudflare Sandbox, ...) and isn't repeated here.

**Crabbox** (openclaw; [README](https://github.com/openclaw/crabbox),
[sync](https://github.com/openclaw/crabbox/blob/main/docs/features/sync.md),
[security](https://github.com/openclaw/crabbox/blob/main/docs/security.md)). The closest match:
"warm a box, sync the diff, run the suite", for coding agents and heavy builds. Its machines can be
cloud VMs (AWS, Hetzner, ...), local containers or VMs (Incus among them), your own SSH hosts, or
rented sandboxes (E2B, Cloudflare). Sync is rsync over SSH of tracked and non-ignored untracked
files. Agents get a generated skill; no MCP server was found. Its trust model is "trusted operators
on a shared team". Nothing was found about a machine its owner is also using.

- *Send only the diff.* The runner fetches the base commit from origin, so only local changes cross
  the wire. A fingerprint of HEAD and the changed files skips a sync when nothing changed. (ideas.md
  asks whether our bundle is incremental.)
- *`.crabboxignore`*, with ordered `!` re-includes, for files a run doesn't need.
- *A mass-deletion guard* on sync.
- *Ready pools*: warm boxes that are borrowed, heartbeated and returned.
- *Many providers behind one command*: the shape our compute table points at.

**Offload** (Imbue; [blog](https://imbue.com/blog/offload),
[how it works](https://imbue.com/blog/offload-how-it-works),
[README](https://docs.rs/crate/offload/0.4.1/source/README.md)). A Rust CLI "your agents invoke
directly" that splits a test suite across many Modal sandboxes. Its backends are Modal, local
processes, or any provider you can script with shell commands for prepare, create, exec and destroy.

- *Discover locally, run remotely*: list the tests with the framework's own collect command, then
  feed batches to sandboxes from a queue, longest first, using timings from earlier runs. (Timing
  splitting is already in ideas.md.)
- *Flaky is its own result*: retries run in parallel and stop at the first pass, and a test that
  passed on retry exits 2, not 0. (Already in ideas.md.)
- *Logs by test ID*, "to avoid flooding agent context".
- *Snapshot a prepared setup, apply a small diff per run*: the per-repo warm image ideas.md §4
  describes.
- *An onboarding skill* that sets a repo up for the tool and flags what it changed.

**Perry** ([README](https://github.com/Gricha/perry)). Self-hosted Docker workspaces with Claude
Code, OpenCode and Codex, reached over Tailscale, SSH or a web UI. Install is one `curl | bash` and
`perry agent run`.

- *An explicit host-access switch* (`--no-host-access`), a cousin of our modes.
- *One tailnet name per workspace*, so a dev server in it is reachable directly (compare #190's
  preview URLs).

**omg.dev**, formerly lfg ([README](https://github.com/BennyKok/omg.dev)). Runs agent sessions on
your own computer or server and drives them from a phone, through Tailscale. Isolation isn't
described.

- *Linked devices*: an agent asks another machine to run an action, and the owner approves it on that
  machine.
- *`omg doctor`* writes a sanitized report for support. (`box doctor` is already in ideas.md.)

**Ferryx** ([README](https://raw.githubusercontent.com/Indosaram/ferryx/HEAD/README.md); its license
is source-available and limits commercial use, so take ideas, not code). A terminal for coding agents,
with remote Linux machines paired as hosts.

- *An outbound-only relay*, with a direct QUIC path when both ends can reach each other: a home PC
  behind NAT needs no port forwarding or VPN. A candidate for cloud agents, which can't join a
  tailnet.
- *Sequenced output*, so a client that reattaches replays only what it missed.

**NVIDIA OpenShell** ([remote GPU](https://perspectives.nvidia.com/nvidia-openshell/run-coding-agents-gpu-remote-machine/),
[self-hosted](https://perspectives.nvidia.com/nvidia-openshell/self-hosted-runtime-ai-coding-agents-no-third-party-cloud/);
the pages say they are AI-generated and may contain errors). Agent sandboxes in Docker, Apache 2.0.

- *Bootstrap over SSH*: `openshell gateway start --remote user@host` needs only Docker on the remote
  machine. A shape for our "I have this machine" recipes.
- *Landlock file rules and default-deny network* inside the sandbox, on top of the container.

**mattolson/agent-sandbox** ([README](https://github.com/mattolson/agent-sandbox)) and **littlebox**,
formerly gbrindisi/agentbox ([README](https://github.com/gbrindisi/agentbox)). Local Docker sandboxes
for running an agent, so isolation rather than compute.

- *Tokens never enter the box*: a proxy injects headers from secret files only it can read. (Already
  in ideas.md, and #192 in prior-art.md.)
- *Default-deny egress, checked at every container start*, and a blocked request says which domain
  was blocked.

**Coder Agents** ([InfoQ](https://infoq.com/news/2026/05/coder-agents-self-hosted-ai)). Runs agent
sessions on self-hosted Coder workspaces, started from chat, an API, CI or Slack. The article gives
little detail; there was nothing specific to take.

What none of these was found to do: use a machine its owner still uses every day, and leave that
owner in charge of it.

## Priorities

Tyler's calls, 2026-11-26:

- **Spare-PC owners first.** People with an idle gaming PC or an old laptop. Recipes and docs for
  their machines come before homelab or cloud-budget cases.
- **More compute before cloud agents.** Mac, wiped laptop and VPS recipes come before reaching
  cloud-sandbox agents.
- **Lending is a side effect.** It can happen, but the design stays single-owner.

## Open questions

- Does "connect agents to compute" stop at running commands, or grow into running agents there too?

## The name

"agent-box" stays for now (Tyler, 2026-11-26), and may change later. It describes today's version
(agents get boxes) more than the bridge, and at least six other projects use "agentbox" for agent
sandboxes (checked 2026-11-26). The CLI and MCP tools are `box` and could keep that under any name.
