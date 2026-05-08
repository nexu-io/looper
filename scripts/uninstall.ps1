param(
  [string]$InstallPath = ${env:LOOPER_INSTALL_PATH}
)

function Log($msg) { Write-Host $msg }

function Confirm($prompt) {
  $answer = Read-Host "$prompt [y/N]"
  return $answer -match '^(y|yes)$'
}

function Remove-IfExists($path) {
  if (Test-Path $path) {
    Remove-Item -Recurse -Force $path -ErrorAction SilentlyContinue
    Log "Removed $path"
  }
}

function Is-InstallerOwnedPath($path) {
  $parentDir = Split-Path $path -Parent
  return ($path -match "$env:APPDATA\\Looper") -or ($path -match "$env:LOCALAPPDATA\\Looper")
}

# Find CLI path
$cliPath = ""
if ($InstallPath) {
  $cliPath = $InstallPath
} else {
  try { $cliPath = (Get-Command looper -ErrorAction Stop).Source } catch {}
}

$looperHome = "$env:APPDATA\Looper"

if ($cliPath) {
  if (Is-InstallerOwnedPath $cliPath) {
    Remove-IfExists $cliPath
  } elseif ($InstallPath -and (Confirm "Remove CLI binary at $cliPath? This path is not recognized as installer-owned.")) {
    Remove-IfExists $cliPath
  } else {
    Log "Skipped CLI binary at $cliPath (not recognized as installer-owned; set LOOPER_INSTALL_PATH and confirm to remove)"
  }
} else {
  Log "looper CLI not found on PATH; skipping CLI removal."
}

Remove-IfExists "$looperHome\bin\looperd.exe"
Remove-IfExists "$looperHome\bin\looperd.prev.exe"
Remove-IfExists "$looperHome\state"
Remove-IfExists "$looperHome\run\upgrade.lock"

if (Confirm "Also remove config, database, backups, logs, and worktrees under $looperHome?") {
  Remove-IfExists "$looperHome\config.json"
  Remove-IfExists "$looperHome\looper.sqlite"
  Remove-IfExists "$looperHome\backups"
  Remove-IfExists "$looperHome\logs"
  Remove-IfExists "$looperHome\worktrees"
}

Log "Looper uninstall complete"
