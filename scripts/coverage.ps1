[CmdletBinding()]
param(
    [string]$WslDistro = "Debian"
)

$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root
$toolchain = Get-Content -Raw -LiteralPath (Join-Path $root ".toolchain.json") | ConvertFrom-Json

if (-not $PSBoundParameters.ContainsKey("WslDistro") -and -not [string]::IsNullOrWhiteSpace($env:ASTERFERRY_WSL_DISTRO)) {
    $WslDistro = $env:ASTERFERRY_WSL_DISTRO
}

$expectedGoVersion = if ([string]::IsNullOrWhiteSpace($env:ASTERFERRY_EXPECTED_GO_VERSION)) {
    "go$($toolchain.release.go)"
} else {
    $env:ASTERFERRY_EXPECTED_GO_VERSION
}

$outputDir = Join-Path $root "tmp/coverage/windows"
$null = New-Item -ItemType Directory -Force -Path $outputDir
$logPath = Join-Path $outputDir "coverage.log"

function Require-Command([string]$Name) {
    if ($null -eq (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command not found: $Name"
    }
}

function Invoke-Logged([string]$Name, [string]$File, [string[]]$Arguments) {
    Write-Host "== $Name =="
    $oldErrorAction = $ErrorActionPreference
    try {
        $ErrorActionPreference = "Continue"
        & $File @Arguments 2>&1 | Tee-Object -FilePath $logPath -Append
        $exitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $oldErrorAction
    }
    if ($exitCode -ne 0) {
        throw "$Name failed with exit code $exitCode"
    }
}

function Get-RuntimePackages {
    $listed = @(& go list -f '{{if .GoFiles}}{{.ImportPath}}{{end}}' ./...)
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to enumerate Go runtime packages"
    }
    $packages = @($listed |
        ForEach-Object { $_.Trim() } |
        Where-Object {
            $_ -and
            $_ -notmatch "/cmd/asterferry-bench$"
        })
    if ($packages.Count -eq 0) {
        throw "No runtime packages were found"
    }
    return $packages
}

function Write-CoverageArtifacts([string]$Name, [string]$OutputPath, [string[]]$Packages) {
    $profilePath = Join-Path $OutputPath "coverage.out"
    $functionPath = Join-Path $OutputPath "functions.txt"
    $htmlPath = Join-Path $OutputPath "coverage.html"
    $coverPackages = $Packages -join ","

    $testArguments = @(
        "test",
        "-count=1",
        "-covermode=atomic",
        ("-coverpkg=" + $coverPackages),
        ("-coverprofile=" + $profilePath),
        "./..."
    )
    Invoke-Logged "$Name coverage tests" "go" $testArguments

    $functionArguments = @("tool", "cover", ("-func=" + $profilePath))
    Write-Host "== $Name coverage summary =="
    $functionOutput = @(& go @functionArguments 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "$Name coverage summary failed"
    }
    $functionOutput | Tee-Object -FilePath $functionPath
    $total = $functionOutput | Select-String "^total:" | Select-Object -Last 1
    if ($null -eq $total) {
        throw "$Name coverage summary did not contain a total"
    }
    Write-Host "$Name $($total.Line.Trim())"

    $htmlArguments = @("tool", "cover", ("-html=" + $profilePath), ("-o=" + $htmlPath))
    Invoke-Logged "$Name coverage HTML" "go" $htmlArguments

    if (-not (Test-Path -LiteralPath $profilePath) -or (Get-Item -LiteralPath $profilePath).Length -eq 0) {
        throw "$Name coverage profile is missing or empty"
    }
    if (-not (Test-Path -LiteralPath $htmlPath) -or (Get-Item -LiteralPath $htmlPath).Length -eq 0) {
        throw "$Name coverage HTML report is missing or empty"
    }
}

try {
    Require-Command "go"
    Require-Command "wsl.exe"

    $goVersion = (& go version).Trim()
    if ($goVersion -notlike "*$expectedGoVersion*") {
        throw "Expected Go $expectedGoVersion, got: $goVersion"
    }

    $packages = @(Get-RuntimePackages)
    $metadata = [ordered]@{
        timestamp_utc = [DateTime]::UtcNow.ToString("o")
        commit = (& git rev-parse HEAD).Trim()
        go = $goVersion
        goos = (& go env GOOS).Trim()
        goarch = (& go env GOARCH).Trim()
        included_packages = $packages
        excluded_packages = @("cmd/asterferry-bench")
    }
    $metadata | ConvertTo-Json -Depth 4 | Set-Content -Encoding utf8 (Join-Path $outputDir "metadata.json")

    Write-CoverageArtifacts "Windows" $outputDir $packages

    $wslRoot = (& wsl.exe -d $WslDistro -- wslpath -a ($root -replace "\\", "/")).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($wslRoot)) {
        throw "Unable to resolve repository path in WSL distribution $WslDistro"
    }
    $wslOutputDir = "$wslRoot/tmp/coverage/wsl"
    $wslScript = "$wslRoot/scripts/coverage-wsl.sh"
    & wsl.exe -d $WslDistro -- bash -lc "command -v go >/dev/null 2>&1" 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "WSL distribution $WslDistro has no Go installation; install $expectedGoVersion before running coverage"
    }

    Invoke-Logged "WSL coverage ($WslDistro)" "wsl.exe" @(
        "-d", $WslDistro, "--", "env",
        "ASTERFERRY_WSL_ROOT=$wslRoot",
        "ASTERFERRY_COVERAGE_OUTPUT_DIR=$wslOutputDir",
        "ASTERFERRY_EXPECTED_GO_VERSION=$expectedGoVersion",
        "bash", $wslScript
    )

    Write-Host "Coverage reports written under tmp/coverage/windows and tmp/coverage/wsl"
} catch {
    Write-Error $_
    exit 1
}
