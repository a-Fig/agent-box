<#
.SYNOPSIS
  Run once on the gaming PC in an elevated PowerShell: OpenSSH Server, trusted key, tailnet-only firewall.
.DESCRIPTION
  Changes: installs the OpenSSH.Server capability; sets sshd to Automatic and starts it; adds the key
  to %ProgramData%\ssh\administrators_authorized_keys (ACL: Administrators + SYSTEM only); disables the
  stock "OpenSSH-Server-In-TCP" firewall rule and adds one allowing TCP 22 only from the tailnet;
  makes PowerShell the default SSH shell; stops the PC sleeping on AC power.
  Touches no disks, partitions or boot config. Safe to re-run.
.PARAMETER PublicKey
  One line, e.g. 'ssh-ed25519 AAAA... laptop'.
.PARAMETER TailnetCidr
  Source range allowed to reach SSH. Default 100.64.0.0/10 (Tailscale CGNAT range).
.PARAMETER KeepDefaultRule
  Leave the stock any-source OpenSSH firewall rule enabled.
.PARAMETER Inventory
  Print a short hardware inventory at the end.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PublicKey,
    [string]$TailnetCidr = '100.64.0.0/10',
    [switch]$KeepDefaultRule,
    [switch]$Inventory
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2

function Step([string]$m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Note([string]$m) { Write-Host "    $m" }

$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated (Administrator) PowerShell.'
}

# --- validate the key ------------------------------------------------------
$PublicKey = $PublicKey.Trim()
if ($PublicKey -match "[\r\n]") { throw 'PublicKey must be a single line.' }
if ($PublicKey -notmatch '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)\s+[A-Za-z0-9+/=]+(\s+.*)?$') {
    throw 'PublicKey does not look like an OpenSSH public key (type, base64, optional comment).'
}
$keyBlob = ($PublicKey -split '\s+')[1]

Write-Host ''
Write-Host 'This script will:'
Write-Host '  - install the OpenSSH Server Windows capability (if missing)'
Write-Host '  - set sshd to start automatically and start it'
Write-Host '  - add your key to administrators_authorized_keys (if not already there)'
Write-Host "  - restrict inbound TCP 22 to $TailnetCidr (LAN access to SSH will stop working)"
Write-Host '  - set PowerShell as the default SSH shell'
Write-Host '  - stop the PC sleeping on AC power (powercfg standby-timeout-ac 0)'
Write-Host ''

# --- OpenSSH Server --------------------------------------------------------
Step 'OpenSSH Server capability'
$cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*' | Select-Object -First 1
if (-not $cap) { throw 'OpenSSH.Server capability not found on this Windows build.' }
if ($cap.State -eq 'Installed') {
    Note 'already installed'
} else {
    Note "installing $($cap.Name)"
    Add-WindowsCapability -Online -Name $cap.Name | Out-Null
}

Step 'sshd service'
Set-Service -Name sshd -StartupType Automatic
if ((Get-Service sshd).Status -ne 'Running') { Start-Service sshd }   # first start generates host keys and sshd_config
Note 'sshd is Automatic and running'

# --- authorized key --------------------------------------------------------
Step 'administrators_authorized_keys'
$ak = Join-Path $env:ProgramData 'ssh\administrators_authorized_keys'
$existing = @()
if (Test-Path -LiteralPath $ak) { $existing = @(Get-Content -LiteralPath $ak -Encoding ascii) }
$present = $false
foreach ($line in $existing) { if ($line -match '^\s*[^#\s]\S*\s+(\S+)' -and $Matches[1] -eq $keyBlob) { $present = $true } }
if ($present) {
    Note 'key already present'
} else {
    Add-Content -LiteralPath $ak -Value $PublicKey -Encoding ascii
    Note 'key added'
}
# SIDs, not names, so this works on any Windows language: S-1-5-32-544 Administrators, S-1-5-18 SYSTEM.
& icacls.exe $ak /inheritance:r /grant '*S-1-5-32-544:F' /grant '*S-1-5-18:F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw "icacls failed on $ak (exit $LASTEXITCODE)" }
Note 'ACL set: Administrators and SYSTEM only'

# --- firewall --------------------------------------------------------------
Step "Firewall: TCP 22 only from $TailnetCidr"
$ruleName = 'agent-box SSH (tailnet only)'
Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort 22 `
    -RemoteAddress $TailnetCidr -Profile Any | Out-Null
Note "rule '$ruleName' created"
$stock = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
if ($stock -and -not $KeepDefaultRule) {
    Disable-NetFirewallRule -Name 'OpenSSH-Server-In-TCP'
    Note "stock rule 'OpenSSH-Server-In-TCP' disabled (it allowed any source)"
} elseif ($stock) {
    Write-Warning 'Stock any-source SSH rule left enabled (-KeepDefaultRule): SSH is NOT tailnet-only.'
}

# --- default shell, power --------------------------------------------------
Step 'Default SSH shell: PowerShell'
if (-not (Test-Path 'HKLM:\SOFTWARE\OpenSSH')) { New-Item -Path 'HKLM:\SOFTWARE\OpenSSH' -Force | Out-Null }
New-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell `
    -Value "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" -PropertyType String -Force | Out-Null
Note 'set'

Step 'Power: never sleep on AC'
& powercfg.exe /change standby-timeout-ac 0
if ($LASTEXITCODE -ne 0) { throw "powercfg failed (exit $LASTEXITCODE)" }
Note 'standby-timeout-ac = 0'

if ($Inventory) {
    Step 'Inventory'
    "User: $env:USERNAME"
    (Get-CimInstance Win32_OperatingSystem).Caption
    (Get-CimInstance Win32_Processor).Name
    'RAM GB: ' + [math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB)
    Get-CimInstance Win32_VideoController | ForEach-Object Name
    Get-PhysicalDisk | Format-Table FriendlyName, MediaType, @{n = 'GB'; e = { [math]::Round($_.Size / 1GB) } }
}

Write-Host ''
Write-Host 'Done. Test from the laptop (over the tailnet): ssh <user>@<tailnet-name-or-100.x-address>' -ForegroundColor Green
