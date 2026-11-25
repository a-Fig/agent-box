# Joining agent-box (for coding agents)

You are reading this because your user wants their coding agents to run heavy work (test suites,
`-race`, builds) on an agent-box host, a Linux machine running Incus that hands out disposable
boxes. This page is yours to follow, step by step.

## 1. Ask your user, and wait for the answer

Don't install or run anything yet. Ask your user whether to set up box on this machine, and say
what it changes:

- **A program, `box`.** It is downloaded from GitHub's latest stable release of a-Fig/agent-box,
  checked against the release's SHA-256 checksums, and put in `~/.local/bin` (on Windows,
  `%USERPROFILE%\.local\bin`, which is also added to the user PATH). No sudo or admin rights.
- **It updates itself.** While an agent runs `box mcp`, it checks GitHub hourly and installs a newer
  stable release, checksum checked; `box update --rollback` swaps the previous one back.
  `AGENT_BOX_NO_UPDATE=1` in the environment turns this off.
- **The agents get five tools.** `box join` registers `box mcp` as a user-scope MCP server, for every
  project, with each agent installed here (its CLI on PATH, or its config folder present): Claude
  Code and Codex through their CLIs, and Antigravity in its `mcp_config.json`. Agents that aren't
  installed are skipped. It copies the box skill into their skills folders, and adds `box hook` to
  Claude Code's `PostToolUse` hooks, which shows a busy agent its finished jobs.
- **Your worktree leaves this machine.** Each run copies the worktree, uncommitted and untracked
  files included (not ignored ones), to a box on the host. Whoever runs the host can read it there.
  Values passed with `--env` are visible in the box, so box refuses names that look like secrets.
- **The host must already be set up**, and this machine needs the `incus` client with a remote
  named `pc` that reaches it. The host's owner gives your user the line that adds it, with a token.
- **Undo:** `box join --leave` removes the servers, the skill and the hook. Then delete the binary.

If your user says no, or doesn't answer, leave it alone and carry on with what they asked for. Go on
only after a clear yes.

## 2. Check the remote

Run `incus remote list`. If `incus` isn't found, ask your user to install the client (Debian or
Ubuntu: `apt install incus-client`; macOS: `brew install incus`; Windows:
`winget install LinuxContainers.Incus`). If there is no remote named `pc`, ask your user for the
`incus remote add pc https://<host>:8443 --token <token>` line from the host's owner and run it.
Never guess a host or a token.

## 3. Install box, if this machine doesn't have it

Run `box --version`. If it prints a version, skip to step 4. Otherwise:

- Linux or macOS:

      curl -fsSL https://raw.githubusercontent.com/a-Fig/agent-box/main/install.sh | sh

- Windows PowerShell:

      irm https://raw.githubusercontent.com/a-Fig/agent-box/main/install.ps1 | iex

Your shell may not see the new PATH yet: use the full path the installer printed.

## 4. Join

    box join

It is safe to run twice. It ends by saying which agents it set up and which it skipped. If it found
none, it changed nothing and printed the server entry to add by hand, as below. Then run
`box status`: its first line says whether the host has room now. Tell your user what join printed, and that a session already running may not load the new
tools: in T3 Code, stop and send any message, or open a new thread; in a terminal, start a new
session (`claude --continue` keeps the conversation). Don't say the tools are there until they show
up in your list.

## Cursor and other agents

`box join` doesn't know Cursor. Add the server by hand in `~/.cursor/mcp.json`, with the full path
the installer printed:

```json
{
  "mcpServers": {
    "box": { "command": "/home/you/.local/bin/box", "args": ["mcp"] }
  }
}
```

Any agent that speaks MCP over stdio works the same way: the command is `box mcp`. Set `BOX_REMOTE`
in its `env` if the remote isn't named `pc`.

## Where results come back

A run that ends within 30 seconds returns its result inline. A longer one comes back like this:

| Agent | Long run's result |
|---|---|
| Claude Code or Codex in a T3 Code thread | pushed into the thread as a new turn; the agent can end its turn |
| Claude Code, Codex, Antigravity, Cursor, anything else | the agent calls `box_job`, which waits up to 10 minutes per call |
