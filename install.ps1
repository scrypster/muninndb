# MuninnDB installer for Windows
# Usage: irm https://muninndb.com/install.ps1 | iex
#   or:  powershell -ExecutionPolicy Bypass -File install.ps1

$ErrorActionPreference = "Stop"
$repo = "scrypster/muninndb"
$installDir = "$env:LOCALAPPDATA\muninn"

# When piped through iex, exit closes the PowerShell session and the window
# disappears before the user can read the error.  This helper prints the error
# and waits for a keypress so the message is always visible.
function Abort($msg) {
    Write-Host ""
    Write-Host "  Error: $msg" -ForegroundColor Red
    Write-Host ""
    Write-Host "  Press any key to close..." -ForegroundColor DarkGray
    $null = $Host.UI.RawUI.ReadKey("NoEcho,IncludeKeyDown")
    exit 1
}

Write-Host ""
Write-Host "  Installing MuninnDB..." -ForegroundColor Cyan
Write-Host ""

# Detect architecture
$arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else {
    Abort "MuninnDB requires a 64-bit Windows system."
}

# Query GitHub API for the latest release
try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ "User-Agent" = "muninn-installer" }
} catch {
    Abort "Could not reach GitHub API. Check your internet connection.`n  $_"
}

$version = $release.tag_name -replace '^v', ''
$assetName = "muninn_${version}_windows_${arch}.zip"
$asset = $release.assets | Where-Object { $_.name -eq $assetName }

if (-not $asset) {
    $available = ($release.assets | ForEach-Object { $_.name }) -join "`n    "
    Abort "Could not find $assetName in release $($release.tag_name).`n  Available assets:`n    $available"
}

$downloadUrl = $asset.browser_download_url
$zipPath = "$env:TEMP\muninn.zip"

Write-Host "  Version:  $($release.tag_name)"
Write-Host "  Asset:    $assetName"
Write-Host ""

# Download
Write-Host "  Downloading..." -NoNewline
try {
    Invoke-WebRequest -Uri $downloadUrl -OutFile $zipPath -UseBasicParsing
    Write-Host " done" -ForegroundColor Green
} catch {
    Write-Host " failed" -ForegroundColor Red
    Abort "Download failed: $_"
}

# Extract
Write-Host "  Extracting..." -NoNewline
if (Test-Path $installDir) {
    Remove-Item "$installDir\muninn.exe" -ErrorAction SilentlyContinue
}
New-Item -ItemType Directory -Path $installDir -Force | Out-Null
Expand-Archive -Path $zipPath -DestinationPath $installDir -Force
Remove-Item $zipPath -ErrorAction SilentlyContinue
Write-Host " done" -ForegroundColor Green

# Verify binary
$binary = "$installDir\muninn.exe"
if (-not (Test-Path $binary)) {
    $contents = (Get-ChildItem $installDir | ForEach-Object { $_.Name }) -join "`n    "
    Abort "muninn.exe not found after extraction.`n  Contents of ${installDir}:`n    $contents"
}

# Add to PATH if not already there
$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -notlike "*$installDir*") {
    Write-Host "  Adding to PATH..." -NoNewline
    [Environment]::SetEnvironmentVariable("PATH", "$userPath;$installDir", "User")
    $env:PATH += ";$installDir"
    Write-Host " done" -ForegroundColor Green
} else {
    Write-Host "  Already in PATH"
}

# Print version
Write-Host ""
try {
    $ver = & $binary version 2>&1
    Write-Host "  Installed: muninn $ver" -ForegroundColor Green
} catch {
    Write-Host "  Installed: muninn.exe at $installDir" -ForegroundColor Green
}

Write-Host ""
Write-Host "  Next steps:" -ForegroundColor Cyan
Write-Host "    muninn init    # guided setup + AI tool config"
Write-Host "    muninn start   # start the server"
Write-Host ""
Write-Host "  Open a new terminal if 'muninn' is not recognized." -ForegroundColor DarkGray
Write-Host ""
