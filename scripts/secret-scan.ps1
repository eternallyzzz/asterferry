[CmdletBinding()]
param(
    [switch]$Staged
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root
$arguments = @()
if ($Staged) { $arguments += "--staged" }
& python (Join-Path $PSScriptRoot "secret-scan.py") @arguments
if ($LASTEXITCODE -ne 0) {
    throw "secret scan failed with exit code $LASTEXITCODE"
}
