[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$NodeId,
  [Parameter(Mandatory = $true)][string]$Controller,
  [Parameter(Mandatory = $true)][string]$Token,
  [Parameter(Mandatory = $true)][string]$CAPemB64,
  [Parameter(Mandatory = $true)][string]$ReleaseBaseURL,
  [Parameter(Mandatory = $true)][string]$Version,
  [Parameter(Mandatory = $true)][ValidateSet("amd64", "arm64")][string]$Arch,
  [switch]$Force
)

$ErrorActionPreference = "Stop"

function Invoke-Sc {
  param([Parameter(Mandatory = $true)][string[]]$Arguments)
  & sc.exe @Arguments | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "sc.exe $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
  }
}

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "run this installer from an elevated PowerShell window"
}
if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$') { throw "version must be X.Y.Z or X.Y.Z-rc.N" }

$rawArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$actualArch = switch ($rawArch.ToUpperInvariant()) {
  "AMD64" { "amd64"; break }
  "ARM64" { "arm64"; break }
  default { throw "unsupported Windows architecture: $rawArch" }
}
if ($actualArch -ne $Arch) { throw "selected architecture $Arch does not match this host ($actualArch)" }

$installRoot = Join-Path $env:ProgramFiles "AsterFerry"
$stateRoot = Join-Path $env:ProgramData "AsterFerry"
$tempRoot = Join-Path $env:TEMP ("asterferry-node-" + [guid]::NewGuid().ToString("N"))
$archive = "asterferry_${Version}_windows_${Arch}.zip"
$base = ($ReleaseBaseURL.TrimEnd("/")) + "/v" + $Version
$archivePath = Join-Path $tempRoot $archive
$sumsPath = Join-Path $tempRoot "SHA256SUMS"
$extractRoot = Join-Path $tempRoot "extract"
$caPath = Join-Path $stateRoot "controller-ca.crt"
$bootstrapPath = Join-Path $stateRoot "node-bootstrap.json"
$cachePath = Join-Path $stateRoot "snapshot.cache"
$binaryPath = Join-Path $installRoot "asterferry.exe"

New-Item -ItemType Directory -Force -Path $tempRoot, $installRoot, $stateRoot, $extractRoot | Out-Null
try {
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $archivePath
  Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sumsPath
  $escapedArchive = [regex]::Escape($archive)
  $sumLine = Get-Content $sumsPath | Where-Object { $_ -match "^([0-9a-fA-F]{64})\s+\*?$escapedArchive$" } | Select-Object -First 1
  if (-not $sumLine) { throw "release checksum for $archive was not found" }
  $expected = ([regex]::Match($sumLine, '^[0-9a-fA-F]{64}')).Value.ToLowerInvariant()
  $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
  if ($actual -ne $expected) { throw "release checksum verification failed" }
  Expand-Archive -LiteralPath $archivePath -DestinationPath $extractRoot -Force
  $extractedBinary = Join-Path $extractRoot "asterferry.exe"
  if (-not (Test-Path -LiteralPath $extractedBinary)) { throw "release archive does not contain asterferry.exe" }
  Copy-Item -LiteralPath $extractedBinary -Destination $binaryPath -Force

  [IO.File]::WriteAllBytes($caPath, [Convert]::FromBase64String($CAPemB64))
  if (-not (Test-Path -LiteralPath $bootstrapPath) -or $Force) {
    & $binaryPath node enroll --controller $Controller --token $Token --node-id $NodeId --ca $caPath --output $bootstrapPath --cache $cachePath
    if ($LASTEXITCODE -ne 0) { throw "AsterFerry enrollment failed with exit code $LASTEXITCODE" }
  } else {
    Write-Host "existing $bootstrapPath found; enrollment skipped"
  }

  $acl = Get-Acl $stateRoot
  $acl.SetAccessRuleProtection($true, $false)
  $acl.SetAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule("SYSTEM", "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")))
  $acl.SetAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule("BUILTIN\Administrators", "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")))
  $acl.SetAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule("NT AUTHORITY\LOCAL SERVICE", "ReadAndExecute,Write", "ContainerInherit,ObjectInherit", "None", "Allow")))
  Set-Acl -LiteralPath $stateRoot -AclObject $acl

  $nodeCommand = "node"
  $serviceSuffix = "Node"
  $serviceName = "AsterFerry-Node"
  $displayName = "AsterFerry Node"
  $binPath = '"{0}" {1} run --bootstrap "{2}"' -f $binaryPath, $nodeCommand, $bootstrapPath
  $existing = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
  if ($existing) {
    if ($existing.Status -ne "Stopped") { Stop-Service -Name $serviceName -Force }
    Invoke-Sc -Arguments @("config", $serviceName, "binPath=", $binPath, "start=", "auto", "obj=", "NT AUTHORITY\LocalService")
  } else {
    Invoke-Sc -Arguments @("create", $serviceName, "binPath=", $binPath, "start=", "auto", "obj=", "NT AUTHORITY\LocalService", "DisplayName=", $displayName)
  }
  Invoke-Sc -Arguments @("failure", $serviceName, "actions=", "restart/5000/restart/30000/restart/60000", "reset=", "86400")
  Start-Service -Name $serviceName
  Write-Host "AsterFerry $displayName $NodeId installed and started"
} finally {
  Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
}
