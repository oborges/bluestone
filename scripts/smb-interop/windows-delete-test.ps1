# Checks delete-on-close and delete-pending semantics over SMB. Run it against
# Windows' own server for reference and against the gateway, then compare.
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
using System.Text;
using Microsoft.Win32.SafeHandles;
public static class F {
    [DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    static extern SafeFileHandle CreateFileW(string name, uint access, uint share, IntPtr sa, uint disp, uint flags, IntPtr tmpl);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern bool SetFileInformationByHandle(SafeFileHandle h, int cls, byte[] buf, uint size);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern bool GetFileInformationByHandleEx(SafeFileHandle h, int cls, byte[] buf, uint size);

    public const uint DELETE = 0x10000, READ = 0x80000000, WRITE = 0x40000000, READ_ATTR = 0x80;
    public const uint SHARE_ALL = 7;
    public const uint OPEN_EXISTING = 3, CREATE_ALWAYS = 2, OPEN_ALWAYS = 4;
    public const uint DOC = 0x04000000, BACKUP = 0x02000000;

    // Open returns the handle, or null with the Win32 error in Err.
    public static int Err;
    public static SafeFileHandle Open(string name, uint access, uint disp, uint flags) {
        var h = CreateFileW(name, access, SHARE_ALL, IntPtr.Zero, disp, flags, IntPtr.Zero);
        Err = h.IsInvalid ? Marshal.GetLastWin32Error() : 0;
        return h.IsInvalid ? null : h;
    }
    public static int SetDelete(SafeFileHandle h, bool del) {
        return SetFileInformationByHandle(h, 4, new byte[] { (byte)(del ? 1 : 0) }, 1) ? 0 : Marshal.GetLastWin32Error();
    }
    public static int Rename(SafeFileHandle h, string newName, bool replace) {
        byte[] name = Encoding.Unicode.GetBytes(@"\??\UNC\" + newName.TrimStart('\\'));
        byte[] buf = new byte[20 + name.Length + 2];
        buf[0] = (byte)(replace ? 1 : 0);
        BitConverter.GetBytes(name.Length).CopyTo(buf, 16);
        name.CopyTo(buf, 20);
        return SetFileInformationByHandle(h, 3, buf, (uint)buf.Length) ? 0 : Marshal.GetLastWin32Error();
    }
    public static string DeletePending(SafeFileHandle h) {
        byte[] buf = new byte[24];
        if (!GetFileInformationByHandleEx(h, 1, buf, 24)) return "error " + Marshal.GetLastWin32Error();
        return buf[20] != 0 ? "pending" : "not pending";
    }
}
'@

$root = Join-Path $Unc 'deltest'
if (Test-Path $root) { Remove-Item $root -Recurse -Force }
New-Item -ItemType Directory -Path $root | Out-Null
function P($n) { Join-Path $root $n }
# Listed rather than opened: a file whose delete is pending cannot be opened
# but is still in its directory.
function Exists($n) {
    $parent = Split-Path (P $n) -Parent
    $leaf = Split-Path (P $n) -Leaf
    if (Get-ChildItem -LiteralPath $parent -Force -ErrorAction SilentlyContinue | Where-Object { $_.Name -eq $leaf }) { 'listed' } else { 'gone' }
}
function Put($n, $text) { [System.IO.File]::WriteAllText((P $n), $text) }
function Rep($name, $value) { Write-Output ("{0,-48} {1}" -f $name, $value) }

# 1. Delete-on-close from create.
$h = [F]::Open((P 'doc.txt'), [F]::READ -bor [F]::WRITE -bor [F]::DELETE, [F]::CREATE_ALWAYS, [F]::DOC)
Rep '1 doc open' $(if ($h) { 'ok' } else { "error $([F]::Err)" })
Rep '1 doc visible while open' (Exists 'doc.txt')
Rep '1 doc handle reports' ([F]::DeletePending($h))
$h.Close()
Rep '1 doc after close' (Exists 'doc.txt')

# 2. Pending delete waits for the last handle.
Put 'two.txt' 'two handles'
$a = [F]::Open((P 'two.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, 0)
$b = [F]::Open((P 'two.txt'), [F]::READ, [F]::OPEN_EXISTING, 0)
Rep '2 set disposition' ([F]::SetDelete($a, $true))
Rep '2 other handle reports' ([F]::DeletePending($b))
$a.Close()
Rep '2 after first close' (Exists 'two.txt')
Rep '2 other handle still reports' ([F]::DeletePending($b))
$c = [F]::Open((P 'two.txt'), [F]::READ, [F]::OPEN_EXISTING, 0)
Rep '2 new open while pending' $(if ($c) { $c.Close(); 'ok' } else { "error $([F]::Err)" })
$c = [F]::Open((P 'two.txt'), [F]::READ -bor [F]::WRITE, [F]::CREATE_ALWAYS, 0)
Rep '2 overwrite while pending' $(if ($c) { $c.Close(); 'ok' } else { "error $([F]::Err)" })
$b.Close()
Rep '2 after last close' (Exists 'two.txt')

# 3. Clearing the disposition keeps the file.
Put 'undo.txt' 'keep me'
$a = [F]::Open((P 'undo.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, 0)
[F]::SetDelete($a, $true) | Out-Null
Rep '3 clear disposition' ([F]::SetDelete($a, $false))
$a.Close()
Rep '3 after close' (Exists 'undo.txt')

# 4. A directory with something in it.
New-Item -ItemType Directory -Path (P 'full') | Out-Null
Put 'full\child.txt' 'x'
$d = [F]::Open((P 'full'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, [F]::BACKUP)
Rep '4 set disposition on non-empty dir' ([F]::SetDelete($d, $true))
$d.Close()
Rep '4 dir after close' (Exists 'full')
$d = [F]::Open((P 'full'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, [F]::BACKUP -bor [F]::DOC)
Rep '4 doc open of non-empty dir' $(if ($d) { $d.Close(); 'ok' } else { "error $([F]::Err)" })
Rep '4 dir after doc close' (Exists 'full')
Rep '4 child after doc close' (Exists 'full\child.txt')

# 5. An empty directory is deleted.
New-Item -ItemType Directory -Path (P 'empty') | Out-Null
$d = [F]::Open((P 'empty'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, [F]::BACKUP)
Rep '5 set disposition on empty dir' ([F]::SetDelete($d, $true))
$d.Close()
Rep '5 empty dir after close' (Exists 'empty')

# 6. Read-only files.
Put 'ro.txt' 'read only'
Set-ItemProperty -LiteralPath (P 'ro.txt') -Name IsReadOnly -Value $true
Rep '6 read-only attribute stuck' ((Get-Item -LiteralPath (P 'ro.txt')).IsReadOnly)
$a = [F]::Open((P 'ro.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, 0)
Rep '6 set disposition on read-only' ([F]::SetDelete($a, $true))
$a.Close()
$a = [F]::Open((P 'ro.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, [F]::DOC)
Rep '6 doc open of read-only' $(if ($a) { $a.Close(); 'ok' } else { "error $([F]::Err)" })
Rep '6 read-only after close' (Exists 'ro.txt')
if (Test-Path -LiteralPath (P 'ro.txt')) { Set-ItemProperty -LiteralPath (P 'ro.txt') -Name IsReadOnly -Value $false }

# 7. Delete-on-close needs delete access.
Put 'noaccess.txt' 'x'
$a = [F]::Open((P 'noaccess.txt'), [F]::READ, [F]::OPEN_EXISTING, [F]::DOC)
Rep '7 doc open without delete access' $(if ($a) { $a.Close(); 'ok' } else { "error $([F]::Err)" })
Rep '7 file after' (Exists 'noaccess.txt')
Put 'noaccess.txt' 'x'
$a = [F]::Open((P 'noaccess.txt'), [F]::READ, [F]::OPEN_EXISTING, 0)
Rep '7 set disposition without delete access' ([F]::SetDelete($a, $true))
$a.Close()
Rep '7 file after close' (Exists 'noaccess.txt')

# 8. Delete after rename removes the renamed file, not what now has the old name.
Put 'saved.txt' 'old version'
$a = [F]::Open((P 'saved.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, 0)
Rep '8 rename open file' ([F]::Rename($a, (P 'backup.txt'), $false))
Put 'saved.txt' 'new version'
Rep '8 set disposition on renamed handle' ([F]::SetDelete($a, $true))
$a.Close()
Rep '8 new saved.txt' $(if (Test-Path (P 'saved.txt')) { Get-Content (P 'saved.txt') } else { 'gone' })
Rep '8 backup.txt' (Exists 'backup.txt')

# 9. Renaming a directory with a file open inside it.
New-Item -ItemType Directory -Path (P 'busy') | Out-Null
Put 'busy\open.txt' 'x'
$f = [F]::Open((P 'busy\open.txt'), [F]::READ, [F]::OPEN_EXISTING, 0)
$d = [F]::Open((P 'busy'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, [F]::BACKUP)
Rep '9 rename dir with open child' ([F]::Rename($d, (P 'moved'), $false))
$d.Close(); $f.Close()
Rep '9 busy' (Exists 'busy')
Rep '9 moved' (Exists 'moved')

# 10. Replacing a file that is open.
Put 'target.txt' 'target'
Put 'incoming.txt' 'incoming'
$t = [F]::Open((P 'target.txt'), [F]::READ, [F]::OPEN_EXISTING, 0)
$a = [F]::Open((P 'incoming.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, 0)
Rep '10 replace an open file' ([F]::Rename($a, (P 'target.txt'), $true))
$a.Close(); $t.Close()
Rep '10 target' $(if (Test-Path (P 'target.txt')) { Get-Content (P 'target.txt') } else { 'gone' })

# 11. Rename a file whose delete is pending.
Put 'pend.txt' 'x'
$a = [F]::Open((P 'pend.txt'), [F]::READ -bor [F]::DELETE, [F]::OPEN_EXISTING, 0)
[F]::SetDelete($a, $true) | Out-Null
Rep '11 rename pending file' ([F]::Rename($a, (P 'pend2.txt'), $false))
$a.Close()
Rep '11 pend' (Exists 'pend.txt')
Rep '11 pend2' (Exists 'pend2.txt')

Remove-Item $root -Recurse -Force -ErrorAction SilentlyContinue
if ($User) { cmd /c "net use $Unc /delete /y" 2>&1 | Out-Null }
Write-Output done
