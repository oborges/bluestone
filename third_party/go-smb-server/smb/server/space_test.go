package server

import (
	"context"
	"encoding/binary"
	"syscall"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// spaceBackend is a share that reports how much room it has.
type spaceBackend struct {
	*memBackend
	space vfs.Space
	err   error
}

func (b *spaceBackend) Space(context.Context) (vfs.Space, error) { return b.space, b.err }

func buildQueryFilesystem(sessID uint64, treeID uint32, fid [16]byte, class uint8) []byte {
	msg := buildQueryInfo(sessID, treeID, fid, class)
	msg[wire.HeaderSize+2] = wire.InfoFilesystem
	return msg
}

// queryFullSize opens the share root and asks for FILE_FS_FULL_SIZE_INFORMATION,
// returning total, caller-available and actual-available allocation units.
func queryFullSize(t *testing.T, backend vfs.Backend) (status uint32, total, caller, actual uint64) {
	t.Helper()
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newTestServer(backend), srvConn)()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	mustWrite(t, fc, buildCreate(sessID, treeID, "", wire.FileOpen))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("open share root: %#x", rh.Status)
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])

	mustWrite(t, fc, buildQueryFilesystem(sessID, treeID, fid, wire.FileFsFullSizeInformation))
	rh, resp = readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		return rh.Status, 0, 0, 0
	}
	info := resp[64+8:]
	if unit := binary.LittleEndian.Uint32(info[24:28]) * binary.LittleEndian.Uint32(info[28:32]); unit != 4096 {
		t.Fatalf("allocation unit = %d bytes, want 4096", unit)
	}
	return rh.Status,
		binary.LittleEndian.Uint64(info[0:8]),
		binary.LittleEndian.Uint64(info[8:16]),
		binary.LittleEndian.Uint64(info[16:24])
}

// Windows checks free space before a copy and refuses one that will not
// fit, so a share that is filling up has to say so rather than report a
// fixed number and fail partway through with STATUS_DISK_FULL.
func TestFilesystemSizeReportsBackendSpace(t *testing.T) {
	backend := &spaceBackend{memBackend: newMemBackend(), space: vfs.Space{
		TotalBytes:     100 << 30,
		AvailableBytes: 3 << 30,
	}}
	status, total, caller, actual := queryFullSize(t, backend)
	if status != wire.StatusSuccess {
		t.Fatalf("query full size: %#x", status)
	}
	if total != (100<<30)/4096 {
		t.Errorf("total units = %d, want %d", total, (100<<30)/4096)
	}
	if caller != (3<<30)/4096 || actual != caller {
		t.Errorf("available units = %d caller, %d actual; want %d for both", caller, actual, (3<<30)/4096)
	}
}

// A full share reports no free space at all.
func TestFilesystemSizeReportsFullShare(t *testing.T) {
	backend := &spaceBackend{memBackend: newMemBackend(), space: vfs.Space{TotalBytes: 1 << 30}}
	_, total, caller, _ := queryFullSize(t, backend)
	if total == 0 || caller != 0 {
		t.Fatalf("full share = %d total, %d free units; want a size and nothing free", total, caller)
	}
}

// Free space is never more than the share's size, which Windows would show
// as a negative amount used.
func TestFilesystemSizeKeepsFreeWithinTotal(t *testing.T) {
	backend := &spaceBackend{memBackend: newMemBackend(), space: vfs.Space{TotalBytes: 1 << 30, AvailableBytes: 2 << 30}}
	_, total, caller, _ := queryFullSize(t, backend)
	if caller > total {
		t.Fatalf("free units %d exceed total %d", caller, total)
	}
}

// A backend that cannot tell how much room it has reports why, rather than
// the share claiming space it may not have.
func TestFilesystemSizeReportsBackendError(t *testing.T) {
	backend := &spaceBackend{memBackend: newMemBackend(), err: syscall.EIO}
	if status, _, _, _ := queryFullSize(t, backend); status == wire.StatusSuccess {
		t.Fatal("query full size succeeded despite the backend failing")
	}
}

// A backend that does not report space still answers, with a large share.
func TestFilesystemSizeWithoutReporter(t *testing.T) {
	status, total, caller, _ := queryFullSize(t, newMemBackend())
	if status != wire.StatusSuccess || total == 0 || caller == 0 {
		t.Fatalf("nominal size = %#x, %d total, %d free units; want success with room", status, total, caller)
	}
}
