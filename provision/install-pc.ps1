<#
.SYNOPSIS
  Admin PowerShell on the gaming PC: WSL2 distro `agents`, .wslconfig, keep-alive task, Incus setup.
.DESCRIPTION
  Everything runs on top of Windows. No partitions, boot config or formatting are touched; the only
  large thing created is one WSL VHDX inside -InstallDir.
  Safe to re-run: existing pieces are detected and left alone.
.PARAMETER AcceptVmp
  Required only when the Virtual Machine Platform feature is not yet enabled (see the warning printed).
.PARAMETER InstallDir
  Where the distro's VHDX lives. Default: <drive with the most free space>\agent-box\wsl\agents.
.PARAMETER BoxtrayPath
  Path to boxtray.exe. If given, a per-user Scheduled Task starts it at logon.
.PARAMETER TailscaleAuthKey
  Auth key for `tailscale up`. Falls back to $env:TS_AUTHKEY. Passed to the distro via WSLENV; never written to disk.
.PARAMETER BuildBase
  Also build the `agent-base` image (takes several minutes).
.PARAMETER MemoryGB
  WSL VM memory ceiling written to .wslconfig. Default: installed RAM minus 6 GB for Windows (26 on the 32 GB PC).
.PARAMETER SwapGB
  WSL swap written to .wslconfig. Default 24, enough to hold every box's memory: the tray pushes frozen boxes
  out to swap while a game runs, so the RAM goes back to Windows.
#>
[CmdletBinding()]
param(
    [switch]$AcceptVmp,
    [string]$InstallDir,
    [string]$BoxtrayPath,
    [string]$TailscaleAuthKey,
    [switch]$BuildBase,
    [int]$MemoryGB = 0,
    [int]$SwapGB = 24
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2
$Distro = 'agents'
$RootfsUrl = 'https://cloud-images.ubuntu.com/wsl/releases/24.04/current/ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz'
$RootfsSums = 'https://cloud-images.ubuntu.com/wsl/releases/24.04/current/SHA256SUMS'
$StageDir = '/opt/agent-box-provision'
$KeepAliveTask = 'agent-box-wsl-keepalive'
$TrayTask = 'agent-box-tray'

function Step([string]$m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Note([string]$m) { Write-Host "    $m" }

# wsl.exe prints UTF-16; decode it properly.
function Invoke-Wsl {
    param([string[]]$WslArgs, [switch]$AllowFail)
    $old = [Console]::OutputEncoding
    $oldEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'   # PS 5.1 turns native stderr into errors under 'Stop'
    [Console]::OutputEncoding = [Text.Encoding]::Unicode
    try { $out = & wsl.exe @WslArgs 2>&1 | ForEach-Object { "$_" } } finally { [Console]::OutputEncoding = $old; $ErrorActionPreference = $oldEap }
    if ($LASTEXITCODE -ne 0 -and -not $AllowFail) { throw "wsl $($WslArgs -join ' ') failed (exit $LASTEXITCODE): $out" }
    return $out
}
# Commands inside the distro are plain UTF-8 text from Linux, not UTF-16.
function Invoke-InDistro {
    param([string[]]$CmdArgs, [switch]$AllowFail)
    $oldEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $out = & wsl.exe -d $Distro -u root --exec @CmdArgs 2>&1 | ForEach-Object { "$_" } } finally { $ErrorActionPreference = $oldEap }
    if ($LASTEXITCODE -ne 0 -and -not $AllowFail) { throw "in distro: $($CmdArgs -join ' ') failed (exit $LASTEXITCODE): $out" }
    return $out
}

$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated (Administrator) PowerShell.'
}
$ProvisionDir = $PSScriptRoot
foreach ($f in 'setup-distro.sh', 'incus-preseed.yaml', 'default-profile.yaml', 'wsl.conf', 'reaper.sh', 'systemd/agent-box-reaper.service', 'systemd/agent-box-reaper.timer', 'https-cert.sh', 'systemd/agent-box-https-cert.service', 'systemd/agent-box-https-cert.timer') {
    if (-not (Test-Path (Join-Path $ProvisionDir $f))) { throw "Missing $f next to install-pc.ps1." }
}
if ($BoxtrayPath -and -not (Test-Path -LiteralPath $BoxtrayPath)) { throw "BoxtrayPath not found: $BoxtrayPath" }
if (-not $TailscaleAuthKey) { $TailscaleAuthKey = $env:TS_AUTHKEY }

# --- 1. Windows features ---------------------------------------------------
Step 'Checking Windows features (WSL, Virtual Machine Platform)'
$wslFeat = Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Windows-Subsystem-Linux
$vmpFeat = Get-WindowsOptionalFeature -Online -FeatureName VirtualMachinePlatform
$needWsl = $wslFeat.State -ne 'Enabled'
$needVmp = $vmpFeat.State -ne 'Enabled'
if ($needWsl) { Note 'Windows Subsystem for Linux: not enabled' } else { Note 'Windows Subsystem for Linux: enabled' }
if ($needVmp) { Note 'Virtual Machine Platform: not enabled' } else { Note 'Virtual Machine Platform: enabled' }

if ($needVmp) {
    Write-Host ''
    Write-Host 'WARNING - about the Virtual Machine Platform' -ForegroundColor Yellow
    Write-Host '  WSL2 needs it. Turning it on makes Windows itself run on top of a small hypervisor.'
    Write-Host '  - Games can lose a few percent of performance (usually 1-5%, more in some CPU-bound titles).'
    Write-Host '  - Some anti-cheat systems object to a hypervisor being active (some refuse to start the game).'
    Write-Host '  - It is reversible: Disable-WindowsOptionalFeature -Online -FeatureName VirtualMachinePlatform,'
    Write-Host '    then reboot. Nothing about disks or boot setup changes.'
    Write-Host ''
    if (-not $AcceptVmp) {
        throw 'Not enabling Virtual Machine Platform without -AcceptVmp. Re-run with -AcceptVmp if you accept the above.'
    }
}
if ($needWsl -or $needVmp) {
    $restart = $false
    if ($needWsl) {
        Note 'enabling Microsoft-Windows-Subsystem-Linux'
        $r = Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Windows-Subsystem-Linux -NoRestart
        if ($r.RestartNeeded) { $restart = $true }
    }
    if ($needVmp) {
        Note 'enabling VirtualMachinePlatform'
        $r = Enable-WindowsOptionalFeature -Online -FeatureName VirtualMachinePlatform -NoRestart
        if ($r.RestartNeeded) { $restart = $true }
    }
    if ($restart) {
        Write-Host 'A reboot is required. Reboot Windows, then run install-pc.ps1 again.' -ForegroundColor Yellow
        exit 3
    }
}
if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) { throw 'wsl.exe not found.' }
Invoke-Wsl @('--set-default-version', '2') -AllowFail | Out-Null
# Incus 7.x needs Linux 6.12+; current WSL ships 6.18. A VM that is already running keeps its old kernel until wsl --shutdown.
Step 'Updating WSL (wsl --update)'
Invoke-Wsl @('--update') -AllowFail | Out-Null
Note 'done (setup-distro.sh checks that the kernel is 6.12 or newer)'

# --- 2. .wslconfig (merge) -------------------------------------------------
Step 'Merging %USERPROFILE%\.wslconfig'
if ($MemoryGB -le 0) {
    # Installed modules, not TotalPhysicalMemory: that is a little under (hardware reserved) and would round to 25.
    $ramGB = [int][math]::Floor((Get-CimInstance Win32_PhysicalMemory | Measure-Object -Property Capacity -Sum).Sum / 1GB)
    $MemoryGB = [math]::Max(4, $ramGB - 6)
    Note "memory: $MemoryGB GB of $ramGB GB (6 GB left for Windows)"
}
$wantCfg = [ordered]@{
    'wsl2'         = [ordered]@{ 'memory' = "${MemoryGB}GB"; 'swap' = "${SwapGB}GB"; 'vmIdleTimeout' = '-1' }
    'experimental' = [ordered]@{ 'autoMemoryReclaim' = 'gradual'; 'sparseVhd' = 'true' }
}
$cfgPath = Join-Path $env:USERPROFILE '.wslconfig'
$lines = New-Object System.Collections.Generic.List[string]
if (Test-Path -LiteralPath $cfgPath) { foreach ($l in (Get-Content -LiteralPath $cfgPath)) { $lines.Add([string]$l) } }
$original = ($lines -join "`n")
foreach ($section in $wantCfg.Keys) {
    $start = -1
    for ($i = 0; $i -lt $lines.Count; $i++) { if ($lines[$i].Trim() -ieq "[$section]") { $start = $i; break } }
    if ($start -lt 0) {
        if ($lines.Count -gt 0 -and $lines[$lines.Count - 1].Trim() -ne '') { $lines.Add('') }
        $lines.Add("[$section]")
        $start = $lines.Count - 1
    }
    $end = $lines.Count
    for ($i = $start + 1; $i -lt $lines.Count; $i++) { if ($lines[$i].Trim() -match '^\[.+\]$') { $end = $i; break } }
    foreach ($key in $wantCfg[$section].Keys) {
        $val = $wantCfg[$section][$key]
        $found = $false
        for ($i = $start + 1; $i -lt $end; $i++) {
            if ($lines[$i] -match '^\s*([A-Za-z0-9_.]+)\s*=') {
                if ($Matches[1] -ieq $key) { $lines[$i] = "$key=$val"; $found = $true; break }
            }
        }
        if (-not $found) {
            $at = $end   # before the blank lines that end the section
            while ($at -gt $start + 1 -and $lines[$at - 1].Trim() -eq '') { $at-- }
            $lines.Insert($at, "$key=$val"); $end++
        }
    }
}
$cfgChanged = ($lines -join "`n") -ne $original
if ($cfgChanged) {
    if (Test-Path -LiteralPath $cfgPath) {
        $bak = "$cfgPath.agent-box.bak"
        if (-not (Test-Path -LiteralPath $bak)) { Copy-Item -LiteralPath $cfgPath -Destination $bak; Note "backup: $bak" }
    }
    [IO.File]::WriteAllLines($cfgPath, $lines, (New-Object Text.UTF8Encoding($false)))
    Note "updated $cfgPath (other settings kept)"
} else {
    Note 'already up to date'
}

# --- 3. Import the distro --------------------------------------------------
Step "WSL distro '$Distro'"
$installed = @(Invoke-Wsl @('--list', '--quiet') -AllowFail | ForEach-Object { $_.Trim([char]0xFEFF, ' ', "`0") } | Where-Object { $_ })
if ($installed -contains $Distro) {
    Note 'already imported; skipping download and import'
} else {
    if (-not $InstallDir) {
        $best = Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3' | Sort-Object FreeSpace -Descending | Select-Object -First 1
        if (-not $best) { throw 'No fixed drive found; pass -InstallDir.' }
        $InstallDir = Join-Path ($best.DeviceID + '\') 'agent-box\wsl\agents'
        Note ("most free space: {0} ({1:N0} GB free)" -f $best.DeviceID, ($best.FreeSpace / 1GB))
    }
    if ((Test-Path -LiteralPath $InstallDir) -and (Get-ChildItem -LiteralPath $InstallDir -Force | Select-Object -First 1)) {
        throw "InstallDir '$InstallDir' is not empty. Pick an empty folder or delete leftovers."
    }
    $drive = (Split-Path -Qualifier $InstallDir)
    $free = (Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$drive'").FreeSpace
    if ($free -lt 30GB) { throw "Less than 30 GB free on $drive; refusing." }
    Write-Host "    Will create the distro's disk (a VHDX file) in: $InstallDir"

    $dl = Join-Path $env:TEMP 'agent-box-dl'
    New-Item -ItemType Directory -Force -Path $dl | Out-Null
    $tar = Join-Path $dl 'ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz'
    $ProgressPreference = 'SilentlyContinue'
    $sums = (Invoke-WebRequest -UseBasicParsing -Uri $RootfsSums).Content
    # PowerShell 5.1 returns bytes when the server doesn't say the file is text.
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::ASCII.GetString($sums) }
    $wantHash = $null
    foreach ($l in ($sums -split "`n")) { if ($l -match '^([0-9a-fA-F]{64})\s+\*?(\S+)') { if ($Matches[2] -eq 'ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz') { $wantHash = $Matches[1] } } }
    if (-not $wantHash) { throw "Could not find the rootfs checksum in $RootfsSums" }
    $have = $null
    if (Test-Path -LiteralPath $tar) { $have = (Get-FileHash -Algorithm SHA256 -LiteralPath $tar).Hash }
    if ($have -ine $wantHash) {
        Note "downloading $RootfsUrl"
        Invoke-WebRequest -UseBasicParsing -Uri $RootfsUrl -OutFile $tar
        $have = (Get-FileHash -Algorithm SHA256 -LiteralPath $tar).Hash
        if ($have -ine $wantHash) { Remove-Item -LiteralPath $tar -Force; throw 'Rootfs checksum mismatch; deleted the download.' }
    }
    Note 'checksum OK'
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Invoke-Wsl @('--import', $Distro, $InstallDir, $tar, '--version', '2') | Out-Null
    Note "imported as '$Distro'"
    # Sparse VHDX so freed space returns to Windows. Best effort: older/newer wsl.exe builds differ on this flag.
    Invoke-Wsl @('--manage', $Distro, '--set-sparse', 'true') -AllowFail | Out-Null
}

# --- 4. Copy provision files into the distro (tar over a pipe; works with automount off) ---
Step "Copying provision files to $StageDir in the distro"
Invoke-InDistro @('sh', '-c', "rm -rf $StageDir && mkdir -p $StageDir") | Out-Null
$tarExe = Join-Path $env:SystemRoot 'System32\tar.exe'
if (-not (Test-Path $tarExe)) { throw 'tar.exe not found (needs Windows 10 1803+).' }
$cmd = "`"$tarExe`" -cf - -C `"$ProvisionDir`" setup-distro.sh incus-preseed.yaml default-profile.yaml wsl.conf reaper.sh https-cert.sh systemd | wsl.exe -d $Distro -u root --exec tar -xf - -C $StageDir"
& cmd.exe /c $cmd
if ($LASTEXITCODE -ne 0) { throw 'Copying provision files into the distro failed.' }
Invoke-InDistro @('sh', '-c', "sed -i 's/\r$//' $StageDir/*.sh $StageDir/*.yaml $StageDir/wsl.conf && chmod +x $StageDir/setup-distro.sh") | Out-Null
Note 'copied'

# --- 5. /etc/wsl.conf ------------------------------------------------------
Step '/etc/wsl.conf (systemd on, automount off, interop off)'
$same = Invoke-InDistro @('sh', '-c', "cmp -s $StageDir/wsl.conf /etc/wsl.conf && echo same || echo differs")
if (($same | Select-Object -Last 1) -eq 'same') {
    Note 'already in place'
} else {
    Invoke-InDistro @('cp', "$StageDir/wsl.conf", '/etc/wsl.conf') | Out-Null
    Note 'written; restarting the distro so systemd starts'
    Invoke-Wsl @('--terminate', $Distro) | Out-Null
}
if ($cfgChanged) {
    $running = @(Invoke-Wsl @('--list', '--running', '--quiet') -AllowFail | ForEach-Object { $_.Trim([char]0xFEFF, ' ', "`0") } | Where-Object { $_ -and $_ -notmatch 'no running' })
    if ($running.Count -gt 0) {
        Write-Warning ".wslconfig changed while WSL is running ($($running -join ', ')). The new settings apply after 'wsl --shutdown' (this stops ALL WSL distros). Not doing that automatically."
    }
}

Step 'Waiting for systemd in the distro'
$state = ''
for ($i = 0; $i -lt 60; $i++) {
    $state = (Invoke-InDistro @('systemctl', 'is-system-running') -AllowFail | Select-Object -Last 1)
    if ($state -in 'running', 'degraded') { break }
    Start-Sleep -Seconds 2
}
if ($state -notin 'running', 'degraded') { throw "systemd did not come up (state '$state'). Check /etc/wsl.conf and wsl --version." }
Note "systemd: $state"

# --- 6. Keep-alive scheduled task -----------------------------------------
Step "Scheduled Task '$KeepAliveTask' (keeps the distro alive)"
# Not $env:USERDOMAIN: over SSH it reads WORKGROUP, which Task Scheduler can't resolve.
$me = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$wslExe = Join-Path $env:SystemRoot 'System32\wsl.exe'
$action = New-ScheduledTaskAction -Execute $wslExe -Argument "-d $Distro --exec sleep infinity"
$trigs = @((New-ScheduledTaskTrigger -AtStartup), (New-ScheduledTaskTrigger -AtLogOn -User $me))
$princ = New-ScheduledTaskPrincipal -UserId $me -LogonType S4U -RunLevel Limited
$sett = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName $KeepAliveTask -Action $action -Trigger $trigs -Principal $princ -Settings $sett -Force `
    -Description 'agent-box: keep the WSL distro running so Incus boxes stay up' | Out-Null
Note "registered for $me (at startup and at logon)"
Start-ScheduledTask -TaskName $KeepAliveTask -ErrorAction SilentlyContinue

# --- 7. Tray at logon ------------------------------------------------------
if ($BoxtrayPath) {
    Step "Scheduled Task '$TrayTask' (boxtray at logon)"
    $full = (Resolve-Path -LiteralPath $BoxtrayPath).Path
    $tAction = New-ScheduledTaskAction -Execute $full -WorkingDirectory (Split-Path $full)
    $tTrig = New-ScheduledTaskTrigger -AtLogOn -User $me
    $tPrinc = New-ScheduledTaskPrincipal -UserId $me -LogonType Interactive -RunLevel Limited
    $tSett = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) `
        -MultipleInstances IgnoreNew -RestartCount 5 -RestartInterval (New-TimeSpan -Minutes 1)   # restart if boxtray exits with an error
    Register-ScheduledTask -TaskName $TrayTask -Action $tAction -Trigger $tTrig -Principal $tPrinc -Settings $tSett -Force `
        -Description 'agent-box tray' | Out-Null
    Note "registered: $full"
} else {
    Step 'boxtray'
    Note 'no -BoxtrayPath given; not registering the tray'
}

# --- 8. Run setup-distro.sh ------------------------------------------------
Step 'Running setup-distro.sh inside the distro (Incus, project, profile, Tailscale)'
$shArgs = @()
if ($BuildBase) { $shArgs += '--build-base' }
$oldWslenv = $env:WSLENV
try {
    if ($TailscaleAuthKey) {
        $env:TS_AUTHKEY = $TailscaleAuthKey
        $env:WSLENV = if ($oldWslenv) { "$oldWslenv`:TS_AUTHKEY" } else { 'TS_AUTHKEY' }
        Note 'Tailscale auth key will be passed through the environment (not stored)'
    } else {
        Note 'no auth key: Tailscale will be installed but not joined'
    }
    & wsl.exe -d $Distro -u root --exec bash "$StageDir/setup-distro.sh" @shArgs
    if ($LASTEXITCODE -ne 0) { throw "setup-distro.sh failed (exit $LASTEXITCODE). Fix the cause and re-run install-pc.ps1." }
} finally {
    $env:WSLENV = $oldWslenv
    if ($TailscaleAuthKey -and $env:TS_AUTHKEY -eq $TailscaleAuthKey) { Remove-Item Env:\TS_AUTHKEY -ErrorAction SilentlyContinue }
}

Write-Host ''
Write-Host 'Done. Next: add client certs (commands printed above) and build the image if you skipped it.' -ForegroundColor Green
