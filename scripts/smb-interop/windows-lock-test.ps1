# Check byte-range locking over SMB, including against a lock held elsewhere.
#
# Run with -Range to lock bytes and hold them until Enter is pressed, which is
# how the cross-protocol check holds a lock while another client tries.

param(
    [Parameter(Mandatory = $true)][string]$Server,
    [string]$Share  = 'bluestone',
    [string]$User   = 'bluestone',
    [string]$Domain = 'BLUESTONE',
    [string]$File   = 'locktest.bin',
    # Hold a lock on these bytes and wait, instead of running the checks.
    [long]$HoldOffset = -1,
    [long]$HoldLength = 0,
    [int]$HoldSeconds = 20
)

$password = [Console]::In.ReadLine()
$share = "\\$Server\$Share"
$user  = "$Domain\$User"
cmd /c "net use $share /delete /y" 2>&1 | Out-Null
$out = cmd /c "net use $share /user:$user $password" 2>&1
if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }
$path = "$share\$File"

if (-not (Test-Path $path)) {
    [System.IO.File]::WriteAllBytes($path, (New-Object byte[] 4096))
}

function Try-Lock($label, [long]$offset, [long]$length) {
    $stream = [System.IO.File]::Open($path, 'Open', 'ReadWrite', 'ReadWrite')
    try {
        $stream.Lock($offset, $length)
        Write-Output "$label : locked"
        $stream.Unlock($offset, $length)
    } catch {
        Write-Output "$label : refused ($($_.Exception.Message.Split([char]13)[0]))"
    } finally {
        $stream.Close()
    }
}

if ($HoldOffset -ge 0) {
    $stream = [System.IO.File]::Open($path, 'Open', 'ReadWrite', 'ReadWrite')
    $stream.Lock($HoldOffset, $HoldLength)
    Write-Output "holding [$HoldOffset, $($HoldOffset + $HoldLength)) for $HoldSeconds seconds"
    Start-Sleep -Seconds $HoldSeconds
    $stream.Unlock($HoldOffset, $HoldLength)
    $stream.Close()
    Write-Output 'released'
    cmd /c "net use $share /delete /y" 2>&1 | Out-Null
    exit 0
}

Write-Output '=== locking free bytes'
Try-Lock 'lock [0, 100)          ' 0 100

Write-Output '=== a second handle against a held lock'
$held = [System.IO.File]::Open($path, 'Open', 'ReadWrite', 'ReadWrite')
$held.Lock(0, 100)
Try-Lock 'overlapping lock       ' 50 100
Try-Lock 'adjacent lock          ' 100 100
$held.Unlock(0, 100)
$held.Close()

Write-Output '=== after the holder released'
Try-Lock 'overlapping lock again ' 50 100

cmd /c "net use $share /delete /y" 2>&1 | Out-Null
Write-Output 'done'
