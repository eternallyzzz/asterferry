[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$NodeId,
  [Parameter(Mandatory = $true)][string]$Controller,
  [Parameter(Mandatory = $true)][string]$BootstrapUrl,
  [Parameter(Mandatory = $true)][string]$Token,
  [Parameter(Mandatory = $true)][string]$CAPemB64,
  [string]$DataRoot = (Join-Path $env:ProgramData "AsterFerry"),
  [string]$InstallRoot = (Join-Path $env:ProgramFiles "AsterFerry"),
  [string]$ServiceName = "AsterFerry-Node",
  [Alias("ReEnroll")][switch]$Force
)

$ErrorActionPreference = "Stop"

function Invoke-Sc {
  param([Parameter(Mandatory = $true)][string[]]$Arguments)
  & sc.exe @Arguments | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "sc.exe $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
  }
}

function Invoke-Curl {
  param(
    [Parameter(Mandatory = $true)][string[]]$Arguments,
    [Parameter(Mandatory = $true)][string]$Description
  )
  $curlArguments = @(
    "--disable", "--retry", "5", "--retry-delay", "1", "--connect-timeout", "10", "--max-time", "300",
    "--proto", "=https", "--proto-redir", "=https", "--ssl-no-revoke"
  ) + $Arguments
  & curl.exe @curlArguments
  if ($LASTEXITCODE -ne 0) {
    throw "$Description failed with exit code $LASTEXITCODE"
  }
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

function Grant-LocalServiceStateAccess {
  param([Parameter(Mandatory = $true)][string]$Path)
  # A previous LocalService installation may have left protected child files
  # owned by SYSTEM. An elevated installer can safely take ownership while it
  # repairs the ACL; the service still runs with the narrower Modify grant.
  & takeown.exe /F $Path /R /D Y | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "taking ownership of $Path failed with exit code $LASTEXITCODE"
  }
  $output = @(& icacls.exe @($Path, "/grant:r", "*S-1-5-19:(OI)(CI)M", "/T", "/C") 2>&1)
  $exitCode = $LASTEXITCODE
  $message = ($output | ForEach-Object { [string]$_ }) -join "`n"
  if ($exitCode -ne 0 -or $message -match '(?im)access is denied|拒绝访问|failed processing\s+[1-9]\d*\s+files|处理\s*[1-9]\d*\s*个文件时失败') {
    if ([string]::IsNullOrWhiteSpace($message)) {
      throw "granting LocalService access to $Path failed with exit code $exitCode"
    }
    throw "granting LocalService access to $Path failed with exit code $exitCode`n$message"
  }
}

function Get-ReleaseArtifact {
  param(
    [Parameter(Mandatory = $true)][object]$Metadata,
    [Parameter(Mandatory = $true)][string]$Key
  )
  if (-not $Metadata.artifacts) { throw "Controller returned node release metadata without artifacts" }
  $property = $Metadata.artifacts.PSObject.Properties[$Key]
  if (-not $property) { throw "Controller release does not contain an artifact for $Key" }
  return [string]$property.Value
}

function Remove-MalformedProxyEnvironment {
  foreach ($name in @("http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY")) {
    $value = [Environment]::GetEnvironmentVariable($name, [EnvironmentVariableTarget]::Process)
    if ($value -and $value -match '\s') {
      Write-Warning "ignoring malformed $name containing whitespace"
      [Environment]::SetEnvironmentVariable($name, $null, [EnvironmentVariableTarget]::Process)
    }
  }
}

function Test-DirectHost {
  param(
    [Parameter(Mandatory = $true)][string]$HostName,
    [Parameter(Mandatory = $true)][string]$ControllerHost
  )
  $hostValue = $HostName.ToLowerInvariant()
  if ($hostValue -eq $ControllerHost.ToLowerInvariant() -or $hostValue -eq "localhost" -or $hostValue.EndsWith(".localhost")) { return $true }
  if ($hostValue -eq "::1" -or $hostValue -match '^(fc|fd)[0-9a-f]{2}:' -or $hostValue.StartsWith("fe80:")) { return $true }
  if ($hostValue -match '^(127\.|10\.|192\.168\.|169\.254\.)') { return $true }
  if ($hostValue -match '^172\.(\d{1,3})\.') {
    $secondOctet = [int]$Matches[1]
    if ($secondOctet -ge 16 -and $secondOctet -le 31) { return $true }
  }
  return $false
}

if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "run this installer from an elevated PowerShell window"
}
if ($BootstrapUrl -notmatch '^https://[^\s]+$') { throw "bootstrap URL must use HTTPS" }
$bootstrapUri = [Uri]$BootstrapUrl
$bootstrapHost = $bootstrapUri.DnsSafeHost
if ([string]::IsNullOrWhiteSpace($bootstrapHost)) { throw "bootstrap URL does not contain a host" }
if (-not [IO.Path]::IsPathRooted($DataRoot) -or -not [IO.Path]::IsPathRooted($InstallRoot)) {
  throw "DataRoot and InstallRoot must be absolute paths"
}
if ($ServiceName -notmatch '^[A-Za-z0-9_.@:-]+$') {
  throw "ServiceName contains unsupported characters"
}
Remove-MalformedProxyEnvironment

$rawArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($rawArch.ToUpperInvariant()) {
  "AMD64" { "amd64"; break }
  "ARM64" { "arm64"; break }
  default { throw "unsupported Windows architecture: $rawArch" }
}
if ($arch -eq "arm64") { throw "the current Windows release supports amd64 only" }

$installRoot = $InstallRoot
$stateRoot = $DataRoot
$tempRoot = Join-Path $env:TEMP ("asterferry-node-" + [guid]::NewGuid().ToString("N"))
$releaseMetadataPath = Join-Path $tempRoot "node-release.json"
$archivePath = Join-Path $tempRoot "asterferry.zip"
$sumsPath = Join-Path $tempRoot "SHA256SUMS"
$extractRoot = Join-Path $tempRoot "extract"
$caPath = Join-Path $stateRoot "controller-ca.crt"
$temporaryCaPath = Join-Path $tempRoot "controller-ca.crt"
$bootstrapPath = Join-Path $stateRoot "node-bootstrap.json"
$cachePath = Join-Path $stateRoot "snapshot.cache"
$cacheKeyPath = Join-Path $stateRoot "snapshot.key"
$decommissionMarkerPath = "$bootstrapPath.decommissioned"
$binaryPath = Join-Path $stateRoot "bin\asterferry.exe"
$installerBinaryPath = Join-Path $installRoot "asterferry.exe"
$serviceName = $ServiceName
$displayName = "AsterFerry Node"

New-Item -ItemType Directory -Force -Path $tempRoot, $installRoot, $stateRoot, (Join-Path $stateRoot "bin"), $extractRoot | Out-Null
try {
  $caBytes = [Convert]::FromBase64String($CAPemB64)
  [IO.File]::WriteAllBytes($temporaryCaPath, $caBytes)

  $bootstrapEndpoint = $BootstrapUrl.TrimEnd("/") + "/bootstrap/node/release"
  Invoke-Curl -Description "downloading node release metadata" -Arguments @(
    "--fail", "--silent", "--show-error", "--location", "--tlsv1.3",
    "--noproxy", $bootstrapHost,
    "--cacert", $temporaryCaPath,
    "--header", ("X-AsterFerry-Enrollment-Token: " + $Token),
    "--output", $releaseMetadataPath,
    $bootstrapEndpoint
  )

  $metadata = Get-Content -Raw -LiteralPath $releaseMetadataPath | ConvertFrom-Json
  $version = [string]$metadata.version
  if ($version -notmatch '^\d+\.\d+\.\d+(-rc\.\d+)?$') { throw "Controller returned an invalid node release version" }
  $releaseBaseUrl = [string]$metadata.release_base_url
  if ($releaseBaseUrl -notmatch '^https://[^\s]+$') { throw "Controller returned an invalid node release URL" }
  $releaseHost = ([Uri]$releaseBaseUrl).DnsSafeHost
  if ([string]::IsNullOrWhiteSpace($releaseHost)) { throw "Controller returned a node release URL without a host" }
  $releaseProxyArguments = @()
  if (Test-DirectHost -HostName $releaseHost -ControllerHost $bootstrapHost) {
    $releaseProxyArguments = @("--noproxy", $releaseHost)
  }
  $releaseTLSArguments = @()
  if ($releaseHost.Equals($bootstrapHost, [StringComparison]::OrdinalIgnoreCase)) {
    $releaseTLSArguments = @("--cacert", $temporaryCaPath)
  }
  $archive = Get-ReleaseArtifact -Metadata $metadata -Key ("windows/" + $arch)
  if ($archive -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw "Controller returned an unsafe node release artifact name" }

  $releaseUrl = $releaseBaseUrl.TrimEnd("/") + "/v" + $version
  Invoke-Curl -Description "downloading AsterFerry node release" -Arguments (@(
    "--fail", "--silent", "--show-error", "--location", "--tlsv1.3"
  ) + $releaseProxyArguments + $releaseTLSArguments + @(
    "--output", $archivePath,
    ($releaseUrl + "/" + $archive)
  ))
  Invoke-Curl -Description "downloading AsterFerry release checksums" -Arguments (@(
    "--fail", "--silent", "--show-error", "--location", "--tlsv1.3"
  ) + $releaseProxyArguments + $releaseTLSArguments + @(
    "--output", $sumsPath,
    ($releaseUrl + "/SHA256SUMS")
  ))

  $escapedArchive = [regex]::Escape($archive)
  $sumLine = Get-Content -LiteralPath $sumsPath | Where-Object { $_ -match "^([0-9a-fA-F]{64})\s+\*?$escapedArchive$" } | Select-Object -First 1
  if (-not $sumLine) { throw "release checksum for $archive was not found" }
  $expected = ([regex]::Match($sumLine, '^[0-9a-fA-F]{64}')).Value.ToLowerInvariant()
  $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
  if ($actual -ne $expected) { throw "release checksum verification failed" }

  Expand-Archive -LiteralPath $archivePath -DestinationPath $extractRoot -Force
  $extractedBinary = Join-Path $extractRoot "asterferry.exe"
  if (-not (Test-Path -LiteralPath $extractedBinary)) { throw "release archive does not contain asterferry.exe" }

  $existing = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
  $existingStatePaths = @($bootstrapPath, $cachePath, $cacheKeyPath, $decommissionMarkerPath)
  $hasExistingIdentity = $null -ne $existing
  foreach ($statePath in $existingStatePaths) {
    if (Test-Path -LiteralPath $statePath) {
      $hasExistingIdentity = $true
      break
    }
  }
  $existingCADiffers = $false
  if (Test-Path -LiteralPath $caPath) {
    $existingCA = [IO.File]::ReadAllBytes($caPath)
    $existingCADiffers = -not (Assert-ByteArrayEqual -Left $existingCA -Right $caBytes)
  }
  $replaceOrphanedCA = $existingCADiffers -and -not $hasExistingIdentity
  if ($existingCADiffers -and $hasExistingIdentity -and -not $Force) {
    throw "this machine already has a Node identity for a different Controller; rerun with -Force/-ReEnroll and a fresh enrollment token to replace it"
  }
  if ((Test-Path -LiteralPath $decommissionMarkerPath) -and -not $Force) {
    throw "node is decommissioned; rerun with -Force/-ReEnroll and a fresh enrollment token"
  }
  if ($existing -and $existing.Status -ne "Stopped") { Stop-Service -Name $serviceName -Force }
  if ($Force -or $replaceOrphanedCA) {
    $recoveryRoot = Join-Path $stateRoot (Join-Path "recovery" (Get-Date).ToUniversalTime().ToString("yyyyMMddHHmmss"))
    New-Item -ItemType Directory -Force -Path $recoveryRoot | Out-Null
    foreach ($statePath in @($caPath, $bootstrapPath, $cachePath, $cacheKeyPath, $decommissionMarkerPath)) {
      if (Test-Path -LiteralPath $statePath) {
        Copy-Item -LiteralPath $statePath -Destination (Join-Path $recoveryRoot ([IO.Path]::GetFileName($statePath))) -Force
      }
    }
  }
  if ($Force) {
    # The Controller rebuilds desired state after the replacement certificate
    # is issued; do not reuse the old encrypted cache or lifecycle marker.
    Remove-Item -LiteralPath $bootstrapPath, $cachePath, $cacheKeyPath, $decommissionMarkerPath -Force -ErrorAction SilentlyContinue
  }
  if ($replaceOrphanedCA) {
    Write-Host "replacing Controller CA left by an incomplete Node installation"
  }
  if (-not (Test-Path -LiteralPath $caPath) -or $existingCADiffers) {
    [IO.File]::WriteAllBytes($caPath, $caBytes)
  }
  Copy-Item -LiteralPath $extractedBinary -Destination $binaryPath -Force
  Copy-Item -LiteralPath $extractedBinary -Destination $installerBinaryPath -Force
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
  # The Node writes bootstrap/cache state through atomic rename.  Write-only
  # access lets LocalService create the temporary file but not delete/replace
  # it, so the first snapshot.key publish fails with ERROR_ACCESS_DENIED.
  # Modify includes the delete/rename rights required by atomicfile.AtomicWrite
  # and is inherited by the state files and the service log directory.
  $acl.SetAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule("NT AUTHORITY\LOCAL SERVICE", "Modify", "ContainerInherit,ObjectInherit", "None", "Allow")))
  Set-Acl -LiteralPath $stateRoot -AclObject $acl
  # Explicitly repair existing children as well. A file created under the
  # previous ACL can keep a protected DACL and would otherwise still reject
  # the LocalService atomic rename on an upgrade or re-install.
  Grant-LocalServiceStateAccess -Path $stateRoot

  $binPath = '"{0}" node run --bootstrap "{1}" --service-name "{2}" --service-mode windows-service' -f $binaryPath, $bootstrapPath, $serviceName
  if ($existing) {
    Invoke-Sc -Arguments @("config", $serviceName, "binPath=", $binPath, "start=", "auto", "obj=", "NT AUTHORITY\LocalService")
  } else {
    Invoke-Sc -Arguments @("create", $serviceName, "binPath=", $binPath, "start=", "auto", "obj=", "NT AUTHORITY\LocalService", "DisplayName=", $displayName)
  }
  Invoke-Sc -Arguments @("failure", $serviceName, "actions=", "restart/5000/restart/30000/restart/60000", "reset=", "86400")
  try {
    Start-Service -Name $serviceName
    Start-Sleep -Milliseconds 750
    $startedService = Get-Service -Name $serviceName
    if ($startedService.Status -ne "Running") {
      $logPath = Join-Path $env:ProgramData ("AsterFerry\logs\" + $serviceName + ".log")
      $details = if (Test-Path -LiteralPath $logPath) { (Get-Content -LiteralPath $logPath -Tail 20 -ErrorAction SilentlyContinue) -join "`n" } else { "service log was not created" }
      throw "Node service did not remain running (status: $($startedService.Status)). $details"
    }
  } catch {
    throw "failed to start Node service '$serviceName': $($_.Exception.Message)"
  }
  if ($Force) {
    Write-Host "AsterFerry $displayName $NodeId $version re-enrolled and started"
  } else {
    Write-Host "AsterFerry $displayName $NodeId $version installed and started"
  }
} finally {
  Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
}
