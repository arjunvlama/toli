# Installs toli on Windows:
#
#   irm https://raw.githubusercontent.com/arjunvlama/toli/main/install.ps1 | iex
#
# $env:TOLI_VERSION = "v0.1.0" picks a release (default: the latest).
# $env:TOLI_INSTALL_DIR picks where toli goes (default: %LOCALAPPDATA%\Programs\toli).
# $env:TOLI_DOWNLOAD_URL downloads from somewhere else, e.g. a local build for testing.
$ErrorActionPreference = "Stop"

$repo = "arjunvlama/toli"
$dir = if ($env:TOLI_INSTALL_DIR) { $env:TOLI_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\toli" }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  default { throw "toli: unsupported CPU $env:PROCESSOR_ARCHITECTURE" }
}
$base = if ($env:TOLI_DOWNLOAD_URL) { $env:TOLI_DOWNLOAD_URL } elseif ($env:TOLI_VERSION) { "https://github.com/$repo/releases/download/$env:TOLI_VERSION" } else { "https://github.com/$repo/releases/latest/download" }
$archive = "toli_windows_$arch.zip"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading $archive"
  Invoke-WebRequest "$base/$archive" -OutFile "$tmp\$archive" -UseBasicParsing
  Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt" -UseBasicParsing

  $want = (Get-Content "$tmp\checksums.txt" | Where-Object { $_ -match " $([regex]::Escape($archive))$" }) -split " " | Select-Object -First 1
  $got = (Get-FileHash "$tmp\$archive" -Algorithm SHA256).Hash.ToLower()
  if (-not $want -or $want -ne $got) { throw "toli: checksum mismatch for $archive" }

  Expand-Archive "$tmp\$archive" -DestinationPath $tmp -Force
  New-Item -ItemType Directory -Path $dir -Force | Out-Null
  Copy-Item "$tmp\toli.exe" "$dir\toli.exe" -Force
  Write-Host "Installed toli to $dir\toli.exe"

  $path = [Environment]::GetEnvironmentVariable("Path", "User")
  if (($path -split ";") -notcontains $dir) {
    [Environment]::SetEnvironmentVariable("Path", "$path;$dir", "User")
    Write-Host "Added $dir to your PATH (open a new terminal to use it)"
  }
  Write-Host 'Next: run `toli daemon install`, then `toli`.'
} finally {
  Remove-Item $tmp -Recurse -Force
}
