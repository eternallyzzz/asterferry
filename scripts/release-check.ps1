[CmdletBinding()]
param(
    [string]$Version = "1.0.0",
    [string]$OutputDirectory = "tmp/release-check",
    [switch]$SkipDocker
)

$ErrorActionPreference = "Stop"

$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root

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
$stableVersion = [regex]::Match($Version, '^[0-9]+\.[0-9]+\.[0-9]+').Value

Require-Command "go"
Require-Command "node"
Require-Command "npm"
Require-Command "helm"
Require-Command "python"
if (-not $SkipDocker) {
    Require-Command "docker"
}
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

$versionSources = [ordered]@{}
$dashboardPackage = Get-Content -Raw -LiteralPath (Join-Path $root "web/dashboard/package.json") | ConvertFrom-Json
$dashboardLockText = Get-Content -Raw -LiteralPath (Join-Path $root "web/dashboard/package-lock.json")
$dashboardLockVersion = [regex]::Match($dashboardLockText, '(?m)^  "version":\s*"([^"]+)"\s*,?\r?$')
if (-not $dashboardLockVersion.Success) {
    throw "web/dashboard/package-lock.json does not declare a top-level version"
}
$versionSources["web/dashboard/package.json"] = [string]$dashboardPackage.version
$versionSources["web/dashboard/package-lock.json"] = $dashboardLockVersion.Groups[1].Value

foreach ($openapi in @("api/openapi.yaml", "internal/controller/openapi.yaml")) {
    $openapiText = Get-Content -Raw -LiteralPath (Join-Path $root $openapi)
    $openapiVersion = [regex]::Match($openapiText, '(?m)^  version:\s*(\S+)\r?$')
    if (-not $openapiVersion.Success) {
        throw "$openapi does not declare info.version"
    }
    $versionSources[$openapi] = $openapiVersion.Groups[1].Value
}

$chartPaths = @("deploy/helm/asterferry-controller", "deploy/helm/asterferry-node")
foreach ($chart in $chartPaths) {
    $chartPath = Join-Path $root "$chart/Chart.yaml"
    $chartText = Get-Content -Raw -LiteralPath $chartPath
    $chartVersion = [regex]::Match($chartText, '(?m)^version:\s*([^\s]+)').Groups[1].Value
    $appVersion = [regex]::Match($chartText, '(?m)^appVersion:\s*["'']?([^"''\s]+)').Groups[1].Value
    if ($chartVersion -ne $stableVersion -or $appVersion -ne $stableVersion) {
        throw "$chart metadata must match $stableVersion; got version=$chartVersion appVersion=$appVersion"
    }
    $versionSources["$chart/Chart.yaml version"] = $chartVersion
    $versionSources["$chart/Chart.yaml appVersion"] = $appVersion
}

$changelog = Get-Content -Raw -LiteralPath (Join-Path $root "CHANGELOG.md")
$changelogPattern = "(?m)^## \[$([regex]::Escape($stableVersion))\] - (?:Unreleased|\d{4}-\d{2}-\d{2})\r?$"
if (-not [regex]::IsMatch($changelog, $changelogPattern)) {
    throw "CHANGELOG.md has no entry for $stableVersion"
}

$mismatches = @($versionSources.GetEnumerator() | Where-Object { $_.Value -ne $stableVersion })
if ($mismatches.Count -gt 0) {
    $details = ($mismatches | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ", "
    throw "release version $stableVersion is inconsistent: $details"
}
$distinctVersions = @($versionSources.GetEnumerator() | ForEach-Object { $_.Value } | Select-Object -Unique)
if ($distinctVersions.Count -ne 1) {
    throw "release version sources disagree: $($versionSources | Out-String)"
}

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
    Invoke-Checked "Dashboard dependencies" "npm" @("--prefix", $frontendTemp, "ci")
    Invoke-Checked "Dashboard build" "npm" @("--prefix", $frontendTemp, "run", "build")
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
Invoke-Checked "Protocol benchmark smoke" "go" @("test", "./internal/afdp", "./internal/controlwire", "./internal/dataplane", "-run", "^$", "-bench", "^Benchmark", "-benchmem", "-benchtime=1s", "-count=3")

$ldflags = "-s -w -X asterferry/internal/buildinfo.Version=$Version -X asterferry/internal/buildinfo.Commit=release-check -X asterferry/internal/buildinfo.BuildDate=release-check"
$binaryPath = Join-Path $output "asterferry.exe"
Invoke-Checked "Windows amd64 binary" "go" @("build", "-tags=dashboard_assets", "-trimpath", "-ldflags=$ldflags", "-o", $binaryPath, "./cmd/asterferry")
$versionOutput = (& $binaryPath version | Out-String)
if ($LASTEXITCODE -ne 0 -or $versionOutput -notmatch "asterferry $Version" -or $versionOutput -notmatch "protocol: AFDP/2 \+ control/2") {
	throw "Release binary did not report version $Version and AFDP/2: $versionOutput"
}

Invoke-Checked "Helm lint Controller" "helm" @("lint", "deploy/helm/asterferry-controller")
Invoke-Checked "Helm lint Node" "helm" @("lint", "deploy/helm/asterferry-node")
$nodeTemplate = (& helm template release-check deploy/helm/asterferry-node --set image.tag=$Version | Out-String)
$expectedImage = "ghcr.io/eternallyzzz/asterferry:$Version"
if ($LASTEXITCODE -ne 0 -or $nodeTemplate -notmatch [regex]::Escape($expectedImage)) {
    throw "Default Helm image reference did not resolve to the release version"
}
$controllerMetricsDisabledTemplate = (& helm template release-check deploy/helm/asterferry-controller | Out-String)
if ($LASTEXITCODE -ne 0 -or $controllerMetricsDisabledTemplate -notmatch "--metrics-listen" -or $controllerMetricsDisabledTemplate -notmatch 'metrics-listen\r?\n\s+- ""') {
    throw "Helm default metrics policy must explicitly disable the internal listener"
}
$digest = "sha256:" + ("a" * 64)
foreach ($chart in $chartPaths) {
    $digestTemplate = (& helm template release-check $chart --set image.digest=$digest | Out-String)
    if ($LASTEXITCODE -ne 0 -or $digestTemplate -notmatch [regex]::Escape("ghcr.io/eternallyzzz/asterferry@$digest")) {
        throw "Helm digest image override did not render correctly for $chart"
    }
}
$controllerMetricsTemplate = (& helm template release-check deploy/helm/asterferry-controller --set metrics.enabled=true --set metrics.listen=:9090 | Out-String)
if ($LASTEXITCODE -ne 0 -or $controllerMetricsTemplate -notmatch "--metrics-listen" -or $controllerMetricsTemplate -notmatch "name: metrics") {
    throw "Helm metrics opt-in did not render the dedicated metrics listener and Service port"
}
$nodeGeoIPTemplate = (& helm template release-check deploy/helm/asterferry-node --set geoip.enabled=true --set geoip.existingConfigMap=geoip-data | Out-String)
if ($LASTEXITCODE -ne 0 -or $nodeGeoIPTemplate -notmatch "--geoip-db" -or $nodeGeoIPTemplate -notmatch "name: geoip") {
    throw "Helm GeoIP opt-in did not render the external database mount"
}
foreach ($chart in $chartPaths) {
    Invoke-Checked "Helm package $chart" "helm" @("package", $chart, "--destination", $output, "--version", $Version, "--app-version", $Version)
}
$expectedCharts = @("asterferry-controller-$Version.tgz", "asterferry-node-$Version.tgz")
foreach ($chartFile in $expectedCharts) {
    $chartArtifact = Join-Path $output $chartFile
    if (-not (Test-Path -LiteralPath $chartArtifact) -or (Get-Item -LiteralPath $chartArtifact).Length -eq 0) {
        throw "release chart artifact is missing or empty: $chartFile"
    }
}
$actualCharts = @(Get-ChildItem -LiteralPath $output -Filter "*.tgz" -File | Select-Object -ExpandProperty Name | Sort-Object)
if (($actualCharts -join ";") -ne (($expectedCharts | Sort-Object) -join ";")) {
    throw "unexpected release chart artifacts: $($actualCharts -join ', ')"
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
    protocol = "AFDP/2 + control/2"
    windows_binary = $binaryPath
    charts = $expectedCharts
    docker_checked = (-not $SkipDocker)
}
$report | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $output "report.json")
Write-Host "Release preflight passed for $Version (AFDP/2 + control/2)"
