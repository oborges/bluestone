# Checks Kerberos sign-in and per-share access against a gateway configured
# for an Active Directory domain, from a domain-joined Windows machine.
#
# The gateway is expected to have no local SMB users and two shares:
#   eng  valid_users: [<SID of the group EngGroup>]
#   pub  read_only: true, write_list: ['<DOMAIN>\<Member>']
# Member is in EngGroup and Outsider is not. Their passwords are read from
# <SecretDir>\<user>.txt on this machine, so they never pass through the
# session running this script.
#
# With -SingleSignOn, a scheduled task logged on as Member reads and writes
# eng with no credentials given, as a user at a domain-joined desktop does.
# Member needs the right to log on as a batch job for that.
param(
    [string]$Server = 'gw.bluestone.test',
    [string]$Domain = 'BTEST',
    [string]$Member = 'bsalice',
    [string]$Outsider = 'bsbob',
    [string]$ServerIP = '10.250.64.4',
    [string]$SecretDir = 'C:\ProgramData\bluestone-test',
    [switch]$SingleSignOn
)
$ErrorActionPreference = 'Continue'

function Step($label, [scriptblock]$block) {
    try {
        $r = & $block
        Write-Output ("{0,-52} ok {1}" -f $label, $r)
    } catch {
        Write-Output ("{0,-52} FAILED: {1}" -f $label, $_.Exception.Message.Split([Environment]::NewLine)[0])
    }
}
function Secret($user) { (Get-Content (Join-Path $SecretDir "$user.txt") -Raw).Trim() }
function Connect($unc, $user) {
    $out = cmd /c "net use $unc /user:$Domain\$user $(Secret $user)" 2>&1
    if ($LASTEXITCODE -ne 0) { throw ($out | Where-Object { $_ -match 'error|denied|System' } | Select-Object -First 2) -join ' ' }
    'connected'
}
function Disconnect { cmd /c "net use * /delete /y" 2>&1 | Out-Null; Start-Sleep 2 }
function Refused([scriptblock]$block) {
    try { & $block } catch { return "refused ($($_.Exception.Message.Split([Environment]::NewLine)[0]))" }
    throw 'was allowed'
}

$eng = "\\$Server\eng"
$pub = "\\$Server\pub"
Disconnect

Write-Output "=== $Member, in the group eng admits"
Step "connect to eng" { Connect $eng $Member }
Step "session" { $c = Get-SmbConnection -ServerName $Server | Select-Object -First 1; "$($c.UserName) dialect $($c.Dialect)" }
Step "write eng\$Member.txt" { Set-Content "$eng\$Member.txt" "written by $Member"; 'written' }
Step "read it back" { Get-Content "$eng\$Member.txt" }
Step "owner Windows shows" { (Get-Acl "$eng\$Member.txt").Owner }
Step "make a folder" { New-Item -ItemType Directory "$eng\folder-$Member" -Force | Out-Null; (Get-Acl "$eng\folder-$Member").Owner }
Step "connect to pub (on its write list)" { Connect $pub $Member }
Step "write pub\notice.txt" { Set-Content "$pub\notice.txt" "posted by $Member"; 'written' }
Disconnect

Write-Output "=== $Outsider, not in the group"
Step "connect to eng" { Refused { Connect $eng $Outsider } }
Step "connect to pub (read-only)" { Connect $pub $Outsider }
Step "read pub\notice.txt" { Get-Content "$pub\notice.txt" }
Step "write to pub" { Refused { Set-Content "$pub\$Outsider.txt" 'x' -ErrorAction Stop } }
Step "delete pub\notice.txt" { Refused { Remove-Item "$pub\notice.txt" -ErrorAction Stop } }
Step "make a folder in pub" { Refused { New-Item -ItemType Directory "$pub\folder-$Outsider" -ErrorAction Stop | Out-Null } }
Step "notice.txt still there" { Get-Content "$pub\notice.txt" }
Disconnect

Write-Output "=== by IP address, which only NTLM can use: no local accounts"
Step "connect to \\$ServerIP\eng as $Member" { Refused { Connect "\\$ServerIP\eng" $Member } }
Disconnect

if ($SingleSignOn) {
    Write-Output "=== single sign-on: a logon session of $Member, no credentials given"
    $result = 'C:\Users\Public\bluestone-sso.txt'
    Remove-Item $result -ErrorAction SilentlyContinue
    $script = "C:\Users\Public\bluestone-sso.ps1"
    Set-Content $script @"
`$out = @()
`$out += 'user ' + [Security.Principal.WindowsIdentity]::GetCurrent().Name
try { Set-Content '$eng\sso-$Member.txt' 'single sign-on' -ErrorAction Stop; `$out += 'write ok' } catch { `$out += 'write FAILED: ' + `$_.Exception.Message }
try { `$out += 'read ' + (Get-Content '$eng\sso-$Member.txt' -ErrorAction Stop) } catch { `$out += 'read FAILED: ' + `$_.Exception.Message }
try { `$out += 'list ' + ((Get-ChildItem '$eng' -ErrorAction Stop).Name -join ',') } catch { `$out += 'list FAILED: ' + `$_.Exception.Message }
`$out += (klist | Select-String 'Server: cifs' | ForEach-Object { `$_.Line.Trim() })
Set-Content '$result' `$out
"@
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File $script"
    Register-ScheduledTask -TaskName BluestoneSSO -Action $action -User "$Domain\$Member" -Password (Secret $Member) -Force | Out-Null
    Start-ScheduledTask -TaskName BluestoneSSO
    for ($i = 0; $i -lt 30 -and -not (Test-Path $result); $i++) { Start-Sleep 1 }
    if (Test-Path $result) { Get-Content $result | ForEach-Object { "    $_" } } else { Write-Output "    no result: $((Get-ScheduledTaskInfo BluestoneSSO).LastTaskResult)" }
    Unregister-ScheduledTask -TaskName BluestoneSSO -Confirm:$false
    Remove-Item $script, $result -ErrorAction SilentlyContinue
}
Write-Output done
