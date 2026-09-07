[CmdletBinding()]
# Defaults: HTTPS/gRPC listen on 0.0.0.0, metrics on 127.0.0.1, and the newest
# stable release is resolved from the configured GitHub repository when no
# explicit release URL/version is supplied.
param(
  [string]$GrpcAdvertise = "",
  [string]$Repo = "eternallyzzz/asterferry",
  [string]$Version = "",
  [string]$ReleaseBaseUrl = "",
  [string]$DataRoot = (Join-Path $env:ProgramData "AsterFerry\Controller"),
  [string]$InstallRoot = (Join-Path $env:ProgramFiles "AsterFerry"),
  [string]$HttpListen = "0.0.0.0:8443",
  [string]$GrpcListen = "0.0.0.0:9443",
  [string]$MetricsListen = "127.0.0.1:9090",
  [string]$Username = "admin",
  [string]$PasswordFile = "",
  [string]$ServiceName = "AsterFerry-Controller",
  [switch]$NonInteractive
)

$ErrorActionPreference = "Stop"

function Write-Step {
  param([Parameter(Mandatory = $true)][string]$Message)
  Write-Host "==> $Message"
}

function Write-Info {
  param([Parameter(Mandatory = $true)][string]$Message)
  Write-Host "    $Message"
}

function Assert-HostPort {
  param(
    [Parameter(Mandatory = $true)][string]$Value,
    [Parameter(Mandatory = $true)][string]$Label
  )
  if ($Value -match '\s') {
    throw "$Label must not contain whitespace"
  }
  if ($Value -notmatch '^([^:]+|\[[^\]]+\]):\d+$') {
    throw "$Label must be host:port (for example controller.example.com:9443)"
  }
  if ($Value -match '^(?<host>.+):(?<port>\d+)$') {
    $hostPart = $Matches['host'] -replace '^\[', '' -replace '\]$', ''
    $port = [int]$Matches['port']
    if ($hostPart -eq '' -or $hostPart -eq '0.0.0.0' -or $hostPart -eq '::') {
      throw "$Label must identify a reachable host, not an unspecified address"
    }
    if ($port -lt 1 -or $port -gt 65535) {
      throw "$Label port must be between 1 and 65535"
    }
  }
}

function Get-DefaultAdvertiseAddress {
  $grpcPort = ($GrpcListen -split ':')[-1]
  $candidate = $null
  try {
    $candidate = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
      Where-Object {
        $_.IPAddress -notlike '127.*' -and
        $_.IPAddress -notlike '169.254.*' -and
        $_.IPAddress -ne '0.0.0.0' -and
        $_.AddressState -eq 'Preferred'
      } |
      Select-Object -First 1 -ExpandProperty IPAddress
  } catch {
    $candidate = $null
  }
  if (-not $candidate) {
    try {
      $candidate = [System.Net.Dns]::GetHostAddresses([System.Net.Dns]::GetHostName()) |
        Where-Object {
          $_.AddressFamily -eq 'InterNetwork' -and
          $_.IPAddressToString -notlike '127.*' -and
          $_.IPAddressToString -notlike '169.254.*'
        } |
        Select-Object -First 1 -ExpandProperty IPAddressToString
    } catch {
      $candidate = $null
    }
  }
  if ($candidate) {
    return ("{0}:{1}" -f $candidate, $grpcPort)
  }
  return ""
}

# The source installer leaves these empty and resolves the newest stable release.
# Release packaging replaces them with the immutable release URL and version.
$script:embeddedReleaseBaseUrl = ""
$script:embeddedReleaseVersion = ""
$script:installerIdentity = $null
$script:installerAccessGranted = $false
$script:installerAccessPaths = @()

function Invoke-Sc {
  param([Parameter(Mandatory = $true)][string[]]$Arguments)
  & sc.exe @Arguments | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "sc.exe $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
  }
}

function Assert-Administrator {
  $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "run this installer from an elevated PowerShell window (right-click PowerShell and choose Run as administrator, or use: sudo pwsh -File $PSCommandPath)"
  }
}

function Invoke-Icacls {
  param(
    [Parameter(Mandatory = $true)][string[]]$Arguments,
    [Parameter(Mandatory = $true)][string]$Operation
  )
  $output = @(& icacls.exe @Arguments 2>&1)
  $exitCode = $LASTEXITCODE
  $message = ($output | ForEach-Object { [string]$_ }) -join "`n"
  if ($exitCode -ne 0 -or $message -match '(?im)access is denied|拒绝访问|failed processing\s+[1-9]\d*\s+files|处理\s*[1-9]\d*\s*个文件时失败') {
    if ([string]::IsNullOrWhiteSpace($message)) {
      throw "$Operation failed with exit code $exitCode"
    }
    throw "$Operation failed with exit code $exitCode`n$message"
  }
}

function Read-InstallerValue {
  param(
    [Parameter(Mandatory = $true)][string]$Prompt,
    [string]$Default = "",
    [string]$DefaultLabel = "",
    [switch]$Required
  )
  $suffix = if ($DefaultLabel) { " [$DefaultLabel]" } elseif ($Default) { " [$Default]" } else { "" }
  $value = (Read-Host "$Prompt$suffix").Trim()
  if ([string]::IsNullOrWhiteSpace($value)) {
    $value = $Default
  }
  if ($Required -and [string]::IsNullOrWhiteSpace($value)) {
    throw "$Prompt is required"
  }
  return $value
}

function Register-InstallerAccessPath {
  param([Parameter(Mandatory = $true)][string]$Path)
  if ($script:installerAccessPaths -notcontains $Path) {
    $script:installerAccessPaths += $Path
  }
  $script:installerAccessGranted = $true
}

function Grant-InstallerPathAccess {
  param(
    [Parameter(Mandatory = $true)][string]$Path,
    [switch]$Directory
  )
  Register-InstallerAccessPath -Path $Path
  $takeownArguments = @("/F", $Path)
  if ($Directory) {
    # Ownership of a directory alone is not enough to repair a protected
    # controller.json or a node-installer child. /D is valid only together
    # with /R; answering Y also makes this non-interactive for inherited ACLs.
    $takeownArguments += @("/R", "/D", "Y")
  }
  & takeown.exe @takeownArguments | Out-Null
  if ($LASTEXITCODE -ne 0) {
    throw "could not take ownership of $Path"
  }
  $permission = if ($Directory) { "(OI)(CI)F" } else { "F" }
  $adminGrant = "*S-1-5-32-544:$permission"
  $installerGrant = "*{0}:{1}" -f $script:installerIdentity, $permission
  Invoke-Icacls -Arguments @($Path, "/grant:r", $adminGrant, $installerGrant) -Operation "granting installer access to $Path"
  if ($Directory) {
    Invoke-Icacls -Arguments @($Path, "/inheritance:r", "/grant:r", $adminGrant, $installerGrant) -Operation "isolating installer directory permissions for $Path"
  }
}

function Grant-InstallerAccess {
  param(
    [Parameter(Mandatory = $true)][string]$Path,
    [string]$ConfigPath = "",
    [bool]$ConfigExists = $false
  )
  $script:installerIdentity = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  Grant-InstallerPathAccess -Path $Path -Directory
  if ($ConfigExists -and $ConfigPath) {
    Grant-InstallerPathAccess -Path $ConfigPath
  }
}

function Remove-InstallerAccess {
  if (-not $script:installerAccessGranted -or [string]::IsNullOrWhiteSpace($script:installerIdentity)) {
    return
  }
  foreach ($path in @($script:installerAccessPaths | Select-Object -Unique)) {
    try {
      Invoke-Icacls -Arguments @($path, "/remove", "*$script:installerIdentity") -Operation "removing temporary installer access from $path"
    } catch {
      Write-Warning $_.Exception.Message
    }
  }
  $script:installerAccessGranted = $false
}

function Restore-ControllerDataSecurity {
  foreach ($path in @($script:installerAccessPaths | Select-Object -Unique)) {
    $isDirectory = Test-Path -LiteralPath $path -PathType Container -ErrorAction SilentlyContinue
    $permission = if ($isDirectory) { "(OI)(CI)F" } else { "F" }
    $arguments = @($path)
    if ($isDirectory) {
      $arguments += "/inheritance:r"
    }
    $arguments += @("/grant:r", "*S-1-5-18:$permission", "*S-1-5-32-544:$permission")
    if ($isDirectory) {
      $arguments += @("/T", "/C")
    }
    Invoke-Icacls -Arguments $arguments -Operation "securing Controller data path $path"

    # /T does not reliably replace a protected child DACL on all Windows
    # versions. Explicitly grant the service account and Administrators on
    # every child as well; otherwise LocalSystem can still receive
    # "Access is denied" for controller.json or the database even though the
    # parent directory looks correct.
    if ($isDirectory) {
      $children = @(Get-ChildItem -LiteralPath $path -Force -Recurse -ErrorAction Stop)
      foreach ($child in $children) {
        $childIsDirectory = $child.PSIsContainer
        $childPermission = if ($childIsDirectory) { "(OI)(CI)F" } else { "F" }
        $childArguments = @($child.FullName, "/grant:r", "*S-1-5-18:$childPermission", "*S-1-5-32-544:$childPermission")
        Invoke-Icacls -Arguments $childArguments -Operation "securing Controller data path $($child.FullName)"
      }
    }

    # The service only needs the SYSTEM ACL above. On some Windows versions an
    # elevated Administrators token can repair the ACL but cannot change the
    # owner of every existing child file to SYSTEM (icacls reports access
    # denied even though the ACL is already correct). Treat owner normalization
    # as best effort so a stale owner cannot prevent a usable installation.
    $ownerArguments = @($path, "/setowner", "*S-1-5-18")
    if ($isDirectory) {
      $ownerArguments += @("/T", "/C")
    }
    try {
      Invoke-Icacls -Arguments $ownerArguments -Operation "restoring Controller data path owner $path"
    } catch {
      Write-Warning $_.Exception.Message
    }
  }
}

function Assert-ReleaseVersion {
  param([Parameter(Mandatory = $true)][string]$Value)
  if ($Value -notmatch '^\d+\.\d+\.\d+(-rc\.\d+)?$') {
    throw "version must be X.Y.Z or X.Y.Z-rc.N"
  }
}

function Resolve-LatestVersion {
  $headers = @{ "Accept" = "application/vnd.github+json"; "User-Agent" = "asterferry-installer" }
  $releases = Invoke-RestMethod -UseBasicParsing -Headers $headers -Uri "https://api.github.com/repos/$Repo/releases?per_page=100"
  foreach ($release in @($releases)) {
    if ($release.draft) { continue }
    $tag = [string]$release.tag_name
    if ($tag -match '^v\d+\.\d+\.\d+(-rc\.\d+)?$') {
      return $tag.TrimStart('v')
    }
  }
  throw "no published release was found for $Repo"
}

function Get-ExpectedHash {
  param([Parameter(Mandatory = $true)][string]$Name)
  $escaped = [regex]::Escape($Name)
  $line = Get-Content -LiteralPath $script:SumsPath |
    Where-Object { $_ -match ("^[0-9a-fA-F]{64}\s+\*?" + $escaped + "$") } |
    Select-Object -First 1
  if (-not $line) {
    throw "release checksum for $Name was not found"
  }
  return ([string]$line).Substring(0, 64).ToLowerInvariant()
}

function Download-VerifiedAsset {
  param([Parameter(Mandatory = $true)][string]$Name)
  $destination = Join-Path $TempRoot $Name
  Write-Info "Downloading $Name"
  Invoke-WebRequest -UseBasicParsing -Uri "$ReleaseRoot/$Name" -OutFile $destination
  $expected = Get-ExpectedHash -Name $Name
  $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $destination).Hash.ToLowerInvariant()
  if ($actual -ne $expected) {
    throw "release checksum verification failed for $Name"
  }
  return $destination
}

$repoWasExplicit = $PSBoundParameters.ContainsKey("Repo")
$releaseBaseUrlWasExplicit = $PSBoundParameters.ContainsKey("ReleaseBaseUrl")
$versionWasExplicit = $PSBoundParameters.ContainsKey("Version")
$grpcAdvertiseWasExplicit = $PSBoundParameters.ContainsKey("GrpcAdvertise")
$dataRootWasExplicit = $PSBoundParameters.ContainsKey("DataRoot")
$installRootWasExplicit = $PSBoundParameters.ContainsKey("InstallRoot")
$httpListenWasExplicit = $PSBoundParameters.ContainsKey("HttpListen")
$grpcListenWasExplicit = $PSBoundParameters.ContainsKey("GrpcListen")
$metricsListenWasExplicit = $PSBoundParameters.ContainsKey("MetricsListen")
$usernameWasExplicit = $PSBoundParameters.ContainsKey("Username")
$passwordFileWasExplicit = $PSBoundParameters.ContainsKey("PasswordFile")
$serviceNameWasExplicit = $PSBoundParameters.ContainsKey("ServiceName")
if (-not $repoWasExplicit -and -not $releaseBaseUrlWasExplicit -and $script:embeddedReleaseBaseUrl) {
  $ReleaseBaseUrl = $script:embeddedReleaseBaseUrl
}
if (-not $repoWasExplicit -and -not $versionWasExplicit -and $script:embeddedReleaseVersion) {
  $Version = $script:embeddedReleaseVersion
}

Assert-Administrator

if (-not $NonInteractive) {
  $detectedAdvertise = Get-DefaultAdvertiseAddress
  if (-not $grpcAdvertiseWasExplicit -or [string]::IsNullOrWhiteSpace($GrpcAdvertise)) {
    if ($detectedAdvertise) {
      $GrpcAdvertise = Read-InstallerValue -Prompt "Controller gRPC advertise address (required)" -Default $detectedAdvertise
    } else {
      $GrpcAdvertise = Read-InstallerValue -Prompt "Controller gRPC advertise address (required)" -Required
    }
  }
  if (-not $repoWasExplicit) {
    $Repo = Read-InstallerValue -Prompt "GitHub repository" -Default $Repo
  }
  if (-not $releaseBaseUrlWasExplicit) {
    $defaultReleaseBase = "https://github.com/$Repo/releases/download"
    $ReleaseBaseUrl = Read-InstallerValue -Prompt "Release base URL" -Default $defaultReleaseBase
    if ($ReleaseBaseUrl -and $ReleaseBaseUrl -ne $defaultReleaseBase) {
      $releaseBaseUrlWasExplicit = $true
    }
  }
  if (-not $versionWasExplicit) {
    if ($releaseBaseUrlWasExplicit) {
      $Version = Read-InstallerValue -Prompt "Release version (required for custom mirror)" -Required
      $Version = $Version.TrimStart('v')
    } else {
      $suggestedVersion = $null
      try {
        $suggestedVersion = Resolve-LatestVersion
      } catch {
        Write-Warning "could not resolve the latest stable release from GitHub; leaving version empty will retry later"
      }
      if ($suggestedVersion) {
        $Version = Read-InstallerValue -Prompt "Release version" -Default $suggestedVersion
      } else {
        $Version = Read-InstallerValue -Prompt "Release version" -DefaultLabel "empty for latest stable"
      }
    }
  }
  if (-not $dataRootWasExplicit) {
    $DataRoot = Read-InstallerValue -Prompt "Controller data directory" -Default $DataRoot
  }
  if (-not $installRootWasExplicit) {
    $InstallRoot = Read-InstallerValue -Prompt "Controller install directory" -Default $InstallRoot
  }
  if (-not $httpListenWasExplicit) {
    $HttpListen = Read-InstallerValue -Prompt "HTTPS listen address" -Default $HttpListen
  }
  if (-not $grpcListenWasExplicit) {
    $GrpcListen = Read-InstallerValue -Prompt "gRPC listen address" -Default $GrpcListen
  }
  if (-not $metricsListenWasExplicit) {
    $MetricsListen = Read-InstallerValue -Prompt "Metrics listen address" -Default $MetricsListen
  }
  if (-not $usernameWasExplicit) {
    $Username = Read-InstallerValue -Prompt "Initial Admin username" -Default $Username
  }
  if (-not $passwordFileWasExplicit) {
    $PasswordFile = Read-InstallerValue -Prompt "Initial Admin password file" -DefaultLabel "empty to generate random password"
  }
  if (-not $serviceNameWasExplicit) {
    $ServiceName = Read-InstallerValue -Prompt "Windows service name" -Default $ServiceName
  }
} elseif (-not $grpcAdvertiseWasExplicit -or [string]::IsNullOrWhiteSpace($GrpcAdvertise)) {
  $detectedAdvertise = Get-DefaultAdvertiseAddress
  if ($detectedAdvertise) {
    $GrpcAdvertise = $detectedAdvertise
  } else {
    $GrpcAdvertise = "127.0.0.1:" + (($GrpcListen -split ':')[-1])
    Write-Host "Could not detect a non-loopback IP; using loopback default: $GrpcAdvertise"
  }
  Write-Host "Using gRPC advertise address: $GrpcAdvertise"
  Write-Host "  (override with -GrpcAdvertise if Nodes cannot reach this address)"
}

if ($Repo -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
  throw "repo must be OWNER/REPO"
}
if ([string]::IsNullOrWhiteSpace($GrpcAdvertise)) {
  throw "GrpcAdvertise must be a reachable host:port"
}
Assert-HostPort -Value $GrpcAdvertise -Label "Controller gRPC advertise address"
if (-not [IO.Path]::IsPathRooted($DataRoot) -or -not [IO.Path]::IsPathRooted($InstallRoot)) {
  throw "DataRoot and InstallRoot must be absolute paths"
}
if ([string]::IsNullOrWhiteSpace($Username)) {
  throw "Username must not be empty"
}
if ($PasswordFile -and -not (Test-Path -LiteralPath $PasswordFile -PathType Leaf)) {
  throw "password file does not exist: $PasswordFile"
}
if ($ReleaseBaseUrl -and $ReleaseBaseUrl -notmatch '^https://[^\s]+$') {
  throw "ReleaseBaseUrl must use HTTPS"
}

Write-Step "Resolving AsterFerry Controller installation"
$Version = $Version.TrimStart('v')
if ([string]::IsNullOrWhiteSpace($Version)) {
  if ($ReleaseBaseUrl -and $releaseBaseUrlWasExplicit) {
    if (-not $NonInteractive) {
      $Version = Read-InstallerValue -Prompt "Release version" -Required
      $Version = $Version.TrimStart('v')
    } else {
      throw "Version is required when ReleaseBaseUrl is used"
    }
  }
  if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = Resolve-LatestVersion
  }
}
Assert-ReleaseVersion -Value $Version
Write-Info "Version $Version"
Write-Info "gRPC advertise: $GrpcAdvertise"

$TempRoot = Join-Path $env:TEMP ("asterferry-controller-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $TempRoot | Out-Null
try {
  if ($ReleaseBaseUrl) {
    $ReleaseRoot = $ReleaseBaseUrl.TrimEnd('/') + "/v$Version"
  } else {
    $ReleaseRoot = "https://github.com/$Repo/releases/download/v$Version"
  }
  Write-Step "Downloading and verifying Controller release"
  $script:SumsPath = Join-Path $TempRoot "SHA256SUMS"
  Invoke-WebRequest -UseBasicParsing -Uri "$ReleaseRoot/SHA256SUMS" -OutFile $script:SumsPath

  $archiveName = "asterferry_${Version}_windows_amd64.zip"
  $archivePath = Download-VerifiedAsset -Name $archiveName
  $nodeAssetNames = @("install-node.ps1", "install-node.sh", "node-release.json")
  $nodeAssetPaths = @{}
  foreach ($assetName in $nodeAssetNames) {
    $nodeAssetPaths[$assetName] = Download-VerifiedAsset -Name $assetName
  }

  $extractRoot = Join-Path $TempRoot "controller"
  New-Item -ItemType Directory -Force -Path $extractRoot | Out-Null
  Expand-Archive -LiteralPath $archivePath -DestinationPath $extractRoot -Force
  $extractedBinary = Get-ChildItem -LiteralPath $extractRoot -Filter "asterferry.exe" -File -Recurse | Select-Object -First 1
  if (-not $extractedBinary) {
    throw "release archive does not contain asterferry.exe"
  }

  Write-Step "Configuring Controller"
  $configPath = Join-Path $DataRoot "controller.json"
  $existingConfig = Test-Path -LiteralPath $configPath -PathType Leaf -ErrorAction SilentlyContinue
  $existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($existingService -and $existingService.Status -ne "Stopped") {
    Stop-Service -Name $ServiceName -Force
  }
  New-Item -ItemType Directory -Force -Path $InstallRoot, $DataRoot, (Join-Path $DataRoot "bin") | Out-Null
  Grant-InstallerAccess -Path $DataRoot -ConfigPath $configPath -ConfigExists $existingConfig
  # The service account must be able to atomically replace the active binary
  # during a Controller-managed upgrade. Keep the Program Files copy as an
  # installer-facing cache, while running the service from the data-owned path.
  $binaryPath = Join-Path $DataRoot "bin\asterferry.exe"
  Copy-Item -LiteralPath $extractedBinary.FullName -Destination $binaryPath -Force
  Copy-Item -LiteralPath $extractedBinary.FullName -Destination (Join-Path $InstallRoot "asterferry.exe") -Force

  if (-not $existingConfig) {
    $initArguments = @(
      "controller", "init",
      "--dir", $DataRoot,
      "--force",
      "--http-listen", $HttpListen,
      "--grpc-listen", $GrpcListen,
      "--grpc-advertise", $GrpcAdvertise,
      "--metrics-listen", $MetricsListen,
      "--username", $Username
    )
    if ($PasswordFile) {
      $initArguments += @("--password-file", $PasswordFile)
    }
    & $binaryPath @initArguments
    if ($LASTEXITCODE -ne 0) {
      throw "Controller initialization failed with exit code $LASTEXITCODE"
    }
  } else {
    Write-Host "existing Controller configuration found; initialization skipped"
  }

  $config = Get-Content -Raw -LiteralPath $configPath | ConvertFrom-Json
  $propertyNames = @($config.PSObject.Properties | ForEach-Object Name)
  if ($propertyNames -contains "release_base_url" -or $propertyNames -contains "release_version") {
    throw "existing Controller configuration uses the removed release protocol; use a new data directory"
  }
  if ($propertyNames -notcontains "node_installers_dir" -or [string]::IsNullOrWhiteSpace([string]$config.node_installers_dir)) {
    throw "Controller configuration does not contain node_installers_dir"
  }
  $nodeInstallersDir = [string]$config.node_installers_dir
  Write-Step "Installing Node installer assets"
  Write-Info $nodeInstallersDir
  New-Item -ItemType Directory -Force -Path $nodeInstallersDir | Out-Null
  Grant-InstallerPathAccess -Path $nodeInstallersDir -Directory
  foreach ($assetName in $nodeAssetNames) {
    $destination = Join-Path $nodeInstallersDir $assetName
    $destinationExists = Test-Path -LiteralPath $destination -PathType Leaf -ErrorAction SilentlyContinue
    if ($destinationExists) {
      Grant-InstallerPathAccess -Path $destination
    }
    Copy-Item -LiteralPath $nodeAssetPaths[$assetName] -Destination $destination -Force
    if (-not $destinationExists) {
      Grant-InstallerPathAccess -Path $destination
    }
  }

  Restore-ControllerDataSecurity

  Write-Step "Registering and starting Controller service"
  $serviceCommand = '"{0}" controller run --config "{1}" --service-name "{2}" --service-mode windows-service' -f $binaryPath, $configPath, $ServiceName
  if ($existingService) {
    Invoke-Sc -Arguments @("config", $ServiceName, "binPath=", $serviceCommand, "start=", "auto")
  } else {
    Invoke-Sc -Arguments @("create", $ServiceName, "binPath=", $serviceCommand, "start=", "auto", "DisplayName=", "AsterFerry Controller", "obj=", "LocalSystem")
  }
  Invoke-Sc -Arguments @("failure", $ServiceName, "actions=", "restart/5000/restart/30000/restart/60000", "reset=", "86400")
  try {
    Start-Service -Name $ServiceName
    Start-Sleep -Milliseconds 750
    $startedService = Get-Service -Name $ServiceName
    if ($startedService.Status -ne "Running") {
      $logPath = Join-Path $env:ProgramData ("AsterFerry\logs\" + $ServiceName + ".log")
      $details = if (Test-Path -LiteralPath $logPath) { (Get-Content -LiteralPath $logPath -Tail 20 -ErrorAction SilentlyContinue) -join "`n" } else { "service log was not created" }
      throw "Controller service did not remain running (status: $($startedService.Status)). $details"
    }
  } catch {
    throw "failed to start Controller service '$ServiceName': $($_.Exception.Message)"
  }

  Write-Host "AsterFerry Controller $Version installed and started"
  Write-Host "config: $configPath"
  Write-Host "service: $ServiceName"
  Write-Host "log: $(Join-Path $env:ProgramData ('AsterFerry\logs\' + $ServiceName + '.log'))"
} finally {
  Remove-InstallerAccess
  Remove-Item -LiteralPath $TempRoot -Recurse -Force -ErrorAction SilentlyContinue
}
