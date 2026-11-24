# agent-box

On-demand agent compute on Tyler's Windows gaming PC. Read `docs/DESIGN.md` first: it is the contract between components (API, modes, CLI, MCP tools).

## Rules

- **Never wipe, repartition or dual-boot the gaming PC.** Everything runs on top of Windows (WSL2 + Incus). Ask before anything that touches its disks or boot config.
- The API types in `internal/api` and the tables in `docs/DESIGN.md` must agree. Change both in the same commit.
- Go: standard library first; dependencies need a reason. `go vet ./...` and `go test ./...` must pass. Use `gofmt`.
- Go isn't on PATH on the laptop: prepend `~/sdk/go1.27.3/bin`. Run one Go build or test at a time on the laptop, never with `-race` (it starves the machine); CI runs the full suite.
- Code that needs Incus, WSL or Windows APIs sits behind an interface so the rest is testable with fakes.
- Branches: work on a feature branch, fold into `dev`; `main` changes only through a PR Tyler merges.

## Layout

`cmd/boxd` daemon (WSL) · `cmd/box` CLI + MCP server · `cmd/boxtray` Windows tray app · `internal/api` shared types · `internal/incus` Incus wrapper · `provision/` setup scripts · `docs/`.
