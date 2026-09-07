[CmdletBinding()]
param(
    [string]$Version = "",
    [string]$OutputDirectory = "tmp/release-check",
    [switch]$SkipDocker
)

$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root

if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = (Get-Content -Raw -LiteralPath (Join-Path $root "VERSION")).Trim()
}

function Require-Command([string]$Name) {
    if ($null -eq (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command not found: $Name"
    }
}

function Invoke-Checked([string]$Name, [string]$File, [string[]]$Arguments) {
    Write-Host "== $Name =="
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Name failed with exit code $LASTEXITCODE"
    }
}

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
    $path = Join-Path ([System.IO.Path]::GetTempPath()) ("asterferry-dashboard-release-" + [guid]::NewGuid().ToString("N"))
    $null = New-Item -ItemType Directory -Force -Path $path
    Get-ChildItem -LiteralPath (Join-Path $root "web/dashboard") -Force |
        Where-Object { $_.Name -ne "node_modules" } |
        Copy-Item -Destination $path -Recurse -Force
    return $path
}

if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?$') {
    throw "Version must be MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH-rc.N without the leading v"
}
Require-Command "go"
Require-Command "node"
Require-Command "npm"
Require-Command "helm"
Require-Command "python"
if (-not $SkipDocker) {
    Require-Command "docker"
}
Invoke-Checked "Release metadata check" "python" @((Join-Path $root "scripts/check-release-metadata.py"), "--version", $Version)
Invoke-Checked "Release verification key" "python" @((Join-Path $root "scripts/check-release-public-key.py"))
Invoke-Checked "Toolchain pin check" "python" @((Join-Path $root "scripts/check-toolchain.py"))
$toolchain = Get-Content -Raw -LiteralPath (Join-Path $root ".toolchain.json") | ConvertFrom-Json
$expectedNodeVersion = "v$($toolchain.release.node)"
$nodeVersion = (& node --version).Trim()
if ($LASTEXITCODE -ne 0 -or $nodeVersion -ne $expectedNodeVersion) {
    throw "Expected Node $expectedNodeVersion, got: $nodeVersion"
}
$expectedNpmVersion = [string]$toolchain.release.npm
$npmVersion = (& npm --version).Trim()
if ($LASTEXITCODE -ne 0 -or $npmVersion -ne $expectedNpmVersion) {
    throw "Expected npm $expectedNpmVersion, got: $npmVersion"
}

$tmpRoot = [System.IO.Path]::GetFullPath((Join-Path $root "tmp"))
$output = [System.IO.Path]::GetFullPath((Join-Path $root $OutputDirectory))
if (-not $output.StartsWith($tmpRoot + [System.IO.Path]::DirectorySeparatorChar, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputDirectory must stay under $tmpRoot"
}
if (Test-Path -LiteralPath $output) {
    Remove-Item -LiteralPath $output -Recurse -Force
}
$null = New-Item -ItemType Directory -Force -Path $output
Remove-FrontendScratch

Invoke-Checked "OpenAPI generated copy" "python" @("scripts/sync-openapi.py", "--check")
Invoke-Checked "Source layout check" "python" @((Join-Path $root "scripts/check-source-layout.py"))
Invoke-Checked "Tracked-file secret scan" "python" @((Join-Path $root "scripts/secret-scan.py"))
if (Test-Path -LiteralPath (Join-Path $root "internal/dataplane/cn.mmdb")) {
    throw "GeoIP database must be supplied as an external, versioned release resource; it must not be tracked in source"
}

Invoke-Checked "Go module tidy check" "go" @("mod", "tidy", "-diff")
$frontendTemp = Prepare-FrontendScratch
$oldDashboardOut = $env:ASTERFERRY_DASHBOARD_OUT
try {
    $env:ASTERFERRY_DASHBOARD_OUT = (Join-Path $root "internal/dashboard/dist")
    Invoke-Checked "Dashboard dependencies" "npm" @("--prefix", $frontendTemp, "ci", "--audit=false", "--registry=https://registry.npmjs.org", "--replace-registry-host=always")
    Invoke-Checked "Dashboard build" "npm" @("--prefix", $frontendTemp, "run", "build")
    Invoke-Checked "Dashboard dependency audit" "npm" @("--prefix", $frontendTemp, "audit", "--registry=https://registry.npmjs.org", "--audit-level=high")
} finally {
    if ($null -eq $oldDashboardOut) { Remove-Item Env:ASTERFERRY_DASHBOARD_OUT -ErrorAction SilentlyContinue } else { $env:ASTERFERRY_DASHBOARD_OUT = $oldDashboardOut }
    if (Test-Path -LiteralPath $frontendTemp) { Remove-Item -LiteralPath $frontendTemp -Recurse -Force -ErrorAction SilentlyContinue }
}
$dashboardAssetIndex = Join-Path $root "internal/dashboard/dist/index.html"
if (-not (Test-Path -LiteralPath $dashboardAssetIndex)) {
    throw "Dashboard build did not produce internal/dashboard/dist/index.html"
}
if ((Get-Item -LiteralPath $dashboardAssetIndex).Length -eq 0) {
    throw "Dashboard build produced an empty internal/dashboard/dist/index.html"
}
$trackedDashboardAssets = @(git ls-files -- internal/dashboard/dist)
if ($trackedDashboardAssets.Count -gt 0) {
    throw "generated Dashboard assets must not be tracked: $($trackedDashboardAssets -join ', ')"
}
Invoke-Checked "Go module verification" "go" @("mod", "verify")
Invoke-Checked "Behavior contract and state-machine tests" "go" @("test", "-count=1", "./internal/afdp", "./internal/controller", "./internal/dataplane", "./internal/duplex", "./internal/node", "-run", "Contract|StateMachine")
Invoke-Checked "Protocol benchmark smoke" "go" @("test", "./internal/afdp", "./internal/controlwire", "./internal/dataplane", "-run", "^$", "-bench", "^Benchmark", "-benchmem", "-benchtime=1s", "-count=3")

$ldflags = "-s -w -X asterferry/internal/buildinfo.Version=$Version -X asterferry/internal/buildinfo.Commit=release-check -X asterferry/internal/buildinfo.BuildDate=release-check"
$binaryPath = Join-Path $output "asterferry.exe"
Invoke-Checked "Windows amd64 binary" "go" @("build", "-tags=dashboard_assets", "-trimpath", "-ldflags=$ldflags", "-o", $binaryPath, "./cmd/asterferry")
$versionOutput = (& $binaryPath version | Out-String)
if ($LASTEXITCODE -ne 0 -or $versionOutput -notmatch "asterferry $Version" -or $versionOutput -notmatch "protocol: AFDP/2 \+ control/3") {
	throw "Release binary did not report version $Version and AFDP/2: $versionOutput"
}

$helmImageArgs = @("--set", "image.repository=asterferry", "--set", "image.tag=$Version")
Invoke-Checked "Helm lint Controller" "helm" (@("lint", "deploy/helm/asterferry-controller") + $helmImageArgs)
Invoke-Checked "Helm lint Node" "helm" (@("lint", "deploy/helm/asterferry-node") + $helmImageArgs)
$nodeTemplate = (& helm template release-check deploy/helm/asterferry-node @helmImageArgs | Out-String)
$expectedImage = "asterferry:$Version"
if ($LASTEXITCODE -ne 0 -or $nodeTemplate -notmatch [regex]::Escape($expectedImage)) {
    throw "Operator-provided Helm image reference did not resolve to the release version"
}
$missingImageTemplate = (& helm template release-check deploy/helm/asterferry-node 2>$null | Out-String)
if ($LASTEXITCODE -eq 0) {
    throw "Helm templates must reject an unset image.repository"
}
$controllerMetricsDisabledTemplate = (& helm template release-check deploy/helm/asterferry-controller @helmImageArgs | Out-String)
if ($LASTEXITCODE -ne 0 -or $controllerMetricsDisabledTemplate -notmatch "--metrics-listen" -or $controllerMetricsDisabledTemplate -notmatch 'metrics-listen\r?\n\s+- ""') {
    throw "Helm default metrics policy must explicitly disable the internal listener"
}
$controllerHAArgs = @("template", "release-check", "deploy/helm/asterferry-controller") + $helmImageArgs + @("--set", "controller.highAvailability.enabled=true", "--set", "controller.replicas=2", "--set", "controller.highAvailability.existingSecret=asterferry-controller-identity")
$controllerHATemplate = (& helm @controllerHAArgs | Out-String)
if ($LASTEXITCODE -ne 0 -or $controllerHATemplate -notmatch "replicas: 2" -or $controllerHATemplate -notmatch "clusterIP: None" -or $controllerHATemplate -notmatch "path: /readyz" -or $controllerHATemplate -notmatch "secretName: asterferry-controller-identity" -or $controllerHATemplate -notmatch "fsGroup: 10001" -or $controllerHATemplate -notmatch "defaultMode: 0440" -or $controllerHATemplate -match "volumeClaimTemplates:") {
    throw "Helm Controller HA template did not render the two-replica, readiness-gated, Secret-backed deployment"
}
$controllerHAInvalidArgs = @("template", "release-check", "deploy/helm/asterferry-controller") + $helmImageArgs + @("--set", "controller.highAvailability.enabled=true", "--set", "controller.highAvailability.existingSecret=asterferry-controller-identity")
$controllerHAInvalidTemplate = (& helm @controllerHAInvalidArgs 2>$null | Out-String)
if ($LASTEXITCODE -eq 0) {
    throw "Helm Controller HA template must reject replicas other than two"
}
$digest = "sha256:" + ("a" * 64)
foreach ($chart in $chartPaths) {
    $digestArgs = @("template", "release-check", $chart) + $helmImageArgs + @("--set", "image.digest=$digest")
    $digestTemplate = (& helm @digestArgs | Out-String)
    if ($LASTEXITCODE -ne 0 -or $digestTemplate -notmatch [regex]::Escape("asterferry@$digest")) {
        throw "Helm digest image override did not render correctly for $chart"
    }
}
$controllerMetricsArgs = @("template", "release-check", "deploy/helm/asterferry-controller") + $helmImageArgs + @("--set", "metrics.enabled=true", "--set", "metrics.listen=:9090")
$controllerMetricsTemplate = (& helm @controllerMetricsArgs | Out-String)
if ($LASTEXITCODE -ne 0 -or $controllerMetricsTemplate -notmatch "--metrics-listen" -or $controllerMetricsTemplate -notmatch "name: metrics") {
    throw "Helm metrics opt-in did not render the dedicated metrics listener and Service port"
}
$nodeGeoIPArgs = @("template", "release-check", "deploy/helm/asterferry-node") + $helmImageArgs + @("--set", "geoip.enabled=true", "--set", "geoip.existingConfigMap=geoip-data")
$nodeGeoIPTemplate = (& helm @nodeGeoIPArgs | Out-String)
if ($LASTEXITCODE -ne 0 -or $nodeGeoIPTemplate -notmatch "--geoip-db" -or $nodeGeoIPTemplate -notmatch "name: geoip") {
    throw "Helm GeoIP opt-in did not render the external database mount"
}

if (-not $SkipDocker) {
    $image = "asterferry:release-check-$Version"
    Invoke-Checked "Docker amd64 release image" "docker" @("build", "--platform", "linux/amd64", "--build-arg", "VERSION=$Version", "--build-arg", "COMMIT=release-check", "--build-arg", "BUILD_DATE=release-check", "-t", $image, ".")
    $containerVersion = (& docker run --rm $image version --short | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $containerVersion -ne $Version) {
        throw "Container version was '$containerVersion', expected '$Version'"
    }
    $user = (& docker image inspect --format '{{.Config.User}}' $image).Trim()
    if ($user -ne "10001:10001") {
        throw "Release image must run as 10001:10001, got '$user'"
    }
}

$report = [ordered]@{
    version = $Version
    protocol = "AFDP/2 + control/3"
    windows_binary = $binaryPath
    native_only = $true
    helm_validated = $true
    docker_checked = (-not $SkipDocker)
}
$report | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $output "report.json")
Write-Host "Release preflight passed for $Version (AFDP/2 + control/3)"
