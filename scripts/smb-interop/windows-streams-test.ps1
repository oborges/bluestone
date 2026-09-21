# Checks alternate data streams over SMB. Run it against Windows' own server
# for reference and against the gateway, then compare.
param([Parameter(Mandatory=$true)][string]$Unc, [string]$User, [string]$Domain='BLUESTONE')
if ($User) {
    $password = [Console]::In.ReadLine()
    cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null
    $out = cmd /c "net use $Unc /user:$Domain\$User $password" 2>&1
    if ($LASTEXITCODE -ne 0) { Write-Output "CONNECT FAILED: $out"; exit 1 }
}

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
public static class S {
    [DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    static extern SafeFileHandle CreateFileW(string name, uint access, uint share, IntPtr sa, uint disp, uint flags, IntPtr tmpl);
    [DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    static extern bool GetVolumeInformationW(string root, System.Text.StringBuilder name, int nameLen, out uint serial, out uint maxComponent, out uint flags, System.Text.StringBuilder fs, int fsLen);
    public static string NamedStreams(string root) {
        uint serial, max, flags;
        if (!GetVolumeInformationW(root, new System.Text.StringBuilder(261), 261, out serial, out max, out flags, new System.Text.StringBuilder(261), 261)) return "error " + Marshal.GetLastWin32Error();
        return (flags & 0x40000) != 0 ? "yes" : "no";
    }
    public const uint READ = 0x80000000, READWRITE = 0xC0000000;
    public static int Err;
    public static SafeFileHandle Open(string name, uint access, uint share, uint disp, uint flags) {
        var h = CreateFileW(name, access, share, IntPtr.Zero, disp, flags, IntPtr.Zero);
        Err = h.IsInvalid ? Marshal.GetLastWin32Error() : 0;
        return h.IsInvalid ? null : h;
    }
}
'@

$root = Join-Path $Unc 'streamtest'
if (Test-Path $root) { Remove-Item $root -Recurse -Force }
New-Item -ItemType Directory -Path $root | Out-Null
function P($n) { Join-Path $root $n }
function Rep($name, $value) { Write-Output ("{0,-44} {1}" -f $name, $value) }
function Streams($n) {
    $s = Get-Item -LiteralPath (P $n) -Stream * -ErrorAction SilentlyContinue | ForEach-Object { "$($_.Stream)=$($_.Length)" }
    if ($s) { $s -join ' ' } else { '(none)' }
}
function Attempt($block) { try { & $block } catch { "error: " + $_.Exception.Message.Split([Environment]::NewLine)[0] } }

Write-Output "=== filesystem flags"
Rep 'named streams advertised' ([S]::NamedStreams("$Unc\"))

Write-Output "=== write, read and list"
Set-Content -LiteralPath (P 'a.txt') -Value 'main content'
Rep '1 write stream' (Attempt { Set-Content -LiteralPath (P 'a.txt') -Stream 'note' -Value 'stream content'; 'ok' })
Rep '1 read stream' (Attempt { Get-Content -LiteralPath (P 'a.txt') -Stream 'note' })
Rep '1 read stream other case' (Attempt { Get-Content -LiteralPath (P 'a.txt') -Stream 'NOTE' })
Rep '1 main content unchanged' (Attempt { Get-Content -LiteralPath (P 'a.txt') })
Rep '1 main size' ((Get-Item -LiteralPath (P 'a.txt')).Length)
Rep '1 streams' (Streams 'a.txt')
Rep '1 read ::$DATA' (Attempt { Get-Content -LiteralPath (P 'a.txt') -Stream ':$DATA' })
Rep '1 missing stream' (Attempt { Get-Content -LiteralPath (P 'a.txt') -Stream 'nothere' -ErrorAction Stop })

Write-Output "=== what survives"
Set-Content -LiteralPath (P 'a.txt') -Value 'rewritten main'
Rep '2 after Set-Content on main' (Streams 'a.txt')
[System.IO.File]::WriteAllText((P 'a.txt'), 'overwritten')
Rep '2 after CREATE_ALWAYS on main' (Streams 'a.txt')
Set-Content -LiteralPath (P 'a.txt') -Stream 'note' -Value 'stream content'
Rename-Item -LiteralPath (P 'a.txt') -NewName 'b.txt'
Rep '2 after rename' (Streams 'b.txt')
Copy-Item -LiteralPath (P 'b.txt') -Destination (P 'c.txt')
Rep '2 copy has' (Streams 'c.txt')
Remove-Item -LiteralPath (P 'b.txt')
Set-Content -LiteralPath (P 'b.txt') -Value 'recreated'
Rep '2 recreated after delete' (Streams 'b.txt')

Write-Output "=== removing"
Set-Content -LiteralPath (P 'c.txt') -Stream 'other' -Value 'x'
Rep '3 remove one stream' (Attempt { Remove-Item -LiteralPath (P 'c.txt') -Stream 'note'; 'ok' })
Rep '3 left' (Streams 'c.txt')
Rep '3 file still there' (Test-Path -LiteralPath (P 'c.txt'))

Write-Output "=== download mark"
Set-Content -LiteralPath (P 'dl.exe') -Value 'pretend binary'
Set-Content -LiteralPath (P 'dl.exe') -Stream 'Zone.Identifier' -Value "[ZoneTransfer]`r`nZoneId=3`r`nHostUrl=https://example.com/downloads/tool.exe"
Rep '4 marked' (Streams 'dl.exe')
Rep '4 unblock' (Attempt { Unblock-File -LiteralPath (P 'dl.exe'); 'ok' })
Rep '4 after unblock' (Streams 'dl.exe')

Write-Output "=== creating and directories"
Rep '5 stream on new file' (Attempt { Set-Content -LiteralPath (P 'fresh.txt') -Stream 's' -Value 'only a stream'; 'ok' })
Rep '5 new file exists' (Test-Path -LiteralPath (P 'fresh.txt'))
Rep '5 new file main size' (Attempt { (Get-Item -LiteralPath (P 'fresh.txt')).Length })
New-Item -ItemType Directory -Path (P 'dir') | Out-Null
Rep '5 stream on directory' (Attempt { Set-Content -LiteralPath (P 'dir') -Stream 'dirnote' -Value 'on a dir'; 'ok' })
Rep '5 directory streams' (Streams 'dir')
Rep '5 directory listing' ((Get-ChildItem -LiteralPath $root -Force | ForEach-Object Name | Sort-Object) -join ',')

Write-Output "=== share modes are per stream"
Set-Content -LiteralPath (P 'held.txt') -Value 'held'
Set-Content -LiteralPath (P 'held.txt') -Stream 's' -Value 'side'
$h = [S]::Open((P 'held.txt'), [S]::READWRITE, 0, 3, 0)
$s = [S]::Open((P 'held.txt') + ':s', [S]::READ, 7, 3, 0)
Rep '6 open stream while main held exclusively' $(if ($s) { $s.Close(); 'ok' } else { "error $([S]::Err)" })
$m = [S]::Open((P 'held.txt'), [S]::READ, 7, 3, 0)
Rep '6 open main while main held exclusively' $(if ($m) { $m.Close(); 'ok' } else { "error $([S]::Err)" })
$h.Close()

Write-Output "=== size"
$big = 'x' * 65536
Rep '7 64 KiB stream' (Attempt { Set-Content -LiteralPath (P 'big.txt') -Stream 'big' -Value $big -NoNewline; 'ok' })
Rep '7 streams' (Streams 'big.txt')

Remove-Item $root -Recurse -Force -ErrorAction SilentlyContinue
if ($User) { cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null }
Write-Output done
