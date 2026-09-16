# Exercise the Bluestone SMB share from Windows.
# The share password is read from stdin so it never appears in a command line.

param(
    [Parameter(Mandatory = $true)][string]$Server,
    [string]$Share  = 'bluestone',
    [string]$User   = 'bluestone',
    [string]$Domain = 'BLUESTONE'
)

$ErrorActionPreference = 'Continue'
$server = $Server
$base   = "\\$server\$Share"
$user   = "$Domain\$User"

$password = [Console]::In.ReadLine()
if ([string]::IsNullOrWhiteSpace($password)) { Write-Output 'no password on stdin'; exit 1 }

function Step($name) { Write-Output "=== $name" }

Step 'authenticate to the share'
cmd /c "net use $base /delete /y" 2>&1 | Out-Null
$out = cmd /c "net use $base /user:$user $password" 2>&1
if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }
Write-Output 'connected'

Step 'SMB connection details'
Get-SmbConnection -ServerName $server | Select-Object ShareName, Dialect, Encrypted, Signed | Format-Table | Out-String | Write-Output

Step 'list root'
Get-ChildItem $base | Select-Object Mode, Length, Name | Format-Table | Out-String | Write-Output

Step 'create directory'
if (-not (Test-Path "$base\wintest")) { New-Item -ItemType Directory -Path "$base\wintest" | Out-Null }
Write-Output "exists: $(Test-Path "$base\wintest")"

Step 'write and read back a text file'
$text = "written from Windows at $(Get-Date -Format o)"
Set-Content -Path "$base\wintest\notes.txt" -Value $text
$read = (Get-Content -Path "$base\wintest\notes.txt" -Raw).Trim()
Write-Output "round trip: $(if ($read -eq $text) { 'MATCH' } else { "MISMATCH: $read" })"

Step 'copy a 5 MiB binary file and compare hashes'
$local = "$env:TEMP\big.bin"
$bytes = New-Object byte[] (5MB)
(New-Object Random 42).NextBytes($bytes)
[IO.File]::WriteAllBytes($local, $bytes)
Copy-Item $local "$base\wintest\big.bin"
$h1 = (Get-FileHash $local -Algorithm SHA256).Hash
$h2 = (Get-FileHash "$base\wintest\big.bin" -Algorithm SHA256).Hash
Write-Output "5 MiB copy: $(if ($h1 -eq $h2) { 'MATCH' } else { 'MISMATCH' })"

Step 'case-insensitive access'
Write-Output "NOTES.TXT readable: $(Test-Path "$base\WINTEST\NOTES.TXT")"

Step 'attributes and timestamps'
$item = Get-Item "$base\wintest\notes.txt"
Write-Output "attributes: $($item.Attributes)"
Write-Output "creation: $($item.CreationTime)  write: $($item.LastWriteTime)"
try {
    $item.Attributes = 'ReadOnly, Hidden'
    $again = Get-Item "$base\wintest\notes.txt" -Force
    Write-Output "after setting read-only+hidden: $($again.Attributes)"
    $again.Attributes = 'Normal'
} catch { Write-Output "SET ATTRIBUTES FAILED: $($_.Exception.Message)" }
try {
    $stamp = Get-Date '2021-03-04 05:06:07'
    $file = Get-Item "$base\wintest\notes.txt"
    $file.CreationTime = $stamp
    $file.LastWriteTime = $stamp
    $check = Get-Item "$base\wintest\notes.txt"
    Write-Output "creation now: $($check.CreationTime)  write now: $($check.LastWriteTime)"
} catch { Write-Output "SET TIMES FAILED: $($_.Exception.Message)" }

Step 'Office-style save (write temp, rename over original)'
Set-Content -Path "$base\wintest\doc.txt" -Value 'version 1'
Set-Content -Path "$base\wintest\doc.tmp" -Value 'version 2'
Move-Item -Path "$base\wintest\doc.tmp" -Destination "$base\wintest\doc.txt" -Force
Write-Output "after replace: $((Get-Content "$base\wintest\doc.txt" -Raw).Trim())"

Step 'append to an existing file'
Add-Content -Path "$base\wintest\doc.txt" -Value 'appended line'
Write-Output "content now: $((Get-Content "$base\wintest\doc.txt" -Raw).Trim() -replace "`r`n", ' | ')"

Step 'rename and delete'
Remove-Item "$base\wintest\renamed.txt" -Force -ErrorAction SilentlyContinue
Rename-Item "$base\wintest\notes.txt" 'renamed.txt'
Write-Output "renamed exists: $(Test-Path "$base\wintest\renamed.txt")"
Remove-Item "$base\wintest\big.bin"
Write-Output "deleted big.bin: $(-not (Test-Path "$base\wintest\big.bin"))"

Step 'free space as Windows sees it'
cmd /c "net use W: /delete /y" 2>&1 | Out-Null
cmd /c "net use W: $base" 2>&1 | Out-Null
cmd /c "fsutil volume diskfree W:" 2>&1 | Select-Object -First 2 | Write-Output
cmd /c "net use W: /delete /y" 2>&1 | Out-Null

Step 'final listing'
Get-ChildItem "$base\wintest" -Force | Select-Object Mode, Length, Name | Format-Table | Out-String | Write-Output

Step 'disconnect'
cmd /c "net use $base /delete /y" 2>&1 | Out-Null
Write-Output 'done'
