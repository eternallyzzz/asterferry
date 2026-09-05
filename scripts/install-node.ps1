[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$NodeId,
  [Parameter(Mandatory = $true)][string]$Controller,
  [Parameter(Mandatory = $true)][string]$Token,
  [Parameter(Mandatory = $true)][string]$CAPemB64,
  [string]$Repo = "eternallyzzz/asterferry",
  [string]$Version = "",
  [string]$ReleaseBaseURL = "",
  [string]$Arch = "",
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

function Resolve-LatestVersion {
  $headers = @{
    Accept = "application/vnd.github+json"
    "User-Agent" = "asterferry-installer"
  }
  try {
    $releases = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases?per_page=100" -Headers $headers
  } catch {
    throw "cannot query GitHub releases for ${Repo}: $($_.Exception.Message)"
  }
  $release = @($releases) |
    Where-Object { $_.tag_name -match '^v\d+\.\d+\.\d+(-rc\.\d+)?$' } |
    Select-Object -First 1
  if (-not $release) { throw "no published semantic release was found for $Repo" }
  return ([string]$release.tag_name).TrimStart("v")
}

function Assert-ByteArrayEqual {
  param(
    [Parameter(Mandatory = $true)][byte[]]$Left,
    [Parameter(Mandatory = $true)][byte[]]$Right
  )
  if ($Left.Length -ne $Right.Length) { return $false }
  for ($index = 0; $index -lt $Left.Length; $index++) {
    if ($Left[$index] -ne $Right[$index]) { return $false }
  }
  return $true
}

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "run this installer from an elevated PowerShell window"
}
if ($Repo -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') { throw "repo must be OWNER/REPO" }
if ($ReleaseBaseURL -and $ReleaseBaseURL -notmatch '^https://') { throw "release base URL must use HTTPS" }
if ($Version) {
  $Version = $Version.TrimStart("v")
  if ($Version -notmatch '^\d+\.\d+\.\d+(-rc\.\d+)?$') { throw "version must be X.Y.Z or X.Y.Z-rc.N" }
} elseif ($ReleaseBaseURL) {
  throw "-Version is required when -ReleaseBaseURL is used"
} else {
  $Version = Resolve-LatestVersion
}

$rawArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$actualArch = switch ($rawArch.ToUpperInvariant()) {
  "AMD64" { "amd64"; break }
  "ARM64" { "arm64"; break }
  default { throw "unsupported Windows architecture: $rawArch" }
}
if (-not $Arch) { $Arch = $actualArch }
if ($Arch -notin @("amd64", "arm64")) { throw "arch must be amd64 or arm64" }
if ($actualArch -ne $Arch) { throw "selected architecture $Arch does not match this host ($actualArch)" }
if ($Arch -eq "arm64") { throw "the current Windows release supports amd64 only" }

$installRoot = Join-Path $env:ProgramFiles "AsterFerry"
$stateRoot = Join-Path $env:ProgramData "AsterFerry"
$tempRoot = Join-Path $env:TEMP ("asterferry-node-" + [guid]::NewGuid().ToString("N"))
$archive = "asterferry_${Version}_windows_${Arch}.zip"
$base = if ($ReleaseBaseURL) { ($ReleaseBaseURL.TrimEnd("/")) + "/v" + $Version } else { "https://github.com/$Repo/releases/download/v$Version" }
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

  $caBytes = [Convert]::FromBase64String($CAPemB64)
  if (Test-Path -LiteralPath $caPath) {
    $existingCA = [IO.File]::ReadAllBytes($caPath)
    if (-not (Assert-ByteArrayEqual -Left $existingCA -Right $caBytes)) { throw "existing Controller CA differs; refusing to replace it" }
  } else {
    [IO.File]::WriteAllBytes($caPath, $caBytes)
  }
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

  $serviceName = "AsterFerry-Node"
  $displayName = "AsterFerry Node"
  $binPath = '"{0}" node run --bootstrap "{1}"' -f $binaryPath, $bootstrapPath
  $existing = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
  if ($existing) {
    if ($existing.Status -ne "Stopped") { Stop-Service -Name $serviceName -Force }
    Invoke-Sc -Arguments @("config", $serviceName, "binPath=", $binPath, "start=", "auto", "obj=", "NT AUTHORITY\LocalService")
  } else {
    Invoke-Sc -Arguments @("create", $serviceName, "binPath=", $binPath, "start=", "auto", "obj=", "NT AUTHORITY\LocalService", "DisplayName=", $displayName)
  }
  Invoke-Sc -Arguments @("failure", $serviceName, "actions=", "restart/5000/restart/30000/restart/60000", "reset=", "86400")
  Start-Service -Name $serviceName
  Write-Host "AsterFerry $displayName $NodeId $Version installed and started"
} finally {
  Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
}
