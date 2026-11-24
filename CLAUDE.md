# agent-box

On-demand agent compute on Tyler's Windows gaming PC. Read `docs/DESIGN.md` first. Incus does the heavy lifting (API, auth, web UI, box lifecycle); this repo is glue.

## Rules

- **Never wipe, repartition or dual-boot the gaming PC.** Everything runs on top of Windows (WSL2 + Incus). Ask before anything that touches its disks or boot config.
- Reuse existing tools and open source before writing code; keep custom code thin.
- Go: standard library first; dependencies need a reason. `go vet ./...` and `go test ./...` must pass. Use `gofmt`.
- Go isn't on PATH on the laptop: prepend `~/sdk/go1.27.3/bin`. Run one Go build or test at a time on the laptop, never with `-race` (it starves the machine); CI runs the full suite.
- Code that needs Incus, WSL or Windows APIs sits behind an interface so the rest is testable with fakes.
- Branches: work on a feature branch, fold into `dev`; `main` changes only through a PR Tyler merges.

## Layout

`cmd/box` sync-and-run CLI + MCP · `cmd/boxtray` Windows tray (modes, gaming yield) · `provision/` setup scripts · `docs/`.

<!-- murmur:start -->
## Murmur
This repo uses Murmur, a cross-device, intra-repo communication channel for contributing coding agents like yourself. If you have not joined Murmur yet, you should.

- To join Murmur (ask your user first): https://raw.githubusercontent.com/a-Fig/murmur/main/docs/agents/join.md
- To use Murmur (no tools? in T3 Code, `mur who` and `mur send` work in your shell): https://raw.githubusercontent.com/a-Fig/murmur/main/docs/agents/use.md
<!-- murmur:end -->
