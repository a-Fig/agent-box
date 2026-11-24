# agent-box

Turns a Windows gaming PC on the tailnet into on-demand, disposable Linux boxes for coding agents, without touching Windows.

- **boxd** runs in WSL2 and manages Incus containers/VMs.
- **boxtray** sits in the Windows tray: one click picks *My PC*, *Shared* or *Full power*.
- **box** is the CLI and MCP server agents use: `box run --new -- go test ./...`.

See [docs/DESIGN.md](docs/DESIGN.md).
