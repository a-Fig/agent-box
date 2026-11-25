# Installs agent-box's client, box, on Windows: downloads the latest stable release's binary,
# checks it against the release's SHA256SUMS, puts it in %USERPROFILE%\.local\bin and adds that
# folder to your user PATH. It needs no administrator rights. Linux and macOS have install.sh.
# Shaped like a-Fig/murmur's install.ps1.
#
#   irm https://raw.githubusercontent.com/a-Fig/agent-box/main/install.ps1 | iex
#
# Environment:
#   BOX_VERSION      install this release (v0.1.2 or 0.1.2) instead of the latest stable one
#   BOX_INSTALL_DIR  install here instead of %USERPROFILE%\.local\bin
#
# The work is in a script block so that iex leaves no variables in your session, and it throws
# rather than exits, since exit would close your window.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # the progress bar makes Windows PowerShell 5.1 downloads far slower
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'a-Fig/agent-box'
    $tag = $null
    if ($env:BOX_VERSION) {
        $tag = if ($env:BOX_VERSION.StartsWith('v')) { $env:BOX_VERSION } else { "v$($env:BOX_VERSION)" }
    }
    $base = if ($tag) { "https://github.com/$repo/releases/download/$tag" } else { "https://github.com/$repo/releases/latest/download" }

    # A 32-bit PowerShell on 64-bit Windows reports x86 here; PROCESSOR_ARCHITEW6432 has the real one.
    $machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    if ($machine -ne 'AMD64') { throw "box install: unsupported architecture $machine; box has a Windows binary for amd64 only." }
    $asset = 'box-windows-amd64.exe'

    $dir = if ($env:BOX_INSTALL_DIR) { $env:BOX_INSTALL_DIR } else { Join-Path $env:USERPROFILE '.local\bin' }
    $exe = Join-Path $dir 'box.exe'
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("box-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $asset$(if ($tag) { " ($tag)" }) from $repo..."
        foreach ($name in $asset, 'SHA256SUMS') {
            try {
                Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile (Join-Path $tmp $name)
            } catch {
                throw "box install: could not download $base/${name}: $($_.Exception.Message)"
            }
        }

        # SHA256SUMS lines read "<hash>  <name>", or "<hash> *<name>" for a binary file.
        $want = $null
        foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
            if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$' -and $Matches[2] -eq $asset) { $want = $Matches[1].ToLower(); break }
        }
        if (-not $want) { throw "box install: SHA256SUMS has no line for $asset." }
        $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $asset)).Hash.ToLower()
        if ($got -ne $want) { throw "box install: the checksum of $asset does not match SHA256SUMS (wanted $want, got $got); nothing was installed." }
        Write-Host 'Checksum OK.'

        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        # Agents' box mcp servers keep box.exe running, and Windows won't replace a running exe but
        # will rename it, so an existing box.exe moves aside first. Old copies go once nothing runs them.
        Get-ChildItem -Path $dir -Filter 'box.exe.old-*' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        $old = $null
        if (Test-Path $exe) {
            $old = "$exe.old-" + [Guid]::NewGuid().ToString('N').Substring(0, 8)
            Move-Item -Path $exe -Destination $old
        }
        try {
            Copy-Item -Path (Join-Path $tmp $asset) -Destination $exe
        } catch {
            if ($old) { Move-Item -Force -Path $old -Destination $exe }
            throw "box install: could not put box.exe in place at $exe ($_); the one installed is unchanged."
        }
    } finally {
        Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
    }

    # The user PATH is edited in the registry: [Environment]::SetEnvironmentVariable would write back
    # the expanded value, turning an entry like %USERPROFILE%\go\bin into a fixed path.
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    try {
        $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $kind = if ($null -ne $key.GetValue('Path', $null)) { $key.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
        $present = $false
        foreach ($entry in ($current -split ';')) {
            if ($entry -and ([Environment]::ExpandEnvironmentVariables($entry).TrimEnd('\') -ieq $dir.TrimEnd('\'))) { $present = $true }
        }
        if (-not $present) {
            $key.SetValue('Path', ($(if ($current) { $current.TrimEnd(';') + ';' } else { '' }) + $dir), $kind)
            # Writing the registry doesn't tell running programs; setting any user variable does.
            [Environment]::SetEnvironmentVariable('BOX_INSTALL_REFRESH', '1', 'User')
            [Environment]::SetEnvironmentVariable('BOX_INSTALL_REFRESH', $null, 'User')
        }
    } finally {
        $key.Close()
    }
    if (-not @($env:Path -split ';' | Where-Object { $_.TrimEnd('\') -ieq $dir.TrimEnd('\') }).Count) { $env:Path = "$dir;$env:Path" }

    & $exe --version
    if ($LASTEXITCODE -ne 0) { throw "box install: installed $exe, but it did not run." }
    Write-Host "Installed: $exe"
    if (-not $present) { Write-Host "Added $dir to your user PATH; new terminals find box, and so does this one." }
    Write-Host ''
    Write-Host 'Next:'
    if (-not (Get-Command incus -ErrorAction SilentlyContinue)) {
        Write-Host '  - install the incus client: winget install LinuxContainers.Incus'
    }
    Write-Host '  - add the host as a remote named pc, with the line its setup printed:'
    Write-Host '      incus remote add pc https://<host>:8443 --token <token>'
    Write-Host "  - give your coding agents the box tools: box join"
}
