[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $root
& python (Join-Path $PSScriptRoot "secret-scan.py")
if ($LASTEXITCODE -ne 0) {
    throw "secret scan failed with exit code $LASTEXITCODE"
}
