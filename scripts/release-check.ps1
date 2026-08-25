[CmdletBinding()]
param(
    [string]$Version = "0.1.0",
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

$chartPath = Join-Path $root "deploy/helm/asterferry/Chart.yaml"
$chartText = Get-Content -Raw -LiteralPath $chartPath
$chartVersion = [regex]::Match($chartText, '(?m)^version:\s*([^\s]+)').Groups[1].Value
$appVersion = [regex]::Match($chartText, '(?m)^appVersion:\s*["'']?([^"''\s]+)').Groups[1].Value
if ($chartVersion -ne $Version -or $appVersion -ne $Version) {
    throw "Chart metadata must match $Version; got version=$chartVersion appVersion=$appVersion"
}

Invoke-Checked "Dashboard dependencies" "npm" @("--prefix", "web/dashboard", "ci")
Invoke-Checked "Dashboard build" "npm" @("--prefix", "web/dashboard", "run", "build")
Invoke-Checked "Generated dashboard check" "git" @("diff", "--exit-code", "--", "internal/dashboard/dist")
Invoke-Checked "Go module verification" "go" @("mod", "verify")

$ldflags = "-s -w -X asterferry/internal/buildinfo.Version=$Version -X asterferry/internal/buildinfo.Commit=release-check -X asterferry/internal/buildinfo.BuildDate=release-check"
$binaryPath = Join-Path $output "asterferry.exe"
Invoke-Checked "Windows amd64 binary" "go" @("build", "-trimpath", "-ldflags=$ldflags", "-o", $binaryPath, "./cmd/asterferry")
$versionOutput = (& $binaryPath version | Out-String)
if ($LASTEXITCODE -ne 0 -or $versionOutput -notmatch "asterferry $Version" -or $versionOutput -notmatch "protocol: v5") {
    throw "Release binary did not report version $Version and protocol v5: $versionOutput"
}

Invoke-Checked "Helm lint gateway" "helm" @("lint", "deploy/helm/asterferry", "--set", "role=gateway")
Invoke-Checked "Helm lint agent" "helm" @("lint", "deploy/helm/asterferry", "--set", "role=agent")
$gatewayTemplate = (& helm template release-check deploy/helm/asterferry --set role=gateway | Out-String)
$expectedImage = "ghcr.io/eternallyzzz/asterferry:$Version"
if ($LASTEXITCODE -ne 0 -or $gatewayTemplate -notmatch [regex]::Escape($expectedImage)) {
    throw "Default Helm image reference did not resolve to the release version"
}
$digest = "sha256:" + ("a" * 64)
$digestTemplate = (& helm template release-check deploy/helm/asterferry --set role=gateway --set image.digest=$digest | Out-String)
if ($LASTEXITCODE -ne 0 -or $digestTemplate -notmatch "ghcr\.io/eternallyzzz/asterferry@$digest") {
    throw "Helm digest image override did not render correctly"
}
Invoke-Checked "Helm package" "helm" @("package", "deploy/helm/asterferry", "--destination", $output, "--version", $Version, "--app-version", $Version)

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
    protocol = 5
    windows_binary = $binaryPath
    chart = Join-Path $output "asterferry-$Version.tgz"
    docker_checked = (-not $SkipDocker)
}
$report | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $output "report.json")
Write-Host "Release preflight passed for v$Version"
