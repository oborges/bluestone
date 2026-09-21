# Checks SMB leases from Windows. Phases run separately so the gateway's
# counters can be read around each:
#   stale  - read a file through an open handle, wait while the file is
#            changed elsewhere (over NFS), read again through the same handle
#   cache  - read one file 50 times, to count how many reads reach the server
#   handle - read a file and close it, leaving any cached handle behind
param([Parameter(Mandatory=$true)][string]$Unc, [Parameter(Mandatory=$true)][string]$Phase,
      [string]$User, [string]$Domain='BLUESTONE', [int]$Wait=12)
if ($User) {
    $password = [Console]::In.ReadLine()
    cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null
    $out = cmd /c "net use $Unc /user:$Domain\$User $password" 2>&1
    if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }
}
$dir = Join-Path $Unc 'leasetest'
if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
$file = Join-Path $dir "$Phase.txt"

function ReadAll($fs) {
    $fs.Seek(0, 'Begin') | Out-Null
    $buf = New-Object byte[] 4096
    $n = $fs.Read($buf, 0, $buf.Length)
    [System.Text.Encoding]::ASCII.GetString($buf, 0, $n)
}

switch ($Phase) {
    'stale' {
        [System.IO.File]::WriteAllText($file, 'version one')
        $fs = New-Object System.IO.FileStream($file, 'Open', 'Read', 'ReadWrite, Delete')
        Write-Output ("first read : {0}" -f (ReadAll $fs))
        Write-Output ("again      : {0}" -f (ReadAll $fs))
        Start-Sleep -Seconds $Wait
        Write-Output ("after wait : {0}" -f (ReadAll $fs))
        Write-Output ("length     : {0}" -f $fs.Length)
        $fs.Close()
        Write-Output ("reopened   : {0}" -f [System.IO.File]::ReadAllText($file))
    }
    'cache' {
        [System.IO.File]::WriteAllText($file, ('x' * 20000))
        Start-Sleep -Seconds 1
        Write-Output "MARK start"
        Start-Sleep -Seconds 3
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        for ($i = 0; $i -lt 50; $i++) { $null = [System.IO.File]::ReadAllText($file) }
        $sw.Stop()
        Write-Output ("50 reads took {0} ms" -f $sw.ElapsedMilliseconds)
        Start-Sleep -Seconds 3
    }
    'handle' {
        [System.IO.File]::WriteAllText($file, 'windows read this')
        $null = [System.IO.File]::ReadAllText($file)
        Write-Output "read and closed"
    }
}
Write-Output done
