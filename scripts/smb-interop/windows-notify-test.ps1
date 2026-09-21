# Checks change notification over SMB with the .NET FileSystemWatcher, which
# Explorer's refresh uses too (ReadDirectoryChangesW -> CHANGE_NOTIFY).
param([Parameter(Mandatory=$true)][string]$Unc, [string]$User, [string]$Domain='BLUESTONE',
      [int]$ExternalWait=20, [int]$Idle=30, [int]$Watchers=20)
if ($User) {
    $password = [Console]::In.ReadLine()
    cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null
    $out = cmd /c "net use $Unc /user:$Domain\$User $password" 2>&1
    if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }
}
$dir = Join-Path $Unc 'notifytest'
if (Test-Path $dir) { Remove-Item $dir -Recurse -Force }
New-Item -ItemType Directory -Path $dir | Out-Null

$global:events = [System.Collections.ArrayList]::Synchronized((New-Object System.Collections.ArrayList))
$w = New-Object System.IO.FileSystemWatcher $dir
$w.IncludeSubdirectories = $true
$w.NotifyFilter = [System.IO.NotifyFilters]'FileName, DirectoryName, Size, LastWrite'
$w.InternalBufferSize = 65536
foreach ($kind in 'Created','Changed','Deleted') {
    Register-ObjectEvent $w $kind -Action { [void]$global:events.Add("$($EventArgs.ChangeType) $($EventArgs.Name)") } | Out-Null
}
Register-ObjectEvent $w 'Renamed' -Action { [void]$global:events.Add("Renamed $($EventArgs.OldName) -> $($EventArgs.Name)") } | Out-Null
Register-ObjectEvent $w 'Error' -Action { [void]$global:events.Add("ERROR $($EventArgs.GetException().Message)") } | Out-Null
$w.EnableRaisingEvents = $true

# Idle watchers, as Explorer windows open on the share would be.
$idlers = @()
for ($i = 0; $i -lt $Watchers; $i++) {
    $x = New-Object System.IO.FileSystemWatcher $dir
    $x.EnableRaisingEvents = $true
    $idlers += $x
}
Start-Sleep -Milliseconds 500

function Show($label) {
    Start-Sleep -Seconds 2
    Write-Output "=== $label"
    $snapshot = @($global:events.ToArray())
    $global:events.Clear()
    if ($snapshot.Count -eq 0) { Write-Output "  (no events)" }
    $snapshot | Select-Object -Unique | ForEach-Object { "  $_" }
}

[System.IO.File]::WriteAllText("$dir\a.txt", 'hello')
[System.IO.File]::AppendAllText("$dir\a.txt", ' again')
Rename-Item "$dir\a.txt" 'b.txt'
New-Item -ItemType Directory -Path "$dir\sub" | Out-Null
[System.IO.File]::WriteAllText("$dir\sub\inner.txt", 'x')
Remove-Item "$dir\b.txt"
Show 'changes made over SMB'

Write-Output "READY for changes from elsewhere ($ExternalWait s)"
Start-Sleep -Seconds $ExternalWait
Show 'changes made elsewhere'

Write-Output "IDLE $Idle s with $($Watchers + 1) watchers"
Start-Sleep -Seconds $Idle
Show 'while idle'

$w.EnableRaisingEvents = $false; $w.Dispose()
$idlers | ForEach-Object { $_.Dispose() }
Get-EventSubscriber | Unregister-Event
Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
if ($User) { cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null }
Write-Output done
