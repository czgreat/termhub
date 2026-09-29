<#
.SYNOPSIS
  Installs the termhub agent for the current Windows user and starts it (docs/M5).

.DESCRIPTION
  Run it as the Windows user whose terminals, CLI logins and files termhub
  should use. The values for -Hub and -Pin come from the Hub (管理 → 节点 →
  添加节点, or the cert_fingerprint line of the Hub's log); leave out -Token
  and the script asks for it, so the token stays out of the shell history.

  1. Copies termhub-agent.exe to %LOCALAPPDATA%\termhub.
  2. Enrolls: the agent checks the address, the certificate fingerprint and the
     token with a real connection and stores the token sealed with DPAPI for
     this user. No task is registered if that fails.
  3. Registers the scheduled task (default name "termhub-agent") and starts it.

  By default the task starts when this user logs on (3 minutes later), from a
  normal PowerShell window. With -AtStartup it starts at boot without anyone
  logging on: Windows then needs this user's password once and keeps it in the
  Task Scheduler (termhub never stores it), and only an elevated window
  ("Run as administrator") may register a start-at-boot task. The user needs
  the "Log on as a batch job" right, which administrators have; for a
  standard user an administrator grants it once.

.EXAMPLE
  .\install-agent.ps1 -Hub https://192.168.1.10:27443 -Pin 3f5a...
.EXAMPLE
  .\install-agent.ps1 -Hub https://192.168.1.10:27443 -Pin 3f5a... -Name PC1 -AtStartup   # elevated window
#>
param(
  [Parameter(Mandatory)][string]$Hub,
  [string]$Token = '',
  [string]$Pin = '',
  [string]$Name = '',
  [string]$Exe = (Join-Path $PSScriptRoot 'termhub-agent.exe'),
  [switch]$AtStartup,
  # the password for -AtStartup; asked for when not given
  [pscredential]$Credential,
  [ValidateRange(0, 60)][int]$DelayMinutes = 3,
  [string]$TaskName = 'termhub-agent'
)
$ErrorActionPreference = 'Stop'
$dir = Join-Path $env:LOCALAPPDATA 'termhub'
$target = Join-Path $dir 'termhub-agent.exe'

if (-not (Test-Path -LiteralPath $Exe)) { throw "termhub-agent.exe not found at $Exe; pass -Exe <path>" }
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
  throw "the scheduled task '$TaskName' already exists. To upgrade use upgrade-agent.ps1; to reinstall run uninstall-agent.ps1 first; for a second Windows user on this machine pass another -TaskName."
}
$elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if ($AtStartup -and -not $elevated) {
  throw '-AtStartup registers a start-at-boot task, which Windows allows only from an elevated window: run PowerShell as administrator (same user) and try again.'
}
if (-not $Token) {
  $t = Read-Host -AsSecureString 'Node token (thn_..., shown once by the Hub)'
  $Token = [Net.NetworkCredential]::new('', $t).Password
}
$me = "$env:USERDOMAIN\$env:USERNAME"
if ($AtStartup -and -not $Credential) {
  $Credential = Get-Credential -UserName $me -Message 'Windows password of this user, kept by the Task Scheduler only'
}

New-Item -ItemType Directory -Force -Path $dir | Out-Null
if ((Resolve-Path -LiteralPath $Exe).Path -ne $target) { Copy-Item -LiteralPath $Exe -Destination $target -Force }

$enroll = @('enroll', '--hub', $Hub, '--token', $Token)
if ($Pin) { $enroll += @('--pin', $Pin) }
if ($Name) { $enroll += @('--name', $Name) }
& $target @enroll
if ($LASTEXITCODE -ne 0) { throw "enroll failed (exit $LASTEXITCODE); no scheduled task was registered" }
$Token = $null

# conhost --headless gives the agent a console without a window, as the
# sessions' shells expect one.
$conhost = Join-Path $env:WINDIR 'System32\conhost.exe'
$action = New-ScheduledTaskAction -Execute $conhost -Argument "--headless `"$target`" run" -WorkingDirectory $dir
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
$desc = 'termhub agent: connects this machine to the termhub Hub'
if ($AtStartup) {
  $trigger = New-ScheduledTaskTrigger -AtStartup
  if ($DelayMinutes) { $trigger.Delay = "PT${DelayMinutes}M" }
  Register-ScheduledTask -TaskName $TaskName -Description $desc -Action $action -Trigger $trigger -Settings $settings `
    -User $me -Password $Credential.GetNetworkCredential().Password -RunLevel Limited | Out-Null
} else {
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $me
  if ($DelayMinutes) { $trigger.Delay = "PT${DelayMinutes}M" }
  $principal = New-ScheduledTaskPrincipal -UserId $me -LogonType Interactive -RunLevel Limited
  Register-ScheduledTask -TaskName $TaskName -Description $desc -Action $action -Trigger $trigger -Settings $settings `
    -Principal $principal | Out-Null
}
Start-ScheduledTask -TaskName $TaskName
$running = $null
for ($i = 0; $i -lt 10 -and -not $running; $i++) {
  Start-Sleep -Seconds 1
  $running = Get-CimInstance Win32_Process -Filter "Name='termhub-agent.exe'" |
    Where-Object { $_.ExecutablePath -eq $target -and $_.CommandLine -match ' run(\s|$)' }
}
if (-not $running) { throw "the task '$TaskName' was registered but the agent is not running; see $dir\agent\logs\agent.log" }
"installed: $target (task '$TaskName'). The node should show as online in the Hub within a few seconds."
