# Installs the wayfindr binary from GitHub Releases on Windows.
#
#   irm https://atomicpages.github.io/wayfindr/install.ps1 | iex
#
# Environment (set with $env:NAME = "..." before running):
#   WAYFINDR_VERSION       release tag to install (default: latest)
#   WAYFINDR_INSTALL_DIR   directory for wayfindr.exe
#                          (default: %LOCALAPPDATA%\Programs\wayfindr\bin)
#   WAYFINDR_DOWNLOAD_URL  base URL holding the archives and checksums.txt, in
#                          place of the GitHub release (a mirror, or file:// in CI)
#
# The archive's SHA-256 must match checksums.txt or nothing is installed. The
# install directory is added to the user PATH. Everything runs from the call at
# the end, so a download cut short runs nothing.
# macOS and Linux: curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh

function Install-Wayfindr {
    $ErrorActionPreference = 'Stop'
    # $IsWindows exists only in PowerShell 6+, which also runs on macOS and Linux.
    if ((Test-Path variable:IsWindows) -and -not $IsWindows) {
        throw 'wayfindr: install.ps1 is for Windows; on macOS and Linux run: curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh'
    }
    # The progress bar makes Invoke-WebRequest very slow in Windows PowerShell 5.1.
    $ProgressPreference = 'SilentlyContinue'
    # Windows PowerShell 5.1 may not offer TLS 1.2 by default; GitHub requires it.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'atomicpages/wayfindr'
    $version = if ($env:WAYFINDR_VERSION) { $env:WAYFINDR_VERSION } else { 'latest' }
    $installDir = if ($env:WAYFINDR_INSTALL_DIR) { $env:WAYFINDR_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\wayfindr\bin' }

    # The OS architecture, not this process's: x64 PowerShell on an Arm64
    # machine still gets the native build.
    $osArch = try { [string][Runtime.InteropServices.RuntimeInformation]::OSArchitecture } catch { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($osArch) {
        { $_ -in 'X64', 'AMD64' } { 'amd64'; break }
        { $_ -in 'Arm64', 'ARM64' } { 'arm64'; break }
        default { throw "wayfindr: no release for this architecture: $osArch" }
    }
    $asset = "wayfindr-windows-$arch.zip"

    if ($env:WAYFINDR_DOWNLOAD_URL) {
        $base = $env:WAYFINDR_DOWNLOAD_URL.TrimEnd('/')
    } elseif ($version -eq 'latest') {
        $base = "https://github.com/$repo/releases/latest/download"
    } else {
        if (-not $version.StartsWith('v')) { $version = "v$version" }
        $base = "https://github.com/$repo/releases/download/$version"
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("wayfindr-" + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    $staged = $null
    try {
        Write-Host "wayfindr: downloading $asset from $base"
        foreach ($name in $asset, 'checksums.txt') {
            $uri = [Uri]"$base/$name"
            $dest = Join-Path $tmp $name
            if ($uri.IsFile) {
                Copy-Item -LiteralPath $uri.LocalPath -Destination $dest
            } elseif ($uri.Scheme -eq 'https') {
                Invoke-WebRequest -Uri $uri -OutFile $dest -UseBasicParsing
            } else {
                throw "wayfindr: refusing to download over $($uri.Scheme): $uri"
            }
        }

        $want = Get-Content -LiteralPath (Join-Path $tmp 'checksums.txt') |
            ForEach-Object { $f = -split $_; if ($f[1] -eq $asset) { $f[0] } } |
            Select-Object -First 1
        if (-not $want) { throw "wayfindr: $asset is not listed in checksums.txt" }
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $asset)).Hash
        if ($got -ne $want) { throw "wayfindr: checksum mismatch for ${asset}: expected $want, got $($got.ToLowerInvariant())" }

        Expand-Archive -LiteralPath (Join-Path $tmp $asset) -DestinationPath (Join-Path $tmp 'x')
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        $exe = Join-Path $installDir 'wayfindr.exe'
        $staged = "$exe.new"
        Copy-Item -LiteralPath (Join-Path $tmp 'x\wayfindr.exe') -Destination $staged -Force
        # A running wayfindr.exe can't be overwritten but can be renamed: move
        # it aside, and the daemon keeps running the old binary until it restarts.
        # Old copies from earlier upgrades go too, unless a process still runs
        # one; then this one takes a unique name instead.
        if (Test-Path -LiteralPath $exe) {
            Get-ChildItem -Path "$exe.old*" -ErrorAction SilentlyContinue |
                Remove-Item -Force -ErrorAction SilentlyContinue
            $old = "$exe.old"
            if (Test-Path -LiteralPath $old) { $old = "$exe.old-" + [Guid]::NewGuid().ToString('N') }
            Move-Item -LiteralPath $exe -Destination $old -Force
        }
        Move-Item -LiteralPath $staged -Destination $exe -Force
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
        if ($staged) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
    }

    # Read and write the raw registry value: [Environment]::GetEnvironmentVariable
    # expands %VARS%, and writing that back would flatten them for good.
    # CreateSubKey opens the key for writing, creating it if it's missing.
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        $userPath = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if (($userPath -split ';') -notcontains $installDir) {
            $newPath = (@($userPath.TrimEnd(';'), $installDir) | Where-Object { $_ }) -join ';'
            $key.SetValue('Path', $newPath, [Microsoft.Win32.RegistryValueKind]::ExpandString)
            # Setting any user variable broadcasts the change to new terminals.
            [Environment]::SetEnvironmentVariable('WAYFINDR_INSTALL', '1', 'User')
            [Environment]::SetEnvironmentVariable('WAYFINDR_INSTALL', $null, 'User')
            Write-Host "wayfindr: added $installDir to your user PATH; open a new terminal to use it"
        }
    } finally {
        $key.Close()
    }
    if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$env:Path;$installDir" }

    Write-Host "wayfindr: installed $(& $exe version) at $exe"
    Write-Host 'wayfindr: next: wayfindr init'
}

Install-Wayfindr
