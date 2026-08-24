[CmdletBinding()]
param(
    [string]$WslDistro = "Debian",
    [switch]$FullBench,
    [switch]$SkipRace
)

$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root

if (-not $PSBoundParameters.ContainsKey("WslDistro") -and -not [string]::IsNullOrWhiteSpace($env:ASTERFERRY_WSL_DISTRO)) {
    $WslDistro = $env:ASTERFERRY_WSL_DISTRO
}

$expectedGoVersion = if ([string]::IsNullOrWhiteSpace($env:ASTERFERRY_EXPECTED_GO_VERSION)) {
    "go1.26.7"
} else {
    $env:ASTERFERRY_EXPECTED_GO_VERSION
}
$outputDir = Join-Path $root "tmp/test/windows"
$null = New-Item -ItemType Directory -Force -Path $outputDir
$logPath = Join-Path $outputDir "test.log"

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

function Invoke-LoggedToFile([string]$Name, [string]$File, [string[]]$Arguments, [string]$CapturePath) {
    Write-Host "== $Name =="
    $oldErrorAction = $ErrorActionPreference
    try {
        $ErrorActionPreference = "Continue"
        & $File @Arguments 2>&1 | Tee-Object -FilePath $CapturePath | Tee-Object -FilePath $logPath -Append
        $exitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $oldErrorAction
    }
    if ($exitCode -ne 0) {
        throw "$Name failed with exit code $exitCode"
    }
}

function Build-GoTarget([string]$GoOS, [string]$GoArch, [string]$Name) {
    $targetDir = Join-Path $outputDir $Name
    $null = New-Item -ItemType Directory -Force -Path $targetDir
    $oldGoOS = $env:GOOS
    $oldGoArch = $env:GOARCH
    $oldCGO = $env:CGO_ENABLED
    try {
        $env:GOOS = $GoOS
        $env:GOARCH = $GoArch
        $env:CGO_ENABLED = "0"
        $suffix = if ($GoOS -eq "windows") { ".exe" } else { "" }
        $asterferryPath = Join-Path $targetDir ("asterferry" + $suffix)
        $benchmarkPath = Join-Path $targetDir ("asterferry-bench" + $suffix)
        Invoke-Logged "build $GoOS/$GoArch asterferry" "go" @("build", "-trimpath", "-o", $asterferryPath, "./cmd/asterferry")
        Invoke-Logged "build $GoOS/$GoArch benchmark" "go" @("build", "-trimpath", "-o", $benchmarkPath, "./cmd/asterferry-bench")
    } finally {
        if ($null -eq $oldGoOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGoOS }
        if ($null -eq $oldGoArch) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGoArch }
        if ($null -eq $oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
    }
}

function Build-WslTestBinaries {
    $targetDir = Join-Path $outputDir "wsl-tests"
    $null = New-Item -ItemType Directory -Force -Path $targetDir
    $manifestPath = Join-Path $targetDir "manifest.tsv"
    Set-Content -Encoding utf8 -Path $manifestPath -Value ""
    $oldGoOS = $env:GOOS
    $oldGoArch = $env:GOARCH
    $oldCGO = $env:CGO_ENABLED
    try {
        $env:GOOS = "linux"
        $env:GOARCH = "amd64"
        $env:CGO_ENABLED = "0"
        $packages = @(& go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
        if ($LASTEXITCODE -ne 0 -or $packages.Count -eq 0) {
            throw "Unable to enumerate Go packages with tests for the WSL fallback"
        }
        for ($index = 0; $index -lt $packages.Count; $index++) {
            $package = $packages[$index].Trim()
            $binaryPath = Join-Path $targetDir ("{0:D3}.test" -f $index)
            Invoke-Logged "cross-compile WSL test $package" "go" @("test", "-c", "-o", $binaryPath, $package)
            Add-Content -Encoding utf8 -Path $manifestPath -Value ("{0:D3}`t{1}" -f $index, $package)
        }
    } finally {
        if ($null -eq $oldGoOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGoOS }
        if ($null -eq $oldGoArch) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGoArch }
        if ($null -eq $oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
    }
    return $targetDir
}

try {
    Require-Command "go"
    Require-Command "gofmt"
    Require-Command "wsl.exe"
    Require-Command "docker"
    Require-Command "helm"

    $goVersion = (& go version).Trim()
    if ($goVersion -notlike "*$expectedGoVersion*") {
        throw "Expected Go $expectedGoVersion, got: $goVersion"
    }
    $cgoEnabled = (& go env CGO_ENABLED).Trim()
    if (-not $SkipRace -and $cgoEnabled -ne "1") {
        throw "CGO_ENABLED must be 1 for the Windows race test"
    }

    $goFiles = @(Get-ChildItem -Path $root -Recurse -File -Filter "*.go" |
        Where-Object { $_.FullName -notlike "$root\.git\*" -and $_.FullName -notlike "$root\tmp\*" } |
        Select-Object -ExpandProperty FullName)
    if ($goFiles.Count -eq 0) {
        throw "No Go files found"
    }
    $unformatted = @(gofmt -l $goFiles)
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt check failed"
    }
    if ($unformatted.Count -gt 0) {
        throw "Unformatted Go files: $($unformatted -join ', ')"
    }

    $commit = (& git rev-parse HEAD).Trim()
    $metadata = [ordered]@{
        timestamp_utc = [DateTime]::UtcNow.ToString("o")
        commit = $commit
        go = $goVersion
        goos = (& go env GOOS).Trim()
        goarch = (& go env GOARCH).Trim()
        gomaxprocs = (& go env GOMAXPROCS).Trim()
        wsl_distro = $WslDistro
        full_bench = [bool]$FullBench
        skip_race = [bool]$SkipRace
    }
    $metadata | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $outputDir "metadata.json")

    Invoke-Logged "Windows go vet" "go" @("vet", "./...")
    Invoke-Logged "Windows full tests" "go" @("test", "-count=1", "./...")
    $uxDir = Join-Path $outputDir "ux-bundle"
    if (Test-Path -LiteralPath $uxDir) {
        Remove-Item -LiteralPath $uxDir -Recurse -Force
    }
    Invoke-Logged "CLI init smoke test" "go" @("run", "./cmd/asterferry", "init", "--dir", $uxDir, "--profile", "dev")
    Invoke-Logged "CLI gateway doctor smoke test" "go" @("run", "./cmd/asterferry", "doctor", "--config", (Join-Path $uxDir "config/gateway.yaml"), "--skip-ports")
    Invoke-Logged "CLI agent doctor smoke test" "go" @("run", "./cmd/asterferry", "doctor", "--config", (Join-Path $uxDir "config/agent.yaml"), "--skip-ports")
    if ($SkipRace) {
        Write-Host "== Windows race tests skipped by -SkipRace =="
    } else {
        Invoke-Logged "Windows race tests" "go" @("test", "-race", "-count=1", "./...")
    }

    Build-GoTarget "windows" "amd64" "windows-amd64"
    Build-GoTarget "linux" "amd64" "linux-amd64"
    Build-GoTarget "linux" "arm64" "linux-arm64"

    $wslRoot = (& wsl.exe -d $WslDistro -- wslpath -a ($root -replace "\\", "/")).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($wslRoot)) {
        throw "Unable to resolve repository path in WSL distribution $WslDistro"
    }
    $wslOutputDir = "$wslRoot/tmp/test/wsl"
    $wslScript = "$wslRoot/scripts/test-wsl.sh"
    & wsl.exe -d $WslDistro -- bash -lc "command -v go >/dev/null 2>&1" 2>$null | Out-Null
    $wslHasGo = $LASTEXITCODE -eq 0
    $wslTestBinPath = ""
    if (-not $wslHasGo) {
        if (-not $SkipRace) {
            throw "WSL Go is unavailable; install Go $expectedGoVersion or rerun with -SkipRace for the functional fallback"
        }
        $wslTestBinDir = Build-WslTestBinaries
        $wslTestBinPath = (& wsl.exe -d $WslDistro -- wslpath -a ($wslTestBinDir -replace "\\", "/")).Trim()
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($wslTestBinPath)) {
            throw "Unable to resolve WSL fallback test binary path"
        }
    }
    $skipRaceValue = if ($SkipRace) { "1" } else { "0" }
    Invoke-Logged "WSL full verification ($WslDistro)" "wsl.exe" @(
        "-d", $WslDistro, "--", "env",
        "ASTERFERRY_WSL_ROOT=$wslRoot",
        "ASTERFERRY_TEST_OUTPUT_DIR=$wslOutputDir",
        "ASTERFERRY_EXPECTED_GO_VERSION=$expectedGoVersion",
        "ASTERFERRY_SKIP_RACE=$skipRaceValue",
        "ASTERFERRY_WSL_TEST_BIN_DIR=$wslTestBinPath",
        "bash", $wslScript
    )

    $shortCommit = (& git rev-parse --short HEAD).Trim()
    $ociPath = Join-Path $outputDir "asterferry-$shortCommit.oci.tar"
    $builderName = "asterferry-test-$shortCommit-$(Get-Date -Format 'HHmmss')"
    $builderCreated = $false
    try {
        Invoke-Logged "Docker Buildx version" "docker" @("buildx", "version")
        Invoke-Logged "Docker create temporary builder" "docker" @("buildx", "create", "--name", $builderName, "--driver", "docker-container")
        $builderCreated = $true
        Invoke-Logged "Docker bootstrap temporary builder" "docker" @("buildx", "inspect", "--builder", $builderName, "--bootstrap")
        Invoke-Logged "Docker multi-platform build" "docker" @(
            "buildx", "build",
            "--builder", $builderName,
            "--platform", "linux/amd64,linux/arm64",
            "--network", "host",
            "--progress", "plain",
            "--output", "type=oci,dest=$ociPath",
            "."
        )
    } finally {
        if ($builderCreated) {
            Write-Host "== Docker remove temporary builder =="
            $oldErrorAction = $ErrorActionPreference
            try {
                $ErrorActionPreference = "Continue"
                & docker buildx rm --force $builderName 2>$null | Tee-Object -FilePath $logPath -Append | Out-Host
            } finally {
                $ErrorActionPreference = $oldErrorAction
            }
        }
    }

    Invoke-Logged "Helm lint gateway" "helm" @("lint", "deploy/helm/asterferry", "--set", "role=gateway")
    Invoke-Logged "Helm lint agent" "helm" @("lint", "deploy/helm/asterferry", "--set", "role=agent")
    Invoke-LoggedToFile "Helm template gateway" "helm" @("template", "asterferry-gateway", "deploy/helm/asterferry", "--set", "role=gateway") (Join-Path $outputDir "gateway.yaml")
    Invoke-LoggedToFile "Helm template agent" "helm" @("template", "asterferry-agent", "deploy/helm/asterferry", "--set", "role=agent") (Join-Path $outputDir "agent.yaml")

    if ($FullBench) {
        $oldDistro = $env:ASTERFERRY_WSL_DISTRO
        try {
            $env:ASTERFERRY_WSL_DISTRO = $WslDistro
            Invoke-Logged "Windows full benchmark matrix" "powershell.exe" @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", (Join-Path $root "scripts/bench-windows.ps1"))
            Invoke-Logged "WSL full benchmark matrix" "powershell.exe" @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", (Join-Path $root "scripts/bench-wsl.ps1"))
        } finally {
            if ($null -eq $oldDistro) { Remove-Item Env:ASTERFERRY_WSL_DISTRO -ErrorAction SilentlyContinue } else { $env:ASTERFERRY_WSL_DISTRO = $oldDistro }
        }
    }

    Write-Host "Local full verification passed"
    if (-not $FullBench) {
        Write-Host "Performance matrices were not run; re-run with -FullBench when needed."
    }
    if ($SkipRace) {
        Write-Host "Race tests were skipped; install a CGO-enabled Go toolchain before claiming strict full verification."
    }
} catch {
    Write-Error $_
    exit 1
}
