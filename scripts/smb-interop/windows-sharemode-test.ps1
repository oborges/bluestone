# Check that share modes are enforced: a file held without sharing must block
# other opens, and opens that permit each other must coexist.

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

$path = "$share\sharemode.txt"
Remove-Item $path -Force -ErrorAction SilentlyContinue
[System.IO.File]::WriteAllText($path, 'held open by the first client')

function Try-Open($label, $mode, $access, $sharing) {
    try {
        $stream = [System.IO.File]::Open($path, $mode, $access, $sharing)
        $stream.Close()
        Write-Output "$label : opened"
    } catch [System.IO.IOException] {
        Write-Output "$label : refused ($($_.Exception.Message.Split([char]13)[0]))"
    } catch {
        Write-Output "$label : error $($_.Exception.GetType().Name) $($_.Exception.Message)"
    }
}

Write-Output '=== nothing open yet'
Try-Open 'read while free' 'Open' 'Read' 'ReadWrite'

Write-Output '=== holding the file with FileShare.None'
$held = [System.IO.File]::Open($path, 'Open', 'ReadWrite', 'None')
Try-Open 'read while held exclusively  ' 'Open' 'Read' 'ReadWrite'
Try-Open 'write while held exclusively ' 'Open' 'Write' 'ReadWrite'
$held.Close()
Write-Output 'released'

Write-Output '=== holding the file with FileShare.Read'
$held = [System.IO.File]::Open($path, 'Open', 'Read', 'Read')
Try-Open 'read while readers are shared' 'Open' 'Read' 'Read'
Try-Open 'write while readers are shared' 'Open' 'Write' 'ReadWrite'
$held.Close()
Write-Output 'released'

Write-Output '=== after everything closed'
Try-Open 'write once free' 'Open' 'Write' 'ReadWrite'

Write-Output '=== listing the directory while a file is held exclusively'
$held = [System.IO.File]::Open($path, 'Open', 'ReadWrite', 'None')
try {
    $names = (Get-ChildItem $share | Select-Object -ExpandProperty Name) -join ', '
    Write-Output "listing: $names"
} catch { Write-Output "LISTING FAILED: $($_.Exception.Message)" }
$held.Close()

Remove-Item $path -Force -ErrorAction SilentlyContinue
cmd /c "net use $share /delete /y" 2>&1 | Out-Null
Write-Output 'done'
