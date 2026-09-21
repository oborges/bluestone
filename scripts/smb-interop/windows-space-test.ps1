param([Parameter(Mandatory=$true)][string]$Server, [string]$Share='bluestone', [string]$User='spacetest', [string]$Domain='BLUESTONE')
$password = [Console]::In.ReadLine()
$unc = "\\$Server\$Share"
cmd /c "net use Z: /delete /y" 2>&1 | Out-Null
$out = cmd /c "net use Z: $unc /user:$Domain\$User $password" 2>&1
if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }

function Show-Space($label) {
    $d = New-Object System.IO.DriveInfo 'Z'
    Write-Host ("{0}: total {1:N3} GiB, free {2:N3} GiB" -f $label, ($d.TotalSize/1GB), ($d.AvailableFreeSpace/1GB))
    return $d.AvailableFreeSpace
}

Write-Output "=== what Windows reads as the share's size"
$before = Show-Space 'before'
cmd /c "fsutil volume diskfree Z:" 2>&1 | ForEach-Object { "  fsutil: $_" }

$dir = 'Z:\spacetest'
if (Test-Path $dir) { Remove-Item $dir -Recurse -Force }
New-Item -ItemType Directory -Path $dir | Out-Null

Write-Output "=== free space falls as unsynced data is staged"
$bytes = New-Object byte[] (64MB)
[System.IO.File]::WriteAllBytes("$dir\staged.bin", $bytes)
$after = Show-Space 'after 64 MiB'
Write-Output ("free fell by {0:N1} MiB" -f (($before - $after)/1MB))

Write-Output "=== a write past the staging quota"
try {
    $fs = [System.IO.File]::Open("$dir\huge.bin", 'Create', 'Write')
    $fs.Seek(12GB, 'Begin') | Out-Null
    $fs.WriteByte(1)
    $fs.Flush()
    $fs.Close()
    Write-Output "UNEXPECTED: the write succeeded"
} catch {
    Write-Output ("refused: HResult 0x{0:X8}: {1}" -f $_.Exception.InnerException.HResult, $_.Exception.InnerException.Message)
    if ($fs) { try { $fs.Dispose() } catch {} }
}

Remove-Item $dir -Recurse -Force
cmd /c "net use Z: /delete /y" 2>&1 | Out-Null
Write-Output done
