$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root
$benchTime = if ($env:ASTERFERRY_BENCHTIME) { $env:ASTERFERRY_BENCHTIME } else { "30s" }
$count = if ($env:ASTERFERRY_BENCHCOUNT) { $env:ASTERFERRY_BENCHCOUNT } else { "5" }
$benchRegex = if ($env:ASTERFERRY_BENCHREGEX) { $env:ASTERFERRY_BENCHREGEX } else { "Benchmark(QUICStream|ConnRoundTrip|AsterFerryProxy)" }
$outputDir = Join-Path $root "tmp/perf/windows"
New-Item -ItemType Directory -Force -Path $outputDir | Out-Null

go version
go env GOOS GOARCH GOMAXPROCS
git rev-parse HEAD
$cpu = "unknown"
$logicalProcessors = 0
try { $cpu = (Get-CimInstance Win32_Processor | Select-Object -First 1 -ExpandProperty Name).Trim() } catch { }
try { $logicalProcessors = Get-CimInstance Win32_ComputerSystem | Select-Object -ExpandProperty NumberOfLogicalProcessors } catch { }
$metadata = [ordered]@{
  timestamp_utc = [DateTime]::UtcNow.ToString("o")
  commit = (git rev-parse HEAD).Trim()
  go = (go version).Trim()
  goos = (go env GOOS).Trim()
  goarch = (go env GOARCH).Trim()
  gomaxprocs = (go env GOMAXPROCS).Trim()
  cpu = $cpu
  logical_processors = $logicalProcessors
  bench_time = $benchTime
  count = $count
  regex = $benchRegex
}
$metadata | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $outputDir "metadata.json")

$benchFile = Join-Path $outputDir "bench.txt"
$lines = & go test ./internal/transport ./internal/relay ./internal/integration `
  -run '^$' `
  -bench $benchRegex `
  -benchmem `
  "-benchtime=$benchTime" `
  "-count=$count" `
  2>&1 | Tee-Object $benchFile
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$records = foreach ($line in $lines) {
  if ($line -match '^(Benchmark\S+)\s+\d+\s+([0-9.]+)\s+ns/op(?:\s+([0-9.]+)\s+MB/s)?') {
    [ordered]@{
      benchmark = $Matches[1]
      ns_per_op = [double]$Matches[2]
      mbps = if ($Matches[3]) { [double]$Matches[3] } else { $null }
    }
  }
}
@{
  metadata = $metadata
  results = @($records)
} | ConvertTo-Json -Depth 5 | Set-Content -Encoding utf8 (Join-Path $outputDir "summary.json")
