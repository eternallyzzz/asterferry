param(
    [ValidateSet("windows-gateway", "wsl-gateway")]
    [string]$Topology = "windows-gateway",
    [string]$WslDistro = "Debian",
    [Parameter(Mandatory = $true)]
    [string]$GatewayConfig,
    [Parameter(Mandatory = $true)]
    [string]$AgentConfig,
    [string]$GatewayReverseTarget = "127.0.0.1:28080",
    [string]$EchoListen = "127.0.0.1:39090",
    [ValidateSet("upload", "download", "roundtrip")]
    [string]$Direction = "roundtrip",
    [int]$Streams = 8,
    [int]$PayloadBytes = 65536,
    [TimeSpan]$Duration = ([TimeSpan]::FromSeconds(30))
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$outputDir = Join-Path $root "tmp/perf/cross"
$null = New-Item -ItemType Directory -Force -Path $outputDir

function Restore-GoEnvironment {
    if ($null -eq $script:oldGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $script:oldGOOS }
    if ($null -eq $script:oldGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $script:oldGOARCH }
    if ($null -eq $script:oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $script:oldCGO }
}

function To-WslPath([string]$path) {
    return (& wsl.exe -d $WslDistro -- wslpath -a ($path -replace '\\', '/')).Trim()
}

function Start-WindowsProcess([string]$file, [string[]]$arguments, [string]$name) {
    $stdout = Join-Path $outputDir "$name.stdout.log"
    $stderr = Join-Path $outputDir "$name.stderr.log"
    $quoted = @($arguments | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\\"') + '"' } else { $_ } })
    return Start-Process -FilePath $file -ArgumentList $quoted -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru -WindowStyle Hidden
}

function Start-WslProcess([string]$binary, [string[]]$arguments, [string]$name) {
    $stdout = Join-Path $outputDir "$name.stdout.log"
    $stderr = Join-Path $outputDir "$name.stderr.log"
    $wslArgs = @("-d", $WslDistro, "--", $binary) + $arguments
    return Start-Process -FilePath "wsl.exe" -ArgumentList $wslArgs -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru -WindowStyle Hidden
}

if (-not (Test-Path -LiteralPath $GatewayConfig) -or -not (Test-Path -LiteralPath $AgentConfig)) {
        throw "Both -GatewayConfig and -AgentConfig must point to existing v6 configurations."
}
if ($Streams -lt 1 -or $Streams -gt 4096) { throw "Streams must be between 1 and 4096." }
if ($PayloadBytes -lt 1 -or $PayloadBytes -gt (16MB)) { throw "PayloadBytes must be between 1 and 16MiB." }
if ($Duration.TotalSeconds -le 0 -or $Duration.TotalHours -gt 1) { throw "Duration must be between 0 and 1 hour." }

$script:oldGOOS = $env:GOOS
$script:oldGOARCH = $env:GOARCH
$script:oldCGO = $env:CGO_ENABLED
$windowsAsterferry = Join-Path $outputDir "asterferry-windows.exe"
$windowsBench = Join-Path $outputDir "asterferry-bench-windows.exe"
$linuxAsterferry = Join-Path $outputDir "asterferry-linux"
$linuxBench = Join-Path $outputDir "asterferry-bench-linux"
$processes = @()

try {
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    $env:CGO_ENABLED = "0"
    & go build -trimpath -o $windowsAsterferry ./cmd/asterferry
    if ($LASTEXITCODE -ne 0) { throw "failed to build Windows AsterFerry binary" }
    & go build -trimpath -o $windowsBench ./cmd/asterferry-bench
    if ($LASTEXITCODE -ne 0) { throw "failed to build Windows benchmark binary" }

    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    & go build -trimpath -o $linuxAsterferry ./cmd/asterferry
    if ($LASTEXITCODE -ne 0) { throw "failed to build Linux AsterFerry binary" }
    & go build -trimpath -o $linuxBench ./cmd/asterferry-bench
    if ($LASTEXITCODE -ne 0) { throw "failed to build Linux benchmark binary" }
    Restore-GoEnvironment

    $wslGatewayConfig = To-WslPath $GatewayConfig
    $wslAgentConfig = To-WslPath $AgentConfig
    $wslAsterferry = To-WslPath $linuxAsterferry
    $wslBench = To-WslPath $linuxBench
    $durationText = "{0}s" -f [math]::Round($Duration.TotalSeconds, 3)

    if ($Topology -eq "windows-gateway") {
        $processes += Start-WindowsProcess $windowsAsterferry @("gateway", "-c", $GatewayConfig) "windows-gateway"
        $processes += Start-WslProcess $wslBench @("echo", "-listen", $EchoListen, "-mode", $(if ($Direction -eq "download") { "source" } elseif ($Direction -eq "upload") { "discard" } else { "echo" }), "-payload", $PayloadBytes) "wsl-echo"
        $processes += Start-WslProcess $wslAsterferry @("agent", "-c", $wslAgentConfig) "wsl-agent"
        Start-Sleep -Seconds 3
        & $windowsBench load -target $GatewayReverseTarget -direction $Direction -streams $Streams -payload $PayloadBytes -duration $durationText | Tee-Object (Join-Path $outputDir "windows-gateway.json")
        if ($LASTEXITCODE -ne 0) { throw "Windows-to-WSL benchmark failed" }
    } else {
        $processes += Start-WslProcess $wslAsterferry @("gateway", "-c", $wslGatewayConfig) "wsl-gateway"
        $processes += Start-WindowsProcess $windowsBench @("echo", "-listen", $EchoListen, "-mode", $(if ($Direction -eq "download") { "source" } elseif ($Direction -eq "upload") { "discard" } else { "echo" }), "-payload", $PayloadBytes) "windows-echo"
        $processes += Start-WindowsProcess $windowsAsterferry @("agent", "-c", $AgentConfig) "windows-agent"
        Start-Sleep -Seconds 3
        $wslResult = & wsl.exe -d $WslDistro -- $wslBench load -target $GatewayReverseTarget -direction $Direction -streams $Streams -payload $PayloadBytes -duration $durationText
        if ($LASTEXITCODE -ne 0) { throw "WSL-to-Windows benchmark failed" }
        $wslResult | Tee-Object (Join-Path $outputDir "wsl-gateway.json")
    }
} finally {
    foreach ($process in $processes) {
        if ($null -ne $process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        }
    }
    & wsl.exe -d $WslDistro -- bash -lc "pkill -TERM -x asterferry-linux 2>/dev/null || true; pkill -TERM -x asterferry-bench-linux 2>/dev/null || true" | Out-Null
    Restore-GoEnvironment
}
