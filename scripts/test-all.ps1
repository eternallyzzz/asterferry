[CmdletBinding()]
param(
    [string]$WslDistro = "Debian",
    [switch]$FullBench,
    [switch]$SkipRace
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
$outputDir = Join-Path $root "tmp/test/windows"
$null = New-Item -ItemType Directory -Force -Path $outputDir
$logPath = Join-Path $outputDir "test.log"
$frontendTemp = $null

function Remove-FrontendScratch {
    $scratchRoot = Join-Path $root "tmp"
    if (-not (Test-Path -LiteralPath $scratchRoot)) { return }
    $targets = @(Get-ChildItem -LiteralPath $scratchRoot -Directory -Force -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -like "release-check-frontend-*" -or $_.Name -like "release-check-worktree-*" -or $_.Name -like "web-dashboard-check-*" })
    foreach ($target in $targets) {
        $targetPath = (Resolve-Path -LiteralPath $target.FullName).Path
        if (-not $targetPath.StartsWith($scratchRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
            throw "refuse to remove frontend scratch outside tmp: $targetPath"
        }
        Get-ChildItem -LiteralPath $targetPath -Recurse -Force -ErrorAction SilentlyContinue | ForEach-Object {
            if ($_.Attributes -band [IO.FileAttributes]::ReadOnly) {
                $_.Attributes = $_.Attributes -bxor [IO.FileAttributes]::ReadOnly
            }
        }
        Remove-Item -LiteralPath $targetPath -Recurse -Force -ErrorAction Stop
        if (Test-Path -LiteralPath $targetPath) {
            throw "frontend scratch cleanup did not remove $targetPath"
        }
    }
}

function Prepare-FrontendScratch {
    $script:frontendTemp = Join-Path ([System.IO.Path]::GetTempPath()) ("asterferry-dashboard-" + [guid]::NewGuid().ToString("N"))
    $null = New-Item -ItemType Directory -Force -Path $script:frontendTemp
    Get-ChildItem -LiteralPath (Join-Path $root "web/dashboard") -Force |
        Where-Object { $_.Name -ne "node_modules" } |
        Copy-Item -Destination $script:frontendTemp -Recurse -Force
}

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
        Invoke-Logged "build $GoOS/$GoArch asterferry" "go" @("build", "-tags=dashboard_assets", "-trimpath", "-o", $asterferryPath, "./cmd/asterferry")
        Invoke-Logged "build $GoOS/$GoArch benchmark" "go" @("build", "-tags=dashboard_assets", "-trimpath", "-o", $benchmarkPath, "./cmd/asterferry-bench")
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
    Remove-FrontendScratch
    Require-Command "go"
    Require-Command "gofmt"
    Require-Command "python"
    Require-Command "node"
    Require-Command "npm"
    Require-Command "wsl.exe"
    Require-Command "docker"
    Require-Command "helm"
    Require-Command "staticcheck"
    Require-Command "govulncheck"

    $goVersion = (& go version).Trim()
    if ($goVersion -notlike "*$expectedGoVersion*") {
        throw "Expected Go $expectedGoVersion, got: $goVersion"
    }
    $nodeVersion = (& node --version).Trim()
    $expectedNodeVersion = "v$($toolchain.release.node)"
    if ($nodeVersion -ne $expectedNodeVersion) {
        throw "Expected Node $expectedNodeVersion, got: $nodeVersion"
    }
    $npmVersion = (& npm --version).Trim()
    if ($npmVersion -ne [string]$toolchain.release.npm) {
        throw "Expected npm $($toolchain.release.npm), got: $npmVersion"
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

    Invoke-Logged "Source layout check" "python" @((Join-Path $root "scripts/check-source-layout.py"))
    Invoke-Logged "Toolchain pin check" "python" @((Join-Path $root "scripts/check-toolchain.py"))
    Invoke-Logged "Tracked-file secret scan" "python" @((Join-Path $root "scripts/secret-scan.py"))

    $commit = (& git rev-parse HEAD).Trim()
    $metadata = [ordered]@{
        timestamp_utc = [DateTime]::UtcNow.ToString("o")
        commit = $commit
        go = $goVersion
        goos = (& go env GOOS).Trim()
        goarch = (& go env GOARCH).Trim()
        gomaxprocs = (& go env GOMAXPROCS).Trim()
        node = $nodeVersion
        wsl_distro = $WslDistro
        full_bench = [bool]$FullBench
        skip_race = [bool]$SkipRace
    }
    $metadata | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $outputDir "metadata.json")

    Invoke-Logged "Go module tidy check" "go" @("mod", "tidy", "-diff")
    Prepare-FrontendScratch
    $oldDashboardOut = $env:ASTERFERRY_DASHBOARD_OUT
    try {
        $env:ASTERFERRY_DASHBOARD_OUT = (Join-Path $root "internal/dashboard/dist")
        Invoke-Logged "Dashboard npm dependencies" "npm" @("--prefix", $frontendTemp, "ci")
        Invoke-Logged "Dashboard type check" "npm" @("--prefix", $frontendTemp, "run", "lint")
        Invoke-Logged "Dashboard unit tests" "npm" @("--prefix", $frontendTemp, "test")
        Invoke-Logged "Dashboard production build" "npm" @("--prefix", $frontendTemp, "run", "build")
        Invoke-Logged "Dashboard dependency audit" "npm" @("--prefix", $frontendTemp, "audit", "--registry=https://registry.npmjs.org", "--omit=dev", "--audit-level=high")
    } finally {
        if ($null -eq $oldDashboardOut) { Remove-Item Env:ASTERFERRY_DASHBOARD_OUT -ErrorAction SilentlyContinue } else { $env:ASTERFERRY_DASHBOARD_OUT = $oldDashboardOut }
        if ($null -ne $frontendTemp -and (Test-Path -LiteralPath $frontendTemp)) { Remove-Item -LiteralPath $frontendTemp -Recurse -Force -ErrorAction SilentlyContinue }
        $frontendTemp = $null
    }
    $dashboardAssetIndex = Join-Path $root "internal/dashboard/dist/index.html"
    if (-not (Test-Path -LiteralPath $dashboardAssetIndex)) {
        throw "Dashboard build did not produce internal/dashboard/dist/index.html"
    }
    if ((Get-Item -LiteralPath $dashboardAssetIndex).Length -eq 0) {
        throw "Dashboard build produced an empty internal/dashboard/dist/index.html"
    }

    Invoke-Logged "Go module verification" "go" @("mod", "verify")
    Invoke-Logged "Windows go vet" "go" @("vet", "./...")
    Invoke-Logged "Windows staticcheck" "staticcheck" @("-checks=all,-SA1019", "./...")
    Invoke-Logged "Windows vulnerability check" "govulncheck" @("./...")
    Invoke-Logged "Windows full tests" "go" @("test", "-count=1", "./...")
    Invoke-Logged "Windows Controller/Gateway/Agent smoke test" "go" @("test", "-tags=integration", "-count=1", "-timeout=5m", "./internal/integration")
    Invoke-Logged "Windows AFDP decoder fuzz smoke" "go" @("test", "./internal/afdp", "-run", "^$", "-fuzz", "FuzzDecodeAFDPFrames", "-fuzztime", "10s")
    Invoke-Logged "Windows control-wire decoder fuzz smoke" "go" @("test", "./internal/controlwire", "-run", "^$", "-fuzz", "FuzzControlwireDecoders", "-fuzztime", "10s")
    $controllerSmoke = Join-Path $outputDir "controller-smoke"
    if (Test-Path -LiteralPath $controllerSmoke) {
        Remove-Item -LiteralPath $controllerSmoke -Recurse -Force
    }
    Invoke-Logged "CLI Controller init smoke test" "go" @("run", "./cmd/asterferry", "controller", "init", "--dir", $controllerSmoke, "--username", "smoke-admin", "--password", "smoke-password")
    Invoke-Logged "CLI AFDP/control version smoke test" "go" @("run", "./cmd/asterferry", "version")
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

    Invoke-Logged "Helm lint Controller" "helm" @("lint", "deploy/helm/asterferry-controller")
    Invoke-Logged "Helm lint generic Node" "helm" @("lint", "deploy/helm/asterferry-node")
    Invoke-LoggedToFile "Helm template Controller" "helm" @("template", "asterferry-controller", "deploy/helm/asterferry-controller") (Join-Path $outputDir "controller.yaml")
    Invoke-LoggedToFile "Helm template generic Node" "helm" @("template", "asterferry-node", "deploy/helm/asterferry-node") (Join-Path $outputDir "node.yaml")

    if ($FullBench) {
        $oldDistro = $env:ASTERFERRY_WSL_DISTRO
        try {
            $env:ASTERFERRY_WSL_DISTRO = $WslDistro
            Invoke-Logged "Windows full benchmark matrix" "powershell.exe" @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", (Join-Path $root "scripts/bench-windows.ps1"), "-FullMatrix")
            Invoke-Logged "WSL full benchmark matrix" "powershell.exe" @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", (Join-Path $root "scripts/bench-wsl.ps1"), "-FullMatrix")
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
    if ($null -ne $frontendTemp -and (Test-Path -LiteralPath $frontendTemp)) { Remove-Item -LiteralPath $frontendTemp -Recurse -Force -ErrorAction SilentlyContinue }
    Remove-FrontendScratch
    Write-Error $_
    exit 1
}
