[CmdletBinding()]
param(
    [switch]$FullMatrix
)

$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$distro = if ($env:ASTERFERRY_WSL_DISTRO) { $env:ASTERFERRY_WSL_DISTRO } else { "Debian" }
$binaryDir = Join-Path $root "tmp/perf/wsl/bin"
$null = New-Item -ItemType Directory -Force -Path $binaryDir

$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
try {
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    & go test -c -o (Join-Path $binaryDir "transport.test") ./internal/transport
    if ($LASTEXITCODE -ne 0) { throw "failed to build transport benchmark binary" }
    & go test -c -o (Join-Path $binaryDir "relay.test") ./internal/relay
    if ($LASTEXITCODE -ne 0) { throw "failed to build relay benchmark binary" }
    & go test -c -o (Join-Path $binaryDir "integration.test") ./internal/integration
    if ($LASTEXITCODE -ne 0) { throw "failed to build integration benchmark binary" }
} finally {
    if ($null -eq $oldGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGOOS }
    if ($null -eq $oldGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGOARCH }
    if ($null -eq $oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
}

$wslRoot = (& wsl.exe -d $distro -- wslpath -a ($root -replace '\\', '/')).Trim()
$wslBinaryDir = (& wsl.exe -d $distro -- wslpath -a ($binaryDir -replace '\\', '/')).Trim()
$wslScript = "$wslRoot/scripts/bench-wsl.sh"
$benchTime = if ($env:ASTERFERRY_BENCHTIME) { $env:ASTERFERRY_BENCHTIME } else { "30s" }
$benchCount = if ($env:ASTERFERRY_BENCHCOUNT) { $env:ASTERFERRY_BENCHCOUNT } else { "5" }
$fullRegex = "Benchmark(QUICStream|ConnRoundTrip|AsterFerryProxy)"
$smokeRegex = "Benchmark(ConnRoundTrip|AsterFerryProxyLatency)|AsterFerryProxy/mode=(standard|camouflage)/profile=balanced/payload=65536/streams=8"
$benchRegex = if ($env:ASTERFERRY_BENCHREGEX) { $env:ASTERFERRY_BENCHREGEX } elseif ($FullMatrix) { $fullRegex } else { $smokeRegex }
$benchRegexB64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($benchRegex))
& wsl.exe -d $distro -- env `
    "ASTERFERRY_WSL_ROOT=$wslRoot" `
    "ASTERFERRY_BENCH_BINARY_DIR=$wslBinaryDir" `
    "ASTERFERRY_BENCHTIME=$benchTime" `
    "ASTERFERRY_BENCHCOUNT=$benchCount" `
    "ASTERFERRY_BENCH_SUITE=$(if ($FullMatrix) { 'full' } else { 'smoke' })" `
    "ASTERFERRY_BENCHREGEX_B64=$benchRegexB64" `
    bash $wslScript
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
