[CmdletBinding()]
param(
    [string]$KeyPath = ""
)

$ErrorActionPreference = "Stop"
$scriptRoot = (Resolve-Path $PSScriptRoot).Path
if ([string]::IsNullOrWhiteSpace($KeyPath)) {
    $KeyPath = Join-Path $scriptRoot "../internal/update/release-public-key.pem"
}
$python = Get-Command python -ErrorAction SilentlyContinue
if ($null -eq $python) {
    throw "Required command not found: python"
}

$arguments = @((Join-Path $scriptRoot "check-release-public-key.py"), "--key", $KeyPath)
if (-not [string]::IsNullOrWhiteSpace($env:ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256)) {
    $arguments += @("--expected-fingerprint", $env:ASTERFERRY_RELEASE_PUBLIC_KEY_SHA256)
}
& $python.Path @arguments
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
