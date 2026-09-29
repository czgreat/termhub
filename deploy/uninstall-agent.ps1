<#
.SYNOPSIS
  Removes the termhub agent of the current Windows user (docs/M5).

.DESCRIPTION
  Stops and removes the scheduled task, ends this user's agent and session
  host (and with them every open termhub session of this user), and deletes
  %LOCALAPPDATA%\termhub. Delete the node in the Hub (管理 → 节点) as well.
  The official CLIs and their history files are not touched.
#>
param([string]$TaskName = 'termhub-agent')
$ErrorActionPreference = 'Stop'
$dir = Join-Path $env:LOCALAPPDATA 'termhub'
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
  Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
}
Get-CimInstance Win32_Process -Filter "Name='termhub-agent.exe'" |
  Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($dir + '\', [StringComparison]::OrdinalIgnoreCase) } |
  ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Start-Sleep -Seconds 2
if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force }
"removed. Delete the node in the Hub too."
