[CmdletBinding()]
param(
    [string]$Destination = ""
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$rootPrefix = $root.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
if ([string]::IsNullOrWhiteSpace($Destination)) {
    $parent = Split-Path -Parent $root
    $Destination = Join-Path $parent ("asterferry-local-quarantine-" + [DateTime]::Now.ToString("yyyyMMdd-HHmmss"))
}
$destinationFull = [IO.Path]::GetFullPath($Destination)
if ($destinationFull.Equals($root, [StringComparison]::OrdinalIgnoreCase) -or $destinationFull.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "quarantine destination must be outside the repository: $destinationFull"
}
if (Test-Path -LiteralPath $destinationFull) {
    throw "quarantine destination already exists: $destinationFull"
}

$targetNames = @("tmp", "dist", "controller", "internal/dashboard/dist", "asterferry.exe", "integration.test.exe")
$targets = @()
foreach ($name in $targetNames) {
    $path = Join-Path $root $name
    if (-not (Test-Path -LiteralPath $path)) { continue }
    $resolved = (Resolve-Path -LiteralPath $path).Path
    if (-not $resolved.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "refuse to move path outside the repository: $resolved"
    }
    $targets += $resolved
}
if ($targets.Count -eq 0) {
    Write-Host "No generated local state found."
    exit 0
}

$null = New-Item -ItemType Directory -Path $destinationFull
foreach ($target in $targets) {
    Move-Item -LiteralPath $target -Destination $destinationFull
    Write-Host "Moved $target to $destinationFull"
}
Write-Host "Local generated state quarantined outside the repository. It is recoverable from $destinationFull."
