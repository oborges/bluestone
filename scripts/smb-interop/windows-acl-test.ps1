param([Parameter(Mandatory=$true)][string]$Server, [string]$Share='bluestone', [string]$User='acltest', [string]$Domain='BLUESTONE')
$password = [Console]::In.ReadLine()
$unc = "\\$Server\$Share"
cmd /c "net use $unc /delete /y" 2>&1 | Out-Null
$out = cmd /c "net use $unc /user:$Domain\$User $password" 2>&1
if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }

$dir = "$unc\acltest"
if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
$file = "$dir\file.txt"
Set-Content -Path $file -Value 'security descriptor test'

Write-Output '=== Get-Acl on a file (what the Security tab reads)'
try {
    $acl = Get-Acl -Path $file
    Write-Output "owner: $($acl.Owner)"
    Write-Output "group: $($acl.Group)"
    foreach ($ace in $acl.Access) {
        Write-Output "ace  : $($ace.IdentityReference) $($ace.FileSystemRights) $($ace.AccessControlType) inherited=$($ace.IsInherited)"
    }
    Write-Output "sddl : $($acl.Sddl)"
} catch { Write-Output "Get-Acl FAILED: $($_.Exception.Message.Split([char]13)[0])" }

Write-Output '=== Get-Acl on a directory'
try {
    $dacl = Get-Acl -Path $dir
    foreach ($ace in $dacl.Access) {
        Write-Output "ace  : $($ace.IdentityReference) $($ace.FileSystemRights) inheritflags=$($ace.InheritanceFlags)"
    }
} catch { Write-Output "Get-Acl on directory FAILED: $($_.Exception.Message.Split([char]13)[0])" }

Write-Output '=== Set-Acl (the gateway stores no ACLs, so this should fail cleanly)'
try {
    $acl = Get-Acl -Path $file
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule('Everyone','Read','Allow')
    $acl.AddAccessRule($rule)
    Set-Acl -Path $file -AclObject $acl -ErrorAction Stop
    Write-Output 'Set-Acl: accepted (unexpected)'
} catch { Write-Output "Set-Acl: refused ($($_.Exception.Message.Split([char]13)[0]))" }

Write-Output '=== file still usable afterwards'
Write-Output "content: $(Get-Content $file)"
Remove-Item $file -Force; Remove-Item $dir -Force
cmd /c "net use $unc /delete /y" 2>&1 | Out-Null
Write-Output 'done'
