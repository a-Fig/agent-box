# Testing agent-box

## On the laptop

Go isn't on PATH and the laptop starves when two Go runs overlap, so run Go through the lock
wrapper, one run at a time, and never with `-race`:

```sh
bash .scratch/golock.sh go test ./...
```

`.scratch/` is gitignored; the wrapper is a few lines (`mkdir` lock, Go from `~/sdk/go1.27.3/bin`).
Unit tests use fakes for Incus, git (sometimes real git in a temp repo), WSL and Windows, so they
need no PC.

## CI

`.github/workflows/test.yml` runs on every PR and every push to `dev` and `main`:

- **Linux:** `gofmt`, `go vet` (also with `GOOS=windows`), `go test -race`, `bash -n` and
  shellcheck on `setup-distro.sh`, and a YAML parse of `provision/*.yaml`.
- **Windows:** `go vet`, `go test`, a build of `boxtray.exe`, and a parse of `provision/*.ps1`.

## Live smoke test on the real PC

Changes to `box`, provisioning or the tray are tried against the real PC before they merge. This
is the checklist that passed on 2026-11-24 (Incus 7.6, all seven checks). Run it from Windows
PowerShell on the laptop, at the repository root, with the restricted remote `pc` set up and the
tray in Shared or Full. It creates disposable boxes and removes only the ones it creates.

### Build box and make a test box

```powershell
New-Item -ItemType Directory -Force '.scratch/bin' | Out-Null
$box = Join-Path (Resolve-Path .).Path '.scratch/bin/box.exe'
bash .scratch/golock.sh go build -o .scratch/bin/box.exe ./cmd/box
$repo = (Resolve-Path .).Path
incus --version
& $box ls
if ($LASTEXITCODE -ne 0) { throw 'box ls failed' }
$name = 'smoke-' + [Guid]::NewGuid().ToString('N').Substring(0, 12)
& $box new $name
if ($LASTEXITCODE -ne 0) { throw "Creating $name failed" }
```

`box ls` shows names, status and expiry. Leave other boxes alone; the project allows 10 (`limits.instances`).

### Run the repository tests and check the idle expiry

```powershell
function Assert-BoxExpiry([string]$Name, [double]$Hours) {
    $raw = incus --project agents config get "pc:$Name" user.expires
    if ($LASTEXITCODE -ne 0) { throw "Could not read expiry for $Name" }
    if ($raw -notmatch 'Z$') { throw "Expected a UTC expiry for $Name, got $raw" }
    $left = ([DateTimeOffset]::Parse($raw) - [DateTimeOffset]::UtcNow).TotalHours
    if ([Math]::Abs($left - $Hours) -gt (5.0 / 60.0)) {
        throw "Expected $Name to expire about ${Hours}h from now, got $raw"
    }
    Write-Host "$Name expires at $raw"
}

Assert-BoxExpiry $name 2
Push-Location -LiteralPath $repo
try {
    & $box run --box $name -- go test ./internal/...
    if ($LASTEXITCODE -ne 0) { throw 'Remote Go tests failed' }
} finally { Pop-Location }
Assert-BoxExpiry $name 2
```

### Detach with Ctrl-C and reattach

```powershell
Push-Location -LiteralPath $repo
& $box run --box $name -- 'echo smoke-job-started; sleep 75; echo smoke-job-finished; exit 7'
```

After `smoke-job-started` appears, press Ctrl-C once. Record the job ID printed by `box`. Keep the PowerShell session open and use its prompt to check the expiry and reattach.

```powershell
Assert-BoxExpiry $name 24
$job = '<job ID printed by box run>'
& $box job $job
if ($LASTEXITCODE -ne 7) { throw 'Reattached job did not return 7' }
Pop-Location
Assert-BoxExpiry $name 2
```

The reattached log should end with `smoke-job-finished`. The running expiry is about 24 hours and the idle expiry returns to about two hours after the exit code is read.

### Verify My PC mode and restricted access

Use the tray to select My PC and wait until the project reports `mypc`.

```powershell
incus --project agents project get pc:agents user.agent-box.mode
Push-Location -LiteralPath $repo
try {
    & $box run --box $name -- 'echo THIS_SHOULD_NOT_RUN'
    if ($LASTEXITCODE -ne 75) { throw 'Expected dry fallback exit 75 in My PC mode' }
} finally { Pop-Location }
```

The command should return `75` without printing `THIS_SHOULD_NOT_RUN` in its output. Restore the prior tray mode. With the restricted `pc` remote confirmed, check that Incus refuses the project update. The fallback restores the original limit if the check reveals an unexpectedly privileged client.

```powershell
$original = incus --project agents project get pc:agents limits.cpu
if ($LASTEXITCODE -ne 0 -or $original -ne '24') { throw 'Unexpected project CPU limit. Stop and verify the target.' }
$denial = incus --project agents project set pc:agents limits.cpu=99 2>&1
$code = $LASTEXITCODE
if ($code -eq 0) {
    incus --project agents project set pc:agents "limits.cpu=$original"
    if ($LASTEXITCODE -ne 0) { throw 'The client changed the limit and could not restore it' }
    throw 'The client could change the project CPU limit. The original limit was restored.'
}
if (($denial -join "`n") -notmatch '(?i)restricted|forbidden|not authorized|permission denied') {
    throw "Expected an authorization refusal. Got $denial"
}
& $box rm $name
if ($LASTEXITCODE -ne 0) { throw "Could not remove smoke box $name" }
```

### Verify automatic cleanup of a throwaway

```powershell
Push-Location -LiteralPath $repo
try {
    $log = & $box run --new -- 'echo throwaway-smoke-ok' 2>&1
    if ($LASTEXITCODE -ne 0) { throw ($log -join "`n") }
} finally { Pop-Location }
$text = $log -join "`n"
$jobLine = [regex]::Match($text, '(?m)^box: job \S+ on (?<box>\S+)')
if (-not $jobLine.Success -or $text -notmatch 'throwaway-smoke-ok') {
    throw "Missing throwaway output or box name`n$text"
}
$throwaway = $jobLine.Groups['box'].Value
$instances = incus --project agents list pc: --format json | ConvertFrom-Json
if ($LASTEXITCODE -ne 0) { throw 'Could not list instances after the run' }
if ($instances | Where-Object { $_.name -eq $throwaway }) { throw "Throwaway $throwaway still exists" }
```

If a previous step left the named smoke box behind, use `box ls` to confirm the printed name and remove only that box.

### Test status, job control, log pages, and artifact pulls

Run `box status` in the repo and again from a directory outside Git, such as the laptop's temp
directory. Both show mode, capacity and advice. The repo run also shows its named box or tells
you that it has yet to be created.

```powershell
& $box status
Push-Location -LiteralPath $env:TEMP
try { & $box status } finally { Pop-Location }
$name = 'toolsmoke-' + [Guid]::NewGuid().ToString('N').Substring(0, 10)
& $box new $name
if ($LASTEXITCODE -ne 0) { throw "Creating $name failed" }
& $box run --box $name --env SMOKE_LABEL=tools -- 'printf "%s\n" "$SMOKE_LABEL"'
if ($LASTEXITCODE -ne 0) { throw 'Environment smoke failed' }
& $box run --box $name --timeout 1s -- 'sleep 20'
if ($LASTEXITCODE -ne 124) { throw 'Expected exit 124 from the timeout' }
```

The environment check prints `tools`. To test forced termination, repeat the timeout with
`trap "" TERM; sleep 20` as the shell command. It returns `124` after the 10-second grace period.

Start a job that produces a log, leaves two artifacts and waits on a child.

```powershell
& $box run --box $name -- 'echo abcdefghijk; echo file-ok > smoke-file.txt; mkdir -p smoke-tree/sub; echo dir-ok > smoke-tree/sub/result.txt; sleep 180 & child=$!; echo "CHILD_PID=$child"; wait "$child"'
```

After `CHILD_PID` appears, record the job ID and PID and press Ctrl-C to detach.

```powershell
$job = '<printed job ID>'
$child = '<printed child PID>'
& $box jobs --box $name
& $box job $job --offset 2 --limit 4
& $box cancel $job
& $box job $job
if ($LASTEXITCODE -ne 130) { throw 'Cancelled job did not return 130' }
incus --project agents exec "pc:$name" -- ps -o stat= -p $child
$out = Join-Path (Resolve-Path .).Path ".scratch/smoke/$name"
& $box pull $job smoke-file.txt $out
& $box pull $job smoke-tree $out
Get-Content (Join-Path $out 'smoke-file.txt')
Get-Content (Join-Path $out 'smoke-tree/sub/result.txt')
& $box rm $name
```

The jobs list shows the job running before cancellation. The page prints `cdef` and reports
next offset `6`. The same job ID and offsets can be tested with MCP `box_job`, which returns
`nextOffset` in `structuredContent`. Cancellation should report exit `130`, and `ps` should
show no live child. The two pulls should print `file-ok` and `dir-ok`.

### Box sizes

Not yet run on the PC (2026-11-23); it needs `setup-distro.sh` re-run first, for the new caps.

```powershell
incus --project agents project get pc:agents limits.instances   # 10
incus --project agents project get pc:agents limits.cpu         # 24
$small = 'sizesmoke-' + [Guid]::NewGuid().ToString('N').Substring(0, 8)
& $box new $small --size small
if ($LASTEXITCODE -ne 0) { throw "Creating $small failed" }
incus --project agents config get "pc:$small" user.box.size     # small
incus --project agents config get "pc:$small" limits.memory     # 2GiB
incus --project agents config device get "pc:$small" root size  # 18GiB
Start-Sleep 10
incus --project agents config get "pc:$small" limits.cpu        # 1 in Shared, 2 in Full
& $box status   # "room for:" counts the small box. Shared (12GiB): 1 large, or 2 medium, or 5 small at most
& $box new --size large
& $box new --size large   # in Shared, with 2GiB + 8GiB used, the second large is refused: exit 75, "no room"
if ($LASTEXITCODE -ne 75) { throw 'Expected exit 75 for a launch over the memory cap' }
```

Remove the boxes it made with `box rm` (check `box ls` for the large one's name). Then, in a repo
whose box exists as medium, `box run --size small -- true` should say it uses a throwaway small
box, and leave the medium box as it was.

### Memory cap and reclaim

Needs the tray from the change under test. The cap follows the mode, and never drops below what
boxes hold:

```powershell
incus project get pc:agents limits.memory   # 24GiB in Full, 12GiB in Shared, My PC or a game
```

With a large and a small box (10GiB) plus a medium (4GiB), switching Full to Shared leaves it at
14GiB; removing the large one takes it to 12GiB within a tick.

Reclaim, on the PC: fill a medium box with about 3GiB that stays resident, then start a game (or
pick My PC), and watch Windows get the RAM back:

```powershell
"{0:N2} GB free, vmmem {1:N2} GB" -f ((Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory/1MB), ((Get-Process vmmemWSL).WorkingSet64/1GB)
wsl -d agents -u root -- cat /sys/fs/cgroup/lxc.payload.agents_<box>/memory.swap.current   # about 3GiB
wsl -d agents -u root -- cat /sys/fs/cgroup/lxc.payload.agents_<box>/memory.swap.max       # the box's limits.memory in bytes
```

On 2026-11-24 a 3.26GB box went to swap in 3.2s, and within 35s vmmem fell by 3.1 GB and Windows'
free memory rose by 3.2 GB. Over SSH the tray reads the notification state as busy and yields, so
`.scratch/traytest.ps1` exercises this path without a game. Set one box's `memory.swap.max` to 0 by hand: the tray
puts it back within a tick.

### GPU box

Not yet run on the PC (2026-11-24). It needs `setup-distro.sh` re-run first, for the GPU
restriction keys, and the tray rebuilt, so it stops GPU boxes. Start in Shared or Full, with no
game running.

```powershell
incus --project agents project get pc:agents restricted.devices.unix-char   # allow
incus --project agents project get pc:agents restricted.devices.disk.paths  # /usr/lib/wsl/lib,/usr/lib/wsl/drivers
& $box status   # gpu: free
# The CUDA driver API through ctypes: init, a context, a 1 GiB alloc and a copy round trip.
# A here-string without double quotes, because PowerShell 5.1 mangles nested ones.
$check = @'
nvidia-smi && python3 - <<PY
import ctypes
c = ctypes.CDLL('libcuda.so.1')
assert c.cuInit(0) == 0
n = ctypes.c_int()
assert c.cuDeviceGetCount(ctypes.byref(n)) == 0 and n.value >= 1
ctx, d = ctypes.c_void_p(), ctypes.c_uint64()
assert c.cuCtxCreate_v2(ctypes.byref(ctx), 0, 0) == 0
assert c.cuMemAlloc_v2(ctypes.byref(d), 1 << 30) == 0
src, dst = ctypes.create_string_buffer(b'agent-box-gpu-ok', 16), ctypes.create_string_buffer(16)
assert c.cuMemcpyHtoD_v2(d, src, 16) == 0 and c.cuMemcpyDtoH_v2(dst, d, 16) == 0
assert dst.raw == src.raw
print('cuda ok:', n.value, 'device(s), 1 GiB alloc and copy round trip')
PY
'@
Push-Location -LiteralPath $repo
try {
    & $box run --gpu -- $check
    if ($LASTEXITCODE -ne 0) { throw 'GPU box smoke failed' }
} finally { Pop-Location }
```

`nvidia-smi` lists the RTX 3070 and the check prints `cuda ok: 1 device(s), ...`; the throwaway
is gone afterwards (`box ls`). Then check one-at-a-time and the stop for games:

```powershell
Push-Location -LiteralPath $repo
& $box run --gpu -- 'nvidia-smi -l 5'   # press Ctrl-C after the first table; record the job ID
& $box status                           # gpu: in use: job <id>
& $box run --gpu -- true                # exit 75: the GPU box is in use (job <id>); run on CPU or wait
if ($LASTEXITCODE -ne 75) { throw 'Expected exit 75 for a second GPU box' }
Pop-Location
```

Start a game (or pick My PC in the tray). Within a tick the GPU box shows `STOPPED` in `box ls`,
and `nvidia-smi` on Windows no longer lists its process. Then:

```powershell
& $box job '<job ID>'   # exit 75: the GPU box was stopped because the PC's owner started a game
incus --project agents config get 'pc:<gpu box>' user.box.stopped   # gaming: ...
incus --project agents config get 'pc:<gpu box>' user.expires       # about 30 minutes from the stop
```

After the game ends and the tray is back in Shared, the box stays stopped, and the next
`box run --gpu` deletes it and launches a fresh one.
