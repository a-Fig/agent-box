# Cloud overflow and the fleet controller

agent-box runs on machines you own first. Two optional cloud pieces sit beside them:

- **A Modal runner** (`kind: "modal"` in `hosts.json`): metered overflow compute for when every
  owned host is busy, asleep or in My PC. It runs `box run` jobs on [Modal](https://modal.com),
  speaking [`runner-v1`](runner-protocol.md).
- **A fleet controller** on Cloudflare Workers: a tiny cache of each host's last dashboard state,
  so `box status` can say what a host was doing when it stopped answering. It speaks
  [`controller-v1`](controller-protocol.md).

Neither is needed, and each is set up by one command: `box setup modal` and
`box setup cloudflare`. Both take `--dry-run`, which prints every step and the exact config
changes without running `modal` or `wrangler`, touching the network or writing files.

## What it costs

The aim is $0 a month for both. Prices and plan limits are as of 2026-11-27; check the providers'
pages before relying on them.

| | Free part | What needs a card |
| --- | --- | --- |
| Modal runner | Modal's Starter plan includes $30 of compute credit a month. box caps the runner at $25 a month by default, and it keeps no warm containers (`min_containers=0`), so an idle runner costs nothing. | Spending past the monthly credit. With a card on file, also set Modal's workspace spend limit (below). |
| Fleet controller | Workers Free: 100,000 requests a day and SQLite Durable Objects, with no D1, KV or R2. A host sends about 2,000 to 3,000 requests a day, so a handful of hosts fit. | Nothing. Past the daily limit Cloudflare refuses requests until the next day; it doesn't bill. |

Modal bills two ways, chosen by the host's `mode`:

- **`function`** (default): Modal Functions at the 1x rate. Files aren't kept after a run, so
  `box pull` is refused.
- **`sandbox`**: Modal Sandboxes, about 3x the CPU and memory rate (GPU is the same), which keep
  the job's files for `keepSec` seconds so `box pull` works.

Each cloud run prints a cost line, and `box status` shows the month's spend against the cap.

## Budget guardrails

Several limits stack, so one mistake doesn't become a bill:

- **`capUSD`** (default 25): the runner's monthly cap. Once spent plus running cost reaches it,
  the runner answers `402 budget` and `box run` moves on to your other hosts (exit 75 if none can
  take the run). `box status` shows "cap reached".
- **`warnAt`** (default 0.75): the fraction of the cap at which `box status` shows "warning".
- **`maxConcurrent`** (default 4): most runs at once; more get `409 busy` and go elsewhere.
- **`maxTimeoutSec`** (default 3600): the longest run allowed; asking for more is refused.
- **`priority: -1`**: `box setup modal` adds the runner below owned hosts (priority 0), so cloud
  runs happen only as overflow.
- **`box cloud pause | resume | stop [--host modal]`**: pause turns new runs away at once,
  resume undoes it, stop pauses and cancels every running job.
- **Modal's workspace spend limit**, in Modal's dashboard under Settings. This is the backstop
  box can't bypass: set it whenever a card is on file.

`box setup modal` sets `capUSD`, `maxConcurrent`, `maxTimeoutSec` and `mode` through the
runner's `PUT /v1/config`; running it again with new flags changes them.

## `box setup modal`

You need the `modal` CLI signed in (`pip install modal`, then `modal setup`) and a proxy-auth
token for the runner's endpoint (Modal dashboard: Settings, Proxy Auth Tokens). The token's ID
and secret become the `Modal-Key` and `Modal-Secret` headers on every request.

Save the token to a file, one value per line (ID, then secret), or as
`{"headers":{"Modal-Key":"…","Modal-Secret":"…"}}`, and pass the file. box never takes a secret
as a flag value, which would leave it in shell history and process lists:

```sh
box setup modal --dry-run --token-in modal-token.txt   # see the plan first
box setup modal --token-in modal-token.txt             # or --token-in - to read stdin
```

Flags: `--name modal`, `--cap 25`, `--max-concurrent 4`, `--max-timeout 1h`,
`--mode function|sandbox`, and `--endpoint URL` when `modal deploy` doesn't print the URL.

What it does:

1. Explains the costs above and checks `modal --version`.
2. Reads the token, then writes the runner's source (embedded in `box`, so no checkout is
   needed) to a temporary directory and runs `modal deploy` on it. The directory is removed
   afterwards.
3. Writes the token to `<config>/agent-box/<name>-token.json`, readable only by you (0600).
   `<config>` is `%APPDATA%` on Windows, `~/Library/Application Support` on macOS and
   `~/.config` on Linux.
4. Applies the budget with `PUT /v1/config`.
5. Adds the host to `hosts.json`, keeping every existing host and field. Without a
   `hosts.json`, box used only the default host (`BOX_REMOTE`, `pc`), so that host goes in first.
   Re-running updates the entry's endpoint, token file and mode and keeps its priority.
6. Reminds you to set Modal's workspace spend limit.

Then `box status` lists the runner, and `box run` sends work there when owned hosts can't take
it.

## `box setup cloudflare`

You need `wrangler` signed in to Cloudflare (`wrangler login`). Without `wrangler` installed, box
uses `npx --yes wrangler@4.135.0`, the version `controller/package.json` pins.

```sh
box setup cloudflare --dry-run
box setup cloudflare
```

What it does:

1. Explains the costs above and checks `wrangler whoami`.
2. Writes the worker's source (embedded in `box`) to a temporary directory and runs
   `wrangler deploy` there.
3. Makes two tokens of 32 random bytes each, `HOST_TOKEN` (hosts write state) and
   `CLIENT_TOKEN` (clients read it), and stores each with `wrangler secret put`, passing the
   token on stdin, never as an argument.
4. Writes `controller-client-token` and `controller-host-token` (0600) and `controller.json`
   (`url` and `token_file`, other keys kept) in `<config>/agent-box/`. Tokens are never printed.
5. Prints the `boxd.json` snippet for each host.

On each host (for the PC, inside its `agents` distro):

1. Copy `controller-host-token` to `/etc/agent-box/controller-host-token`, readable only by
   boxd's user (`chmod 600`).
2. Add the printed `"controller"` block to `/etc/agent-box/boxd.json`. Its `id` should be the
   host's name in `hosts.json` (`pc` without one); otherwise set `"controller_id"` on that host
   entry. Restart boxd.

From then on, when a host doesn't answer, `box status` shows its last state from the controller,
and makes no controller requests while every host answers. Running `box setup cloudflare` again
makes new tokens, so the host token has to be copied again.
