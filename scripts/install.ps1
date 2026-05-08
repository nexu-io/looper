param(
  [string]$Owner = ${env:LOOPER_GITHUB_OWNER},
  [string]$Repo = ${env:LOOPER_GITHUB_REPO},
  [string]$Version = ${env:LOOPER_VERSION},
  [string]$InstallDir = ${env:LOOPER_INSTALL_DIR}
)

if (-not $Owner) { $Owner = "nexu-io" }
if (-not $Repo) { $Repo = "looper" }
if (-not $Version) { $Version = "latest" }
if (-not $InstallDir) { $InstallDir = "$env:APPDATA\Looper\bin" }

function Log($msg) { Write-Host $msg }

function Fail($msg) { Write-Host "error: $msg" -ForegroundColor Red; exit 1 }

function Detect-Target {
  $os = "windows"
  $arch = if ([Environment]::Is64BitOperatingSystem) {
    $env:PROCESSOR_ARCHITECTURE
  } else { "x86" }

  switch -Wildcard ($arch) {
    "AMD64" { return "windows-amd64" }
    "ARM64" { return "windows-arm64" }
    default { Fail "unsupported architecture: $arch (supported: amd64, arm64)" }
  }
}

function Build-DownloadBase($tag) {
  if ($tag -eq "latest") {
    return "https://github.com/$Owner/$Repo/releases/latest/download"
  }
  return "https://github.com/$Owner/$Repo/releases/download/$tag"
}

$target = Detect-Target
$asset = "looper-$target"
$archiveAsset = "$asset.tar.gz"
$downloadBase = Build-DownloadBase $Version
$binaryUrl = "$downloadBase/$asset"
$checksumUrl = "$downloadBase/$asset.sha256"
$archiveUrl = "$downloadBase/$archiveAsset"
$archiveChecksumUrl = "$downloadBase/$archiveAsset.sha256"

Log "Detected target: $target"
Log "Download base: $downloadBase"

# Create install directory
New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null

# Use archive if available, fall back to raw binary
$tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) "looper-install"
if (Test-Path $tmpDir) { Remove-Item -Recurse -Force $tmpDir }
New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

$tmpBinary = Join-Path $tmpDir $asset

try {
  $archiveOk = $false
  try {
    $req = [System.Net.WebRequest]::Create($archiveUrl)
    $req.Method = "HEAD"
    $resp = $req.GetResponse()
    $archiveOk = $resp.StatusCode -eq 200
    $resp.Close()
  } catch { $archiveOk = $false }

  if ($archiveOk) {
    $archivePath = Join-Path $tmpDir $archiveAsset
    $archiveChecksumPath = Join-Path $tmpDir "$archiveAsset.sha256"

    Log "Downloading $archiveUrl"
    Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath
    Invoke-WebRequest -Uri $archiveChecksumUrl -OutFile $archiveChecksumPath

    $expected = (Get-Content $archiveChecksumPath).Split(' ')[0]
    $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected) { Fail "checksum mismatch for archive: expected $expected, got $actual" }

    tar -xzf $archivePath -C $tmpDir
    if (-not (Test-Path $tmpBinary)) { Fail "archive did not contain $asset" }
  } else {
    Log "Archive unavailable; downloading raw binary."
    $tmpChecksum = Join-Path $tmpDir "$asset.sha256"

    Log "Downloading $binaryUrl"
    Invoke-WebRequest -Uri $binaryUrl -OutFile $tmpBinary
    Invoke-WebRequest -Uri $checksumUrl -OutFile $tmpChecksum

    $expected = (Get-Content $tmpChecksum).Split(' ')[0]
    $actual = (Get-FileHash -Path $tmpBinary -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected) { Fail "checksum mismatch: expected $expected, got $actual" }
  }

  $installPath = Join-Path $InstallDir "looper.exe"
  Copy-Item -Path $tmpBinary -Destination $installPath -Force

  Log "Installed looper to $installPath"
} finally {
  Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
}

# Add to PATH if not already present
$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -notlike "*$InstallDir*") {
  $newPath = "$InstallDir;$userPath"
  [Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
  Log "Added $InstallDir to user PATH (restart shell or log out/in for changes to take effect)"
} else {
  Log "$InstallDir is already on PATH"
}

Log ""
Log "This installer only installs the looper CLI."
Log "looper bootstrap will install/start the matching looperd daemon."
Log ""
Log "Next steps:"
Log "  looper bootstrap"
Log "  looper status"
Log ""
Log "Manual daemon fallback commands:"
Log "  looper daemon install"
Log "  looper daemon start"
