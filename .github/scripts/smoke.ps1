# Checks an installed toli works, as a new user would first use it: the
# daemon starts, and an agent can be added and given an MCP URL.
$ErrorActionPreference = "Stop"

$log = Join-Path $env:RUNNER_TEMP "toli-daemon.log"
$daemon = Start-Process toli -ArgumentList "daemon", "--log-file", $log -WindowStyle Hidden -PassThru
try {
  for ($i = 0; ; $i++) {
    toli agent list *> $null
    if ($LASTEXITCODE -eq 0) { break }
    if ($i -ge 60) { Get-Content $log; throw "the daemon didn't start" }
    Start-Sleep -Milliseconds 500
  }
  toli agent add smoke
  if ($LASTEXITCODE -ne 0) { throw "toli agent add failed" }
  $out = toli agent new-token smoke
  $out
  if (-not ($out | Select-String "^mcp_url\s+http://127.0.0.1:7420/mcp/toli_")) { throw "toli agent new-token didn't print an MCP URL" }
  Write-Host "toli works"
} finally {
  Stop-Process -Id $daemon.Id -Force
}
