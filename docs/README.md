# Documentation

Start with the job in front of you.
The root [README](../README.md) carries the commands.
These pages explain the choices behind them and what to check when they do not work.

## Reader paths

| You need to | Read first | Then |
|---|---|---|
| Set up a host | [Root README: Quick start](../README.md#quick-start-a-linux-server) | [provision/README.md](../provision/README.md), then [DESIGN.md](DESIGN.md) for why the host is shaped that way |
| Give an agent the box tools | [agents/join.md](agents/join.md) | [Root README: Using `box`](../README.md#using-box) for the command surface |
| Run work and follow it | [Root README: Using `box`](../README.md#using-box) | [queue.md](queue.md) when a run waits, [DESIGN.md](DESIGN.md) for push, digest and box lifetime |
| Use the dashboard or modes | [ui.md](ui.md) | [DESIGN.md](DESIGN.md) for tray enforcement and memory |
| Work out why a run did not start | [Start with `box status`](#when-something-does-not-work) below | [queue.md](queue.md) for a waiting run, [ui.md](ui.md) when the PC cannot be reached, [testing.md](testing.md) for the live checks |
| Change or test agent-box | [testing.md](testing.md) | [releasing.md](releasing.md) before shipping, [pr-media.md](pr-media.md) for pictures in a pull request |
| Understand where this is going | [VISION.md](VISION.md) | [DESIGN.md](DESIGN.md) for what exists today |

## When something does not work

Run `box status` first.
Its first line is the short answer: whether offloading is worth trying, what room exists, and whether the host is paused or out of reach.

- **Refused: `state: refused`, or the CLI says nothing started.** The host is paused, full,
  missing a GPU, or unreachable; the message says which.
  Wait, pick a smaller size, or run locally.
  Do not use exit 75 alone as the signal: a paused job below also returns 75.
- **Queued.** The run has been handed off and will start when room or the mode allows.
  `box job <id>` follows it; `box cancel <id>` takes it off.
  See [queue.md](queue.md).
- **Paused.** The job started, then its box was frozen.
  It may resume when the owner switches back; `box job <id>` reattaches.
- **PC unreachable in `box app` or the browser.** The PC may be asleep, off, or its distro stopped.
  See [ui.md](ui.md#when-the-pc-cant-be-reached).
- **A test result looks wrong.** Boxes run commands as root.
  A reused repo box keeps its checkout and caches between runs; across boxes, only `/cache`
  is persistence shared between them.
  The root [README](../README.md#using-box) covers the root-user case and box lifetime.

For a change to `box`, provisioning or the tray, use the live smoke test in [testing.md](testing.md#live-smoke-test-on-the-real-pc) before it merges.

## Every page

| Page | Reader | Status |
|---|---|---|
| [VISION.md](VISION.md) | Anyone deciding what agent-box should become | Current direction, dated 2026-11-26 |
| [DESIGN.md](DESIGN.md) | Contributors changing architecture | Current design and measured history |
| [queue.md](queue.md) | Contributors and agents waiting on a run | Current behaviour and API |
| [ui.md](ui.md) | Contributors working on the dashboard or tray controls | Current contract, plus build history |
| [testing.md](testing.md) | Contributors checking a change | Current laptop, CI and live-PC paths |
| [releasing.md](releasing.md) | Whoever cuts or ships a release | Current release and update process |
| [pr-media.md](pr-media.md) | Pull-request authors | Current picture workflow |
| [agents/join.md](agents/join.md) | A coding agent setting itself up | Step-by-step agent instructions |
| [for-everyone.md](for-everyone.md) | Readers tracing the old install plan | Historical snapshot from before #149; not a setup guide |
| [ideas.md](ideas.md) | Contributors looking for scoped work | Research backlog; nothing here is promised |
| [prior-art.md](prior-art.md) | Contributors designing the fleet | Research notes, not current behaviour |
| [release-v0.1.0.md](release-v0.1.0.md) | Anyone auditing that release | Historical go/no-go record |

Keep each fact in the page that owns it.
Link to it from the reader path instead of copying it: copies age at different speeds.
