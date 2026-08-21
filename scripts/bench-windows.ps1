$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root
$benchTime = if ($env:ASTERFERRY_BENCHTIME) { $env:ASTERFERRY_BENCHTIME } else { "15s" }
$count = if ($env:ASTERFERRY_BENCHCOUNT) { $env:ASTERFERRY_BENCHCOUNT } else { "5" }
$outputDir = Join-Path $root "tmp/perf/windows"
New-Item -ItemType Directory -Force -Path $outputDir | Out-Null

go version
go env GOOS GOARCH GOMAXPROCS
git rev-parse HEAD
go test ./internal/transport ./internal/relay ./internal/integration `
  -run '^$' `
  -bench 'Benchmark(QUICStream|ConnRoundTrip|AsterFerryProxy)' `
  -benchmem `
  "-benchtime=$benchTime" `
  "-count=$count" `
  | Tee-Object (Join-Path $outputDir "bench.txt")
