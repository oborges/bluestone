# Checks that an open file survives the connection dropping: write and lock
# through one handle, wait while the connection is cut, then go on using the
# same handle. Run the cut from the server side during -Wait.
param([Parameter(Mandatory=$true)][string]$Unc, [string]$User, [string]$Domain='BLUESTONE', [int]$Wait=25)
if ($User) {
    $password = [Console]::In.ReadLine()
    cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null
    $out = cmd /c "net use $Unc /user:$Domain\$User $password" 2>&1
    if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }
}
$dir = Join-Path $Unc 'durabletest'
if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
$file = Join-Path $dir 'held.txt'
function Step($label, $block) {
    $t = (Get-Date).ToString('HH:mm:ss')
    try { $r = & $block; Write-Output ("{0} {1,-40} ok {2}" -f $t, $label, $r) }
    catch { Write-Output ("{0} {1,-40} FAILED: {2}" -f $t, $label, $_.Exception.InnerException.Message + $_.Exception.Message.Split([Environment]::NewLine)[0]) }
}
$fs = New-Object System.IO.FileStream($file, 'Create', 'ReadWrite', 'ReadWrite')
$enc = [System.Text.Encoding]::ASCII
Step 'write before the drop' { $b = $enc.GetBytes('part one|'); $fs.Write($b, 0, $b.Length); $fs.Flush() }
Step 'lock bytes 0-9' { $fs.Lock(0, 10) }
Write-Output "WAITING $Wait s (cut the connection now)"
Start-Sleep -Seconds $Wait
Step 'write after the drop, same handle' { $b = $enc.GetBytes('part two'); $fs.Write($b, 0, $b.Length); $fs.Flush() }
Step 'read back through the same handle' { $fs.Seek(0, 'Begin') | Out-Null; $buf = New-Object byte[] 64; $n = $fs.Read($buf, 0, 64); $enc.GetString($buf, 0, $n) }
Step 'unlock the lock taken before the drop' { $fs.Unlock(0, 10) }
Step 'close' { $fs.Close() }
Step 'content after close' { [System.IO.File]::ReadAllText($file) }
Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
if ($User) { cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null }
Write-Output done
