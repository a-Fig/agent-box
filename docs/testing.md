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
$box = Join-Path (Resolve-Path .).Path '.scratchinox.exe'
bash .scratch/golock.sh go build -o .scratch/bin/box.exe ./cmd/box
$repo = (Resolve-Path .).Path
incus --version
& $box ls
if ($LASTEXITCODE -ne 0) { throw 'box ls failed' }
$name = 'smoke-' + [Guid]::NewGuid().ToString('N').Substring(0, 12)
& $box new $name
if ($LASTEXITCODE -ne 0) { throw "Creating $name failed" }
```

`box ls` shows names, status and expiry. Leave other boxes alone; the project allows 3.

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
if ($LASTEXITCODE -ne 0 -or $original -ne '12') { throw 'Unexpected project CPU limit. Stop and verify the target.' }
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
