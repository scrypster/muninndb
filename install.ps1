# MuninnDB installer for Windows
# Usage: irm https://muninndb.com/install.ps1 | iex
#   or:  powershell -ExecutionPolicy Bypass -File install.ps1

$ErrorActionPreference = "Stop"
$repo = "scrypster/muninndb"
$installDir = "$env:LOCALAPPDATA\muninn"

Write-Host ""
Write-Host "  Installing MuninnDB..." -ForegroundColor Cyan
Write-Host ""

# Detect architecture
$arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else {
    Write-Host "  Error: MuninnDB requires a 64-bit Windows system." -ForegroundColor Red
    exit 1
}

# Query GitHub API for the latest release
try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ "User-Agent" = "muninn-installer" }
} catch {
    Write-Host "  Error: Could not reach GitHub API. Check your internet connection." -ForegroundColor Red
    Write-Host "  $_" -ForegroundColor DarkGray
    exit 1
}

$version = $release.tag_name -replace '^v', ''
$assetName = "muninn_${version}_windows_${arch}.zip"
$asset = $release.assets | Where-Object { $_.name -eq $assetName }

if (-not $asset) {
    Write-Host "  Error: Could not find $assetName in release $($release.tag_name)." -ForegroundColor Red
    Write-Host "  Available assets:" -ForegroundColor DarkGray
    $release.assets | ForEach-Object { Write-Host "    $($_.name)" -ForegroundColor DarkGray }
    exit 1
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
    Write-Host "  $_" -ForegroundColor DarkGray
    exit 1
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
    Write-Host "  Error: muninn.exe not found after extraction." -ForegroundColor Red
    Write-Host "  Contents of ${installDir}:" -ForegroundColor DarkGray
    Get-ChildItem $installDir | ForEach-Object { Write-Host "    $($_.Name)" -ForegroundColor DarkGray }
    exit 1
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
