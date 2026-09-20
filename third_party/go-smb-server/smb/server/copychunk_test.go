package server

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func buildIoctl(sessID uint64, treeID uint32, fid [16]byte, ctlCode uint32, input []byte, msgID uint64) []byte {
	hdr := wire.NewHeader(wire.CmdIoctl)
	hdr.SessionId = sessID
	hdr.TreeId = treeID
	hdr.MessageId = msgID
	hdr.Credit = 1
	body := make([]byte, 56)
	binary.LittleEndian.PutUint16(body[0:2], 57)
	binary.LittleEndian.PutUint32(body[4:8], ctlCode)
	copy(body[8:24], fid[:])
	binary.LittleEndian.PutUint32(body[24:28], uint32(wire.HeaderSize+56)) // InputOffset
	binary.LittleEndian.PutUint32(body[28:32], uint32(len(input)))         // InputCount
	binary.LittleEndian.PutUint32(body[40:44], 64*1024)                    // MaxOutputResponse
	binary.LittleEndian.PutUint32(body[48:52], 1)                          // IsFsctl
	return append(append(hdr.Append(nil), body...), input...)
}

// copyChunkInput builds SRV_COPYCHUNK_COPY.
func copyChunkInput(key [24]byte, chunks []wire.CopyChunk) []byte {
	out := make([]byte, 32, 32+24*len(chunks))
	copy(out, key[:])
	binary.LittleEndian.PutUint32(out[24:28], uint32(len(chunks)))
	for _, c := range chunks {
		var buf [24]byte
		binary.LittleEndian.PutUint64(buf[0:8], c.SourceOffset)
		binary.LittleEndian.PutUint64(buf[8:16], c.TargetOffset)
		binary.LittleEndian.PutUint32(buf[16:20], c.Length)
		out = append(out, buf[:]...)
	}
	return out
}

// copySession opens a source file with contents and an empty destination,
// returning their handles and the source's resume key.
func copySession(t *testing.T, fc *transport.FramedConn, sessID uint64, treeID uint32, contents string) (srcKey [24]byte, dstFid [16]byte) {
	t.Helper()
	mustWrite(t, fc, buildCreate(sessID, treeID, "source.txt", wire.FileOpenIf))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("create source: %#x", rh.Status)
	}
	var srcFid [16]byte
	copy(srcFid[:], resp[64+64:64+80])
	mustWrite(t, fc, buildWrite(sessID, treeID, srcFid, 0, []byte(contents)))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("write source: %#x", rh.Status)
	}

	mustWrite(t, fc, buildCreate(sessID, treeID, "dest.txt", wire.FileOpenIf))
	rh, resp = readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("create destination: %#x", rh.Status)
	}
	copy(dstFid[:], resp[64+64:64+80])

	mustWrite(t, fc, buildIoctl(sessID, treeID, srcFid, wire.FSCTLSrvRequestResumeKey, nil, 40))
	rh, resp = readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("resume key: status %#x, want success", rh.Status)
	}
	outOffset := binary.LittleEndian.Uint32(resp[64+32 : 64+36])
	outCount := binary.LittleEndian.Uint32(resp[64+36 : 64+40])
	if outCount < 24 {
		t.Fatalf("resume key response is %d bytes, want at least 24", outCount)
	}
	copy(srcKey[:], resp[outOffset:outOffset+24])
	return srcKey, dstFid
}

// A client copying inside the share asks the server to move the bytes, so
// they never travel out to the client and back.
func TestCopyChunkCopiesServerSide(t *testing.T) {
	backend := newMemBackend()
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newTestServer(backend), srvConn)()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)

	const contents = "the bytes that are copied inside the server"
	srcKey, dstFid := copySession(t, fc, sessID, treeID, contents)

	input := copyChunkInput(srcKey, []wire.CopyChunk{{Length: uint32(len(contents))}})
	mustWrite(t, fc, buildIoctl(sessID, treeID, dstFid, wire.FSCTLSrvCopychunk, input, 41))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("copychunk: status %#x, want success", rh.Status)
	}
	outOffset := binary.LittleEndian.Uint32(resp[64+32 : 64+36])
	out := resp[outOffset:]
	if chunks := binary.LittleEndian.Uint32(out[0:4]); chunks != 1 {
		t.Errorf("ChunksWritten = %d, want 1", chunks)
	}
	if total := binary.LittleEndian.Uint32(out[8:12]); total != uint32(len(contents)) {
		t.Errorf("TotalBytesWritten = %d, want %d", total, len(contents))
	}

	// The destination really holds the bytes.
	mustWrite(t, fc, buildRead(sessID, treeID, dstFid, 0, uint32(len(contents))))
	rh, resp = readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("read destination: %#x", rh.Status)
	}
	dataOffset := resp[64+2]
	length := binary.LittleEndian.Uint32(resp[64+4 : 64+8])
	if got := string(resp[dataOffset : uint32(dataOffset)+length]); got != contents {
		t.Fatalf("destination = %q, want %q", got, contents)
	}
}

// A request past the server's limits is told what they are, so the client
// can split the copy rather than give up.
func TestCopyChunkReportsLimits(t *testing.T) {
	backend := newMemBackend()
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newTestServer(backend), srvConn)()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	srcKey, dstFid := copySession(t, fc, sessID, treeID, "small")

	oversized := []wire.CopyChunk{{Length: maxCopyChunkSize + 1}}
	mustWrite(t, fc, buildIoctl(sessID, treeID, dstFid, wire.FSCTLSrvCopychunk, copyChunkInput(srcKey, oversized), 42))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusInvalidParameter {
		t.Fatalf("oversized chunk: status %#x, want INVALID_PARAMETER", rh.Status)
	}
	outOffset := binary.LittleEndian.Uint32(resp[64+32 : 64+36])
	out := resp[outOffset:]
	if chunks := binary.LittleEndian.Uint32(out[0:4]); chunks != maxCopyChunks {
		t.Errorf("limit response ChunksWritten = %d, want the chunk limit %d", chunks, maxCopyChunks)
	}
	if size := binary.LittleEndian.Uint32(out[4:8]); size != maxCopyChunkSize {
		t.Errorf("limit response ChunkBytesWritten = %d, want the chunk size limit %d", size, maxCopyChunkSize)
	}
}

// A key the server never issued, or one whose file has closed, is refused:
// the client falls back to copying through itself.
func TestCopyChunkRejectsUnknownKey(t *testing.T) {
	backend := newMemBackend()
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newTestServer(backend), srvConn)()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	_, dstFid := copySession(t, fc, sessID, treeID, "contents")

	var bogus [24]byte
	for i := range bogus {
		bogus[i] = 0xAB
	}
	mustWrite(t, fc, buildIoctl(sessID, treeID, dstFid, wire.FSCTLSrvCopychunk, copyChunkInput(bogus, []wire.CopyChunk{{Length: 4}}), 43))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusObjectNameNotFound {
		t.Fatalf("unknown resume key: status %#x, want OBJECT_NAME_NOT_FOUND", rh.Status)
	}
}

// countingCopier records that the backend was asked to copy the range
// itself, and can decline.
type countingCopier struct {
	vfs.Handle
	calls    int
	declined bool
}

func (c *countingCopier) CopyChunk(ctx context.Context, src vfs.Handle, srcOffset, dstOffset, length int64) (int64, error) {
	c.calls++
	if c.declined {
		return 0, errors.ErrUnsupported
	}
	return length, nil
}

// A backend that can copy the range itself is asked to, and one that
// declines a particular copy gets the bytes moved for it.
func TestCopyRangeUsesBackendWhenItCan(t *testing.T) {
	backend := newMemBackend()
	ctx := context.Background()
	src, err := backend.Open(ctx, vfs.OpenOptions{Path: "src.txt", Disposition: vfs.DispositionCreate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Write(ctx, 0, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	dst, err := backend.Open(ctx, vfs.OpenOptions{Path: "dst.txt", Disposition: vfs.DispositionCreate})
	if err != nil {
		t.Fatal(err)
	}

	copier := &countingCopier{Handle: dst}
	if n, err := copyRange(ctx, src, copier, 0, 0, 10); err != nil || n != 10 {
		t.Fatalf("copyRange with a capable backend = %d, %v; want 10", n, err)
	}
	if copier.calls != 1 {
		t.Fatalf("backend copy calls = %d, want 1", copier.calls)
	}

	// Declining falls back to moving the bytes, and the destination really
	// receives them.
	copier.declined = true
	if n, err := copyRange(ctx, src, copier, 0, 0, 10); err != nil || n != 10 {
		t.Fatalf("copyRange with a declining backend = %d, %v; want 10", n, err)
	}
	buf := make([]byte, 10)
	if _, err := dst.Read(ctx, 0, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "0123456789" {
		t.Fatalf("destination = %q, want the source's bytes", buf)
	}
}
