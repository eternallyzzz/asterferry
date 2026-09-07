[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$outputDir = Join-Path $root "tmp/test/installers"
$null = New-Item -ItemType Directory -Force -Path $outputDir

function Require-Command([string]$Name) {
    if ($null -eq (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command not found: $Name"
    }
}

function Assert-Contains([string]$Path, [string]$Needle) {
    $text = Get-Content -Raw -LiteralPath $Path
    if (-not $text.Contains($Needle)) {
        throw "$Path does not contain required installer contract: $Needle"
    }
}

Require-Command "pwsh"
Require-Command "git"
$gitBash = Join-Path (Split-Path (Get-Command "git").Source) "..\bin\bash.exe"
if (-not (Test-Path -LiteralPath $gitBash -PathType Leaf)) {
    throw "Git Bash was not found beside the Git installation: $gitBash"
}
$gitBash = (Resolve-Path -LiteralPath $gitBash).Path

$shellInstallers = @(
    (Join-Path $root "scripts/install-controller.sh"),
    (Join-Path $root "scripts/install-node.sh")
)
foreach ($script in $shellInstallers) {
    & $gitBash -n ($script -replace "\\", "/")
    if ($LASTEXITCODE -ne 0) { throw "shell installer syntax check failed: $script" }
}

$parseErrors = $null
$tokens = $null
foreach ($script in @("scripts/install-controller.ps1", "scripts/install-node.ps1")) {
    $path = Join-Path $root $script
    [void][System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0) {
        throw "PowerShell installer syntax check failed: $path`n$($parseErrors -join "`n")"
    }
}

$controllerInstaller = Join-Path $root "scripts/install-controller.ps1"
$controllerText = Get-Content -Raw -LiteralPath $controllerInstaller
Assert-Contains $controllerInstaller '$script:embeddedReleaseBaseUrl = ""'
Assert-Contains $controllerInstaller '$script:embeddedReleaseVersion = ""'
Assert-Contains $controllerInstaller '0.0.0.0:8443'
Assert-Contains $controllerInstaller '0.0.0.0:9443'
foreach ($expectedPrompt in @(
    'Read-InstallerValue -Prompt "Release base URL',
    'Read-InstallerValue -Prompt "HTTPS listen address',
    'Read-InstallerValue -Prompt "gRPC listen address',
    'Read-InstallerValue -Prompt "Metrics listen address',
    'Read-InstallerValue -Prompt "Windows service name',
    'Read-InstallerValue -Prompt "Initial Admin username',
    'Read-InstallerValue -Prompt "Controller gRPC advertise address (required)'
)) {
    if (-not $controllerText.Contains($expectedPrompt)) {
        throw "$controllerInstaller is missing an interactive prompt: $expectedPrompt"
    }
}
Assert-Contains $controllerInstaller 'DefaultLabel "empty for latest stable"'
Assert-Contains $controllerInstaller 'DefaultLabel "empty to generate random password"'
Assert-Contains $controllerInstaller 'Get-DefaultAdvertiseAddress'
Assert-Contains $controllerInstaller '169.254'

foreach ($script in $shellInstallers) {
    Assert-Contains $script "--disable"
}
Assert-Contains (Join-Path $root "scripts/install-node.sh") "--service-mode"
Assert-Contains (Join-Path $root "scripts/install-controller.ps1") "node-release.json"
Assert-Contains (Join-Path $root "scripts/install-controller.ps1") "takeownArguments"
Assert-Contains (Join-Path $root "scripts/install-controller.ps1") '"/R", "/D", "Y"'
Assert-Contains (Join-Path $root "scripts/install-controller.ps1") "Assert-HostPort"
Assert-Contains (Join-Path $root "scripts/install-controller.ps1") "Get-DefaultAdvertiseAddress"
Assert-Contains (Join-Path $root "scripts/install-node.ps1") "DataRoot"
Assert-Contains (Join-Path $root "scripts/install-node.ps1") "InstallRoot"
Assert-Contains (Join-Path $root "scripts/install-node.ps1") "--disable"
Assert-Contains (Join-Path $root "scripts/install-node.ps1") "Assert-HostPort"

$nodeHelp = (& $gitBash (Join-Path $root "scripts/install-node.sh") --help 2>&1 | Out-String)
if ($LASTEXITCODE -ne 0 -or $nodeHelp -notmatch "Usage: install-node\.sh") {
    throw "Linux Node installer help contract failed"
}
$controllerHelp = (& $gitBash (Join-Path $root "scripts/install-controller.sh") --help 2>&1 | Out-String)
if ($LASTEXITCODE -ne 0 -or $controllerHelp -notmatch "Usage: install-controller\.sh") {
    throw "Linux Controller installer help contract failed"
}

$report = [ordered]@{
    timestamp_utc = [DateTime]::UtcNow.ToString("o")
    platform = "windows"
    shell_syntax = $shellInstallers
    powershell_syntax = @("scripts/install-controller.ps1", "scripts/install-node.ps1")
    runtime_install = "not-run-by-default; use isolated Windows/WSL test hosts"
}
$report | ConvertTo-Json -Depth 4 | Set-Content -Encoding utf8 (Join-Path $outputDir "report.json")
Write-Host "Installer syntax and contract verification passed"
