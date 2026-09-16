# Check that overwriting an existing file replaces its content.

param(
    [Parameter(Mandatory = $true)][string]$Server,
    [string]$Share  = 'bluestone',
    [string]$User   = 'bluestone',
    [string]$Domain = 'BLUESTONE'
)

$password = [Console]::In.ReadLine()
$share = "\\$Server\$Share"
$user  = "$Domain\$User"
cmd /c "net use $share /delete /y" 2>&1 | Out-Null
$out = cmd /c "net use $share /user:$user $password" 2>&1
if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }

$path = "$share\overwrite.txt"
Remove-Item $path -ErrorAction SilentlyContinue

Write-Output '=== 1. create with a long line'
[System.IO.File]::WriteAllText($path, ('A' * 200))
Write-Output ("length: " + (Get-Item $path).Length)

Write-Output '=== 2. overwrite with a short line (WriteAllText, CREATE_ALWAYS)'
[System.IO.File]::WriteAllText($path, 'short')
Write-Output ("length: " + (Get-Item $path).Length + "  content: " + [System.IO.File]::ReadAllText($path))

Write-Output '=== 3. overwrite with Set-Content'
Set-Content -Path $path -Value 'set-content line'
Write-Output ("length: " + (Get-Item $path).Length + "  content: " + [System.IO.File]::ReadAllText($path).Trim())

Write-Output '=== 4. overwrite again with Set-Content (same length)'
Set-Content -Path $path -Value 'set-content line'
Write-Output ("length: " + (Get-Item $path).Length + "  content: " + [System.IO.File]::ReadAllText($path).Trim())

Write-Output '=== 5. explicit truncate via FileStream'
$fs = [System.IO.File]::Open($path, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write)
$bytes = [System.Text.Encoding]::UTF8.GetBytes('fs-create')
$fs.Write($bytes, 0, $bytes.Length)
$fs.Close()
Write-Output ("length: " + (Get-Item $path).Length + "  content: " + [System.IO.File]::ReadAllText($path))

cmd /c "net use $share /delete /y" 2>&1 | Out-Null
Write-Output 'done'
