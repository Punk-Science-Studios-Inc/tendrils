# Tendrils installer for Windows.
#
#   irm https://raw.githubusercontent.com/Punk-Science-Studios-Inc/tendrils/main/install.ps1 | iex
#
# Downloads the latest release, verifies it against the release's checksums.txt,
# and installs tendrils.exe + blossomd.exe. No toolchain required.
#
# Falls back to building from source (needs Go 1.26+ and git) when no release is
# available for this platform.
#
# Env: TENDRILS_BIN_DIR  install directory (default %LOCALAPPDATA%\Programs\tendrils)
#      TENDRILS_VERSION  install a specific tag, e.g. v0.1.0 (default: latest)
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$slug = 'Punk-Science-Studios-Inc/tendrils'
$repo = "https://github.com/$slug.git"

function Info($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Warn($m) { Write-Host "warn: $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host "ERROR: $m" -ForegroundColor Red; exit 1 }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { '' }
}

$tmp = Join-Path $env:TEMP ("tendrils-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null

function Get-LatestVersion {
    try { (Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$slug/releases/latest").tag_name }
    catch { '' }
}

function Install-Release($ver) {
    $archive = "tendrils_$($ver.TrimStart('v'))_windows_$arch.zip"
    $base    = "https://github.com/$slug/releases/download/$ver"

    Info "Downloading $archive"
    Invoke-WebRequest -UseBasicParsing "$base/$archive"       -OutFile "$tmp\$archive"
    Invoke-WebRequest -UseBasicParsing "$base/checksums.txt"  -OutFile "$tmp\checksums.txt"

    $want = (Get-Content "$tmp\checksums.txt" |
             Where-Object { ($_ -split '\s+')[1] -eq $archive } |
             ForEach-Object { ($_ -split '\s+')[0] } | Select-Object -First 1)
    if (-not $want) { Die "$archive is not listed in the release checksums" }
    $got = (Get-FileHash "$tmp\$archive" -Algorithm SHA256).Hash.ToLower()
    if ($want -ne $got) { Die "checksum mismatch for $archive (expected $want, got $got)" }
    Info "Checksum verified"

    Expand-Archive -Path "$tmp\$archive" -DestinationPath $tmp -Force
    Test-Path "$tmp\tendrils.exe"
}

function Build-FromSource {
    if (-not (Get-Command go  -ErrorAction SilentlyContinue)) { Die "no release available for this platform and Go 1.26+ is not installed (https://go.dev/dl/)" }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Die "no release available for this platform and git is not installed" }
    Info "Building from source ($((go version) -replace '^go version ',''))"
    git clone --depth 1 $repo "$tmp\src" 2>$null
    if ($LASTEXITCODE -ne 0) { Die "git clone failed" }
    Push-Location "$tmp\src"
    try {
        & go build -o "$tmp\tendrils.exe" ./cmd/tendrils
        if ($LASTEXITCODE -ne 0) { Die "build failed" }
        & go build -o "$tmp\blossomd.exe" ./cmd/blossomd
    } finally { Pop-Location }
}

try {
    $installed = $false
    if ($arch) {
        $version = if ($env:TENDRILS_VERSION) { $env:TENDRILS_VERSION } else { Get-LatestVersion }
        if ($version) {
            try { $installed = Install-Release $version }
            catch { Warn "could not install the $version release ($($_.Exception.Message)); falling back to a source build" }
        } else {
            Warn "no published release found; falling back to a source build"
        }
    } else {
        Warn "$env:PROCESSOR_ARCHITECTURE has no prebuilt release; falling back to a source build"
    }
    if (-not $installed) { Build-FromSource }

    $dest = if ($env:TENDRILS_BIN_DIR) { $env:TENDRILS_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tendrils' }
    New-Item -ItemType Directory -Force -Path $dest | Out-Null

    # Windows refuses to overwrite a running executable, and the daemon also holds
    # an exclusive lock on the bbolt index. Both have to be released before an
    # upgrade can write over the old binary.
    $running = Get-Process -Name tendrils -ErrorAction SilentlyContinue
    if ($running) {
        Info "Stopping the running tendrils daemon so the binary can be replaced"
        $running | Stop-Process -Force
        Start-Sleep -Milliseconds 500
    }

    Copy-Item "$tmp\tendrils.exe" (Join-Path $dest 'tendrils.exe') -Force
    Info "Installed $dest\tendrils.exe"
    # blossomd is the optional self-hosted blob server. Shipping it here is what
    # lets someone run their own storage without installing a Go toolchain.
    if (Test-Path "$tmp\blossomd.exe") {
        Copy-Item "$tmp\blossomd.exe" (Join-Path $dest 'blossomd.exe') -Force
        Info "Installed $dest\blossomd.exe (optional Blossom blob server)"
    }

    Info (& (Join-Path $dest 'tendrils.exe') version | Select-Object -First 1)

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $dest) {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
        Info "Added $dest to your user PATH (open a new shell to pick it up)."
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host ""
if ($running) { Write-Host "The daemon was stopped for the upgrade — start it again with 'tendrils daemon'." }
Write-Host "Tendrils installed. Next steps:"
Write-Host "  1. tendrils keygen                            # create your master key — BACK UP the nsec"
Write-Host "  2. tendrils enroll --key <nsec> --root <folder> --relay wss://<relay> --blossom http://<blossom>:8091"
Write-Host "  3. tendrils daemon --interval 1m              # start syncing"
Write-Host ""
Write-Host "Enroll every device with the SAME key. See https://github.com/Punk-Science-Studios-Inc/tendrils"
