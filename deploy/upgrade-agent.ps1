<#
.SYNOPSIS
  Replaces the termhub agent of the current Windows user with a new build (docs/M5).

.DESCRIPTION
  Run as the user the agent runs as. Only the agent's "run" process restarts;
  the session host keeps running, so open sessions are not interrupted. The
  old exe is kept next to the new one as termhub-agent.old-<version>.exe.

.EXAMPLE
  .\upgrade-agent.ps1 -Exe .\termhub-agent.exe
#>
param(
  [Parameter(Mandatory)][string]$Exe,
  [string]$TaskName = 'termhub-agent'
)
$ErrorActionPreference = 'Stop'
$dir = Join-Path $env:LOCALAPPDATA 'termhub'
$cur = Join-Path $dir 'termhub-agent.exe'
if (-not (Test-Path -LiteralPath $cur)) { throw "no agent installed at $cur; use install-agent.ps1" }
if (-not (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue)) { throw "no scheduled task '$TaskName'; pass -TaskName" }
$new = Join-Path $dir 'termhub-agent.new.exe'
Copy-Item -LiteralPath $Exe -Destination $new -Force
# Validate the staged program before replacing a working installation (first release review).
try {
  $newVersion = (& $new version 2>&1 | Out-String).Trim()
  if ($LASTEXITCODE -ne 0 -or $newVersion -notmatch '^termhub-agent [0-9]+\.[0-9]+\.[0-9]+(?:[-+][^\s]+)? \(protocol [0-9]+\.[0-9]+\)$') {
    throw 'not a supported termhub-agent executable'
  }
} catch {
  throw "new agent validation failed; the installed program and task were not changed: $($_.Exception.Message)"
}
$oldVer = ((& $cur version) -split ' ')[1]
$backup = "termhub-agent.old-$oldVer.exe"
if (Test-Path (Join-Path $dir $backup)) { $backup = $backup -replace '\.exe$', "-$(Get-Date -Format HHmmss).exe" }
# only this user's agent: other users' installs live in their own profiles
$run = Get-CimInstance Win32_Process -Filter "Name='termhub-agent.exe'" |
  Where-Object { $_.ExecutablePath -eq $cur -and $_.CommandLine -match ' run(\s|$)' }
# a running exe can be renamed but not overwritten
Rename-Item -LiteralPath $cur -NewName $backup
Move-Item -LiteralPath $new -Destination $cur
foreach ($p in $run) {
  Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
  # the task's headless conhost, not a shell the agent was started from by hand
  $parent = Get-CimInstance Win32_Process -Filter "ProcessId=$($p.ParentProcessId)" -ErrorAction SilentlyContinue
  if ($parent -and $parent.Name -eq 'conhost.exe') { Stop-Process -Id $parent.ProcessId -Force -ErrorAction SilentlyContinue }
}
# The task counts as running until its conhost is gone; a start before that is ignored.
for ($i = 0; $i -lt 30 -and (Get-ScheduledTask -TaskName $TaskName).State -eq 'Running'; $i++) { Start-Sleep -Milliseconds 500 }
$up = $null
for ($try = 0; $try -lt 3 -and -not $up; $try++) {
  Start-ScheduledTask -TaskName $TaskName
  Start-Sleep -Seconds 6
  $up = Get-CimInstance Win32_Process -Filter "Name='termhub-agent.exe'" | Where-Object { $_.ExecutablePath -eq $cur -and $_.CommandLine -match ' run(\s|$)' }
}
if (-not $up) { throw "the new agent did not stay running; see $dir\agent\logs\agent.log (old exe: $backup)" }
"upgraded: $oldVer -> " + (& $cur version)
