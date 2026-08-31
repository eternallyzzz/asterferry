[CmdletBinding()]
param(
    [string]$WslDistro = "Debian"
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$outputDir = Join-Path $root "tmp/perf/cross"
$null = New-Item -ItemType Directory -Force -Path $outputDir

function Invoke-Version([string]$binary, [string]$name) {
    $output = (& $binary version --short | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $output.Length -eq 0) {
        throw "$name version check failed"
    }
    [ordered]@{ name = $name; version = $output }
}

$windowsBinary = Join-Path $outputDir "asterferry-windows.exe"
$linuxBinary = Join-Path $outputDir "asterferry-linux"
& go build -trimpath -o $windowsBinary ./cmd/asterferry
if ($LASTEXITCODE -ne 0) { throw "failed to build Windows Controller/node binary" }
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
try {
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    & go build -trimpath -o $linuxBinary ./cmd/asterferry
    if ($LASTEXITCODE -ne 0) { throw "failed to build Linux Controller/node binary" }
} finally {
    if ($null -eq $oldGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGOOS }
    if ($null -eq $oldGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGOARCH }
    if ($null -eq $oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
}

$wslBinary = (& wsl.exe -d $WslDistro -- wslpath -a ($linuxBinary -replace '\', '/')).Trim()
$wslVersion = (& wsl.exe -d $WslDistro -- $wslBinary version --short | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $wslVersion.Length -eq 0) { throw "WSL AFDP/2 binary check failed" }
$report = [ordered]@{
    commit = (git rev-parse HEAD).Trim()
    protocol = "AFDP/2 + control/1"
    binaries = @(
        (Invoke-Version $windowsBinary "windows"),
        [ordered]@{ name = "wsl-linux"; version = $wslVersion }
    )
}
$report | ConvertTo-Json -Depth 5 | Set-Content -Encoding utf8 (Join-Path $outputDir "summary.json")
Write-Host "Cross-platform Controller/node build checks passed."
