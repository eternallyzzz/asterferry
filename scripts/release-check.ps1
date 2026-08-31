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

if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') {
    throw "Version must be MAJOR.MINOR.PATCH without the leading v"
}

Require-Command "go"
Require-Command "node"
Require-Command "npm"
Require-Command "helm"
if (-not $SkipDocker) {
    Require-Command "docker"
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

$chartPaths = @("deploy/helm/asterferry-controller", "deploy/helm/asterferry-node")
foreach ($chart in $chartPaths) {
    $chartPath = Join-Path $root "$chart/Chart.yaml"
    $chartText = Get-Content -Raw -LiteralPath $chartPath
    $chartVersion = [regex]::Match($chartText, '(?m)^version:\s*([^\s]+)').Groups[1].Value
    $appVersion = [regex]::Match($chartText, '(?m)^appVersion:\s*["'']?([^"''\s]+)').Groups[1].Value
    if ($chartVersion -ne $Version -or $appVersion -ne $Version) {
        throw "$chart metadata must match $Version; got version=$chartVersion appVersion=$appVersion"
    }
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
Invoke-Checked "Generated dashboard check" "git" @("diff", "--exit-code", "--", "internal/dashboard/dist")
Invoke-Checked "Go module verification" "go" @("mod", "verify")

$ldflags = "-s -w -X asterferry/internal/buildinfo.Version=$Version -X asterferry/internal/buildinfo.Commit=release-check -X asterferry/internal/buildinfo.BuildDate=release-check"
$binaryPath = Join-Path $output "asterferry.exe"
Invoke-Checked "Windows amd64 binary" "go" @("build", "-trimpath", "-ldflags=$ldflags", "-o", $binaryPath, "./cmd/asterferry")
$versionOutput = (& $binaryPath version | Out-String)
if ($LASTEXITCODE -ne 0 -or $versionOutput -notmatch "asterferry $Version" -or $versionOutput -notmatch "protocol: AFDP/2 \+ control/1") {
	throw "Release binary did not report version $Version and AFDP/2: $versionOutput"
}

Invoke-Checked "Helm lint Controller" "helm" @("lint", "deploy/helm/asterferry-controller")
Invoke-Checked "Helm lint Gateway node" "helm" @("lint", "deploy/helm/asterferry-node", "--set", "role=gateway")
Invoke-Checked "Helm lint Agent node" "helm" @("lint", "deploy/helm/asterferry-node", "--set", "role=agent")
$gatewayTemplate = (& helm template release-check deploy/helm/asterferry-node --set role=gateway | Out-String)
$expectedImage = "ghcr.io/eternallyzzz/asterferry:$Version"
if ($LASTEXITCODE -ne 0 -or $gatewayTemplate -notmatch [regex]::Escape($expectedImage)) {
    throw "Default Helm image reference did not resolve to the release version"
}
$digest = "sha256:" + ("a" * 64)
$digestTemplate = (& helm template release-check deploy/helm/asterferry-node --set role=gateway --set image.digest=$digest | Out-String)
if ($LASTEXITCODE -ne 0 -or $digestTemplate -notmatch "ghcr\.io/eternallyzzz/asterferry@$digest") {
    throw "Helm digest image override did not render correctly"
}
foreach ($chart in $chartPaths) {
    Invoke-Checked "Helm package $chart" "helm" @("package", $chart, "--destination", $output, "--version", $Version, "--app-version", $Version)
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
    protocol = "AFDP/2 + control/1"
    windows_binary = $binaryPath
    charts = @("asterferry-controller-$Version.tgz", "asterferry-node-$Version.tgz")
    docker_checked = (-not $SkipDocker)
}
$report | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $output "report.json")
Write-Host "Release preflight passed for $Version (AFDP/2 + control/1)"
