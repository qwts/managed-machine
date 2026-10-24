# Verifying, non-elevating, profile-free installer for the managed-machine Go
# core on Windows (windows/amd64, windows/arm64; S6 R04, decision D4).
#
# Fetches SHA256SUMS from -ReleaseBase / $env:MANAGED_MACHINE_RELEASE_BASE
# (default: https://github.com/qwts/managed-machine/releases/latest/download,
# or any https:// URL or absolute directory), selects the _windows_<arch>.zip
# archive for the host, verifies its SHA-256 digest with Get-FileHash before
# extracting anything, and installs managed-machine.exe atomically into -Prefix
# (default: %LOCALAPPDATA%\Programs\managed-machine).
#
# Exits:
#   0  installed or already identical
#   1  download, digest or extraction failure (one Error: line, nothing left behind)
#   2  usage or unsupported host
param(
    [string]$ReleaseBase = "",
    [string]$Prefix = "",
    [switch]$Help,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ExtraArgs
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Write-Usage {
    Write-Output @"
Usage: install-core.ps1 [-Prefix <dir>] [-ReleaseBase <url-or-dir>]

Install the prebuilt managed-machine Go core binary on Windows (windows/amd64,
windows/arm64) after verifying SHA256SUMS.

Options:
  -Prefix <dir>              Installation directory (default: %LOCALAPPDATA%\Programs\managed-machine)
  -ReleaseBase <url-or-dir>  Release base URL (https://...) or absolute directory
                             (default: `$env:MANAGED_MACHINE_RELEASE_BASE or
                             https://github.com/qwts/managed-machine/releases/latest/download)
  -Help                      Show this help and exit
"@
}

function Fail-Install([string]$Message) {
    $clean = ($Message -replace "[\r\n]+", " ").Trim()
    [Console]::Error.WriteLine("Error: $clean")
    exit 1
}

function Fail-Usage([string]$Message) {
    $clean = ($Message -replace "[\r\n]+", " ").Trim()
    [Console]::Error.WriteLine("Error: $clean")
    exit 2
}

if ($Help) {
    Write-Usage
    exit 0
}
if ($null -ne $ExtraArgs -and $ExtraArgs.Count -gt 0) {
    foreach ($arg in $ExtraArgs) {
        if ($arg -eq "--help" -or $arg -eq "-h" -or $arg -eq "/?") {
            Write-Usage
            exit 0
        }
    }
    Fail-Usage ("unknown argument: " + ($ExtraArgs -join " "))
}

$osRaw = if ($env:MANAGED_MACHINE_TEST_OS) {
    $env:MANAGED_MACHINE_TEST_OS
} elseif ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) {
    "windows"
} else {
    [System.Environment]::OSVersion.Platform.ToString()
}

if ($osRaw.Trim().ToLowerInvariant() -ne "windows") {
    Fail-Usage "unsupported operating system '$osRaw'; install-core.ps1 supports Windows (windows/amd64, windows/arm64) only"
}

$archRaw = if ($env:MANAGED_MACHINE_TEST_ARCH) {
    $env:MANAGED_MACHINE_TEST_ARCH
} elseif ($env:PROCESSOR_ARCHITEW6432) {
    $env:PROCESSOR_ARCHITEW6432
} elseif ($env:PROCESSOR_ARCHITECTURE) {
    $env:PROCESSOR_ARCHITECTURE
} else {
    [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
}

$archLower = $archRaw.Trim().ToLowerInvariant()
$targetArch = switch ($archLower) {
    "amd64"   { "amd64" }
    "x86_64"  { "amd64" }
    "x64"     { "amd64" }
    "arm64"   { "arm64" }
    "aarch64" { "arm64" }
    default   { "" }
}
if ($targetArch -eq "") {
    Fail-Usage "unsupported architecture '$archRaw' on windows; supported: windows/amd64, windows/arm64"
}

function Test-AbsolutePath([string]$PathValue) {
    if ([string]::IsNullOrWhiteSpace($PathValue)) {
        return $false
    }
    if (-not [System.IO.Path]::IsPathRooted($PathValue)) {
        return $false
    }
    if ($PathValue -match '^[A-Za-z]:[\\/]' -or $PathValue.StartsWith('\\') -or $PathValue.StartsWith('/')) {
        return $true
    }
    return $false
}

if ([string]::IsNullOrEmpty($Prefix)) {
    if ([string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
        Fail-Install "LOCALAPPDATA is not set and -Prefix was not provided"
    }
    if (-not (Test-AbsolutePath $env:LOCALAPPDATA)) {
        Fail-Install "LOCALAPPDATA must be an absolute path (got '$($env:LOCALAPPDATA)')"
    }
    $Prefix = Join-Path (Join-Path $env:LOCALAPPDATA "Programs") "managed-machine"
} else {
    if (-not (Test-AbsolutePath $Prefix)) {
        Fail-Usage "-Prefix must be an absolute path (got '$Prefix')"
    }
}
if ($Prefix.Length -gt 3) {
    $Prefix = $Prefix.TrimEnd('\', '/')
}

if ([string]::IsNullOrEmpty($ReleaseBase)) {
    if ($null -ne $env:MANAGED_MACHINE_RELEASE_BASE) {
        $ReleaseBase = $env:MANAGED_MACHINE_RELEASE_BASE
    } else {
        $ReleaseBase = "https://github.com/qwts/managed-machine/releases/latest/download"
    }
}

if ([string]::IsNullOrWhiteSpace($ReleaseBase)) {
    Fail-Usage "release base must not be empty"
}

$baseKind = ""
if ($ReleaseBase -match '^https://.+') {
    $baseKind = "https"
    $ReleaseBase = $ReleaseBase.TrimEnd('/')
} elseif ($ReleaseBase -match '^http://') {
    Fail-Usage "plain http release base is not allowed; use https:// or an absolute directory path (got '$ReleaseBase')"
} elseif ($ReleaseBase -match '^[A-Za-z][A-Za-z0-9+.-]*://') {
    Fail-Usage "release base must be an https:// URL or an absolute directory path (got '$ReleaseBase')"
} elseif (Test-AbsolutePath $ReleaseBase) {
    $baseKind = "dir"
    if ($ReleaseBase.Length -gt 3) {
        $ReleaseBase = $ReleaseBase.TrimEnd('\', '/')
    }
} else {
    Fail-Usage "release base must be an https:// URL or an absolute directory path (got '$ReleaseBase')"
}

function Download-HttpsFile([string]$Url, [string]$DestPath) {
    try {
        [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
        if ([System.Enum]::GetNames([System.Net.SecurityProtocolType]) -contains 'Tls13') {
            [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12 -bor [System.Net.SecurityProtocolType]::Tls13
        }
    } catch {
        # Modern .NET manages TLS protocols on the handler directly.
    }

    $currentUri = $null
    if (-not [System.Uri]::TryCreate($Url, [System.UriKind]::Absolute, [ref]$currentUri)) {
        Fail-Install "invalid URL: $Url"
    }

    for ($hop = 0; $hop -lt 10; $hop++) {
        if ($currentUri.Scheme -ne "https") {
            Fail-Install "refusing non-https URL: $($currentUri.AbsoluteUri)"
        }
        $response = $null
        try {
            $req = [System.Net.HttpWebRequest]::CreateHttp($currentUri)
            $req.Method = "GET"
            $req.AllowAutoRedirect = $false
            $req.Timeout = 30000
            $req.ReadWriteTimeout = 30000
            $response = [System.Net.HttpWebResponse]$req.GetResponse()
        } catch [System.Net.WebException] {
            if ($null -ne $_.Exception.Response) {
                $response = [System.Net.HttpWebResponse]$_.Exception.Response
            } else {
                Fail-Install "failed to download $($currentUri.AbsoluteUri): $($_.Exception.Message)"
            }
        } catch {
            Fail-Install "failed to download $($currentUri.AbsoluteUri): $($_.Exception.Message)"
        }

        try {
            $code = [int]$response.StatusCode
            if ($code -in @(301, 302, 303, 307, 308)) {
                $loc = $response.Headers["Location"]
                if ([string]::IsNullOrWhiteSpace($loc)) {
                    Fail-Install "redirect from $($currentUri.AbsoluteUri) missing Location header"
                }
                $nextUri = $null
                if (-not [System.Uri]::TryCreate($currentUri, $loc, [ref]$nextUri)) {
                    Fail-Install "invalid redirect Location '$loc' from $($currentUri.AbsoluteUri)"
                }
                if ($nextUri.Scheme -ne "https") {
                    Fail-Install "refusing redirect to non-https URL: $($nextUri.AbsoluteUri)"
                }
                $currentUri = $nextUri
                continue
            }
            if ($code -ne 200) {
                Fail-Install "failed to download $($currentUri.AbsoluteUri): HTTP $code"
            }
            $inStream = $response.GetResponseStream()
            try {
                $outStream = [System.IO.File]::Create($DestPath)
                try {
                    $inStream.CopyTo($outStream)
                } finally {
                    $outStream.Dispose()
                }
            } finally {
                if ($null -ne $inStream) {
                    $inStream.Dispose()
                }
            }
            return
        } finally {
            if ($null -ne $response) {
                $response.Close()
            }
        }
    }
    Fail-Install "too many redirects downloading $Url"
}

function Fetch-Resource([string]$ResourceName, [string]$DestPath) {
    if ($baseKind -eq "dir") {
        $srcPath = Join-Path $ReleaseBase $ResourceName
        if (-not (Test-Path -LiteralPath $srcPath -PathType Leaf)) {
            Fail-Install "missing $ResourceName in $ReleaseBase"
        }
        Copy-Item -LiteralPath $srcPath -Destination $DestPath -Force
    } else {
        $url = "$ReleaseBase/$ResourceName"
        Download-HttpsFile $url $DestPath
    }
}

$workDir = ""
$stageBin = ""
try {
    $tempBase = [System.IO.Path]::GetTempPath()
    $workDir = Join-Path $tempBase ("managed-machine-install-core-" + [System.Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $workDir -Force | Out-Null

    $sumsFile = Join-Path $workDir "SHA256SUMS"
    Fetch-Resource "SHA256SUMS" $sumsFile

    $suffix = "_windows_${targetArch}.zip"
    $pattern = '^([0-9A-Fa-f]{64})  (managed-machine_[^/\s\\]+' + [regex]::Escape($suffix) + ')$'
    $matchedSha = ""
    $matchedArchive = ""
    $matchCount = 0

    foreach ($rawLine in [System.IO.File]::ReadLines($sumsFile)) {
        $line = $rawLine.TrimEnd("`r", "`n")
        if ($line -match $pattern) {
            $shaCandidate = $Matches[1].ToLowerInvariant()
            $nameCandidate = $Matches[2]
            if (-not $nameCandidate.Contains("..")) {
                $matchedSha = $shaCandidate
                $matchedArchive = $nameCandidate
                $matchCount++
            }
        }
    }

    if ($matchCount -eq 0) {
        Fail-Install "SHA256SUMS has no valid entry ending in $suffix"
    }
    if ($matchCount -gt 1) {
        Fail-Install "SHA256SUMS has multiple entries ending in $suffix"
    }

    $archiveFile = Join-Path $workDir $matchedArchive
    Fetch-Resource $matchedArchive $archiveFile

    $actualSha = (Get-FileHash -LiteralPath $archiveFile -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha -ne $matchedSha) {
        Fail-Install "SHA-256 digest mismatch for ${matchedArchive}: expected $matchedSha, got $actualSha"
    }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $extractDir = Join-Path $workDir "extract"
    New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
    [System.IO.Compression.ZipFile]::ExtractToDirectory($archiveFile, $extractDir)

    $extractedBin = Join-Path $extractDir "managed-machine.exe"
    if (-not (Test-Path -LiteralPath $extractedBin -PathType Leaf)) {
        Fail-Install "archive $matchedArchive did not contain managed-machine.exe"
    }

    $binSha = (Get-FileHash -LiteralPath $extractedBin -Algorithm SHA256).Hash.ToLowerInvariant()
    $destBin = Join-Path $Prefix "managed-machine.exe"

    if (Test-Path -LiteralPath $destBin -PathType Leaf) {
        $existingSha = (Get-FileHash -LiteralPath $destBin -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($existingSha -eq $binSha) {
            Write-Output "managed-machine is already up to date at $destBin (sha256:$binSha)"
            Write-Output "Ensure $Prefix is on your PATH:"
            Write-Output "  `$env:PATH = `"$Prefix;`$env:PATH`""
            exit 0
        }
    }

    if (-not (Test-Path -LiteralPath $Prefix -PathType Container)) {
        New-Item -ItemType Directory -Path $Prefix -Force | Out-Null
    }
    $stageBin = Join-Path $Prefix (".managed-machine.exe.tmp." + [System.Guid]::NewGuid().ToString("N"))
    Copy-Item -LiteralPath $extractedBin -Destination $stageBin -Force
    Move-Item -LiteralPath $stageBin -Destination $destBin -Force
    $stageBin = ""

    Write-Output "Installed managed-machine to $destBin (sha256:$binSha)"
    Write-Output "Ensure $Prefix is on your PATH:"
    Write-Output "  `$env:PATH = `"$Prefix;`$env:PATH`""
} catch {
    Fail-Install $_.Exception.Message
} finally {
    if ($stageBin -ne "" -and (Test-Path -LiteralPath $stageBin)) {
        Remove-Item -LiteralPath $stageBin -Force -ErrorAction SilentlyContinue
    }
    if ($workDir -ne "" -and (Test-Path -LiteralPath $workDir)) {
        Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
