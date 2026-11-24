<#
.SYNOPSIS
  Tests install-pc.ps1's .wslconfig merge on sample files. Run with Windows PowerShell 5.1 (CI does).
  Exits 1 if any case fails.
#>
$ErrorActionPreference = 'Stop'
$src = Get-Content -Raw (Join-Path $PSScriptRoot 'install-pc.ps1')
$i = $src.IndexOf('# >>> wslconfig merge'); $j = $src.IndexOf('# <<< wslconfig merge')
if ($i -lt 0 -or $j -lt $i) { throw 'wslconfig merge markers not found in install-pc.ps1' }
$block = [scriptblock]::Create($src.Substring($i, $j - $i))
function Note([string]$m) { }
$MemoryGB = 26; $SwapGB = 24
$wantCfg = [ordered]@{
    'wsl2'         = [ordered]@{ 'memory' = "${MemoryGB}GB"; 'swap' = "${SwapGB}GB"; 'vmIdleTimeout' = '-1' }
    'experimental' = [ordered]@{ 'autoMemoryReclaim' = 'gradual'; 'sparseVhd' = 'true' }
}
$u8 = New-Object Text.UTF8Encoding($false)
$e = [char]0xE9
# name = input bytes, expected output bytes ($null: must be refused and left as it was)
$cases = [ordered]@{
    'utf8 LF, no final newline' = @($u8.GetBytes("[wsl2]`nmemory=16GB`n# caf$e`n`n[experimental]`nsparseVhd=true"),
        $u8.GetBytes("[wsl2]`nmemory=26GB`n# caf$e`nswap=24GB`nvmIdleTimeout=-1`n`n[experimental]`nsparseVhd=true`nautoMemoryReclaim=gradual"))
    'utf8 BOM CRLF'             = @(([byte[]](0xEF, 0xBB, 0xBF) + $u8.GetBytes("[wsl2]`r`nmemory=16GB`r`n")),
        ([byte[]](0xEF, 0xBB, 0xBF) + $u8.GetBytes("[wsl2]`r`nmemory=26GB`r`nswap=24GB`r`nvmIdleTimeout=-1`r`n`r`n[experimental]`r`nautoMemoryReclaim=gradual`r`nsparseVhd=true`r`n")))
    'mixed CRLF and LF'         = @($u8.GetBytes("[wsl2]`r`nmemory=16GB`n# x`r`n"),
        $u8.GetBytes("[wsl2]`r`nmemory=26GB`n# x`r`nswap=24GB`r`nvmIdleTimeout=-1`r`n`r`n[experimental]`r`nautoMemoryReclaim=gradual`r`nsparseVhd=true`r`n"))
    'utf16 LE BOM'              = @(([Text.Encoding]::Unicode.GetPreamble() + [Text.Encoding]::Unicode.GetBytes("[wsl2]`r`nmemory=16GB`r`n")),
        ([Text.Encoding]::Unicode.GetPreamble() + [Text.Encoding]::Unicode.GetBytes("[wsl2]`r`nmemory=26GB`r`nswap=24GB`r`nvmIdleTimeout=-1`r`n`r`n[experimental]`r`nautoMemoryReclaim=gradual`r`nsparseVhd=true`r`n")))
    'ANSI (not UTF-8)'          = @(([byte[]]([Text.Encoding]::ASCII.GetBytes("[wsl2]`r`n# caf") + [byte]0xE9 + [Text.Encoding]::ASCII.GetBytes("`r`nmemory=16GB`r`n"))), $null)
    'UTF-32 LE BOM'             = @(([byte[]](0xFF, 0xFE, 0, 0) + [Text.Encoding]::UTF32.GetBytes("[wsl2]`r`n")), $null)
    'UTF-32 BE BOM'             = @(([byte[]](0, 0, 0xFE, 0xFF) + (New-Object Text.UTF32Encoding($true, $false)).GetBytes("[wsl2]`r`n")), $null)
}
$dir = Join-Path $env:TEMP ('wslconfig-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory $dir | Out-Null
$failed = 0
try {
    $n = 0
    foreach ($name in $cases.Keys) {
        $in, $want = $cases[$name]
        $cfgPath = Join-Path $dir "case$n.wslconfig"; $n++
        [IO.File]::WriteAllBytes($cfgPath, [byte[]]$in)
        $refused = $false
        try { . $block } catch { $refused = $true }
        $got = [IO.File]::ReadAllBytes($cfgPath)
        $ok = $true
        if ($null -eq $want) {
            $ok = $refused -and ([Convert]::ToBase64String($got) -eq [Convert]::ToBase64String([byte[]]$in))
        } else {
            $ok = (-not $refused) -and ([Convert]::ToBase64String($got) -eq [Convert]::ToBase64String([byte[]]$want))
            if ($ok) {   # a second run changes nothing
                . $block
                $ok = [Convert]::ToBase64String([IO.File]::ReadAllBytes($cfgPath)) -eq [Convert]::ToBase64String($got)
            }
        }
        if ($ok) { Write-Host "ok    $name" } else { Write-Host "FAIL  $name (refused: $refused)"; $failed++ }
    }
} finally { Remove-Item -Recurse -Force $dir }
if ($failed) { exit 1 }
