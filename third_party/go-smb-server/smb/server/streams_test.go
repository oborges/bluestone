package server

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func TestSplitStream(t *testing.T) {
	for _, tc := range []struct {
		name, base, stream string
		invalid            bool
	}{
		{name: `dir\file.txt`, base: `dir\file.txt`},
		{name: `file.txt:Zone.Identifier`, base: "file.txt", stream: "Zone.Identifier"},
		{name: `dir\file.txt:AFP_AfpInfo:$DATA`, base: `dir\file.txt`, stream: "AFP_AfpInfo"},
		{name: `file.txt:note:$data`, base: "file.txt", stream: "note"},
		{name: `file.txt::$DATA`, base: "file.txt"},
		{name: `file.txt:note:$INDEX_ALLOCATION`, invalid: true},
		{name: `file.txt:`, invalid: true},
		{name: `dir:x\file.txt`, invalid: true},
		{name: `file.txt:a:b:c`, invalid: true},
	} {
		base, stream, err := splitStream(tc.name)
		if tc.invalid {
			if err == nil {
				t.Errorf("splitStream(%q) = %q, %q; want invalid", tc.name, base, stream)
			}
			continue
		}
		if err != nil || base != tc.base || stream != tc.stream {
			t.Errorf("splitStream(%q) = %q, %q, %v; want %q, %q", tc.name, base, stream, err, tc.base, tc.stream)
		}
	}
}

func (c *deleteClient) read(fid [16]byte, length uint32) (string, uint32) {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildRead(c.sessID, c.treeID, fid, 0, length))
	rh, resp := readReply(c.t, c.fc)
	if rh.Status != wire.StatusSuccess {
		return "", rh.Status
	}
	off := resp[64+2]
	n := binary.LittleEndian.Uint32(resp[64+4 : 64+8])
	return string(resp[off : uint32(off)+n]), rh.Status
}

// queryStreams reads FILE_STREAM_INFORMATION through fid as name=size.
func (c *deleteClient) queryStreams(fid [16]byte) map[string]uint64 {
	c.t.Helper()
	mustWrite(c.t, c.fc, buildQueryInfo(c.sessID, c.treeID, fid, wire.FileStreamInformation))
	rh, resp := readReply(c.t, c.fc)
	if rh.Status != wire.StatusSuccess {
		c.t.Fatalf("query streams: %#x", rh.Status)
	}
	length := binary.LittleEndian.Uint32(resp[64+4 : 64+8])
	buf := resp[64+8 : 64+8+int(length)]
	out := map[string]uint64{}
	for len(buf) >= 24 {
		next := binary.LittleEndian.Uint32(buf[0:4])
		nameLen := binary.LittleEndian.Uint32(buf[4:8])
		out[wire.UTF16FromBytes(buf[24:24+nameLen])] = binary.LittleEndian.Uint64(buf[8:16])
		if next == 0 {
			break
		}
		buf = buf[next:]
	}
	return out
}

// A named stream is written and read on its own, beside the file's data,
// and listed with it.
func TestNamedStreamReadWriteAndList(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("a.txt", "main content")

	s := c.mustOpen("a.txt:note:$DATA", wire.FileOverwriteIf, genericAll, 0)
	c.write(s, "stream content")
	c.close(s)

	s = c.mustOpen("a.txt:NOTE", wire.FileOpen, accessRead, 0)
	if got, _ := c.read(s, 100); got != "stream content" {
		t.Errorf("stream read back = %q", got)
	}
	c.close(s)

	f := c.mustOpen("a.txt", wire.FileOpen, accessRead, 0)
	if got, _ := c.read(f, 100); got != "main content" {
		t.Errorf("file data = %q after writing a stream", got)
	}
	streams := c.queryStreams(f)
	if streams["::$DATA"] != uint64(len("main content")) || streams[":note:$DATA"] != uint64(len("stream content")) || len(streams) != 2 {
		t.Errorf("streams = %v, want ::$DATA and :note:$DATA with their sizes", streams)
	}
	c.close(f)

	m := c.mustOpen("a.txt::$DATA", wire.FileOpen, accessRead, 0)
	if got, _ := c.read(m, 100); got != "main content" {
		t.Errorf("::$DATA read = %q, want the file's own data", got)
	}
	c.close(m)

	if _, status := c.open("a.txt:missing", wire.FileOpen, accessRead, 0); status != wire.StatusObjectNameNotFound {
		t.Errorf("open missing stream: %#x, want OBJECT_NAME_NOT_FOUND", status)
	}
	if _, status := c.open("a.txt:x:$BOGUS", wire.FileOpen, accessRead, 0); status != wire.StatusObjectNameInvalid {
		t.Errorf("open with a bad stream type: %#x, want OBJECT_NAME_INVALID", status)
	}
}

// Deleting a stream leaves the file and its other streams.
func TestNamedStreamDelete(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("c.txt", "file")
	for _, name := range []string{"c.txt:one", "c.txt:two"} {
		s := c.mustOpen(name, wire.FileOverwriteIf, genericAll, 0)
		c.write(s, "x")
		c.close(s)
	}

	s := c.mustOpen("c.txt:one", wire.FileOpen, genericAll, 0)
	if status := c.setDelete(s, true); status != wire.StatusSuccess {
		t.Fatalf("set disposition on stream: %#x", status)
	}
	c.close(s)

	if backend.contents("c.txt") != "file" {
		t.Fatal("deleting a stream deleted the file")
	}
	f := c.mustOpen("c.txt", wire.FileOpen, accessRead, 0)
	if streams := c.queryStreams(f); len(streams) != 2 || streams[":two:$DATA"] != 1 {
		t.Errorf("streams after deleting one = %v, want ::$DATA and :two:$DATA", streams)
	}
	c.close(f)
}

// A file whose streams are open cannot be renamed from under them.
func TestRenameRefusedWithOpenStream(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("r.txt", "x")
	s := c.mustOpen("r.txt:s", wire.FileOverwriteIf, genericAll, 0)
	f := c.mustOpen("r.txt", wire.FileOpen, genericAll, 0)
	if status := c.rename(f, "moved.txt", false); status != wire.StatusAccessDenied {
		t.Errorf("rename with a stream open: %#x, want ACCESS_DENIED", status)
	}
	if status := c.rename(s, "r.txt:t", false); status != wire.StatusNotSupported {
		t.Errorf("rename a stream: %#x, want NOT_SUPPORTED", status)
	}
	c.close(s)
	if status := c.rename(f, "moved.txt", false); status != wire.StatusSuccess {
		t.Errorf("rename once the stream closed: %#x", status)
	}
	c.close(f)
}

// fsAttributes reads FILE_FS_ATTRIBUTE_INFORMATION's flags.
func fsAttributes(t *testing.T, backend vfs.Backend) uint32 {
	t.Helper()
	c := connectDeleteClient(t, newTestServer(backend))
	fid := c.mustOpen("", wire.FileOpen, accessRead, 0)
	mustWrite(t, c.fc, buildQueryFilesystem(c.sessID, c.treeID, fid, wire.FileFsAttributeInformation))
	rh, resp := readReply(t, c.fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("query attributes: %#x", rh.Status)
	}
	return binary.LittleEndian.Uint32(resp[64+8 : 64+12])
}

// noStreams hides the in-memory backend's stream support.
type noStreams struct{ inner *memBackend }

func (b noStreams) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	return b.inner.Open(ctx, opts)
}

// Named streams are advertised only when the backend keeps them, and a
// backend that does not is never handed "file:stream" as a file name.
func TestNamedStreamsNeedBackendSupport(t *testing.T) {
	const fileNamedStreams = 0x00040000
	if fsAttributes(t, newMemBackend())&fileNamedStreams == 0 {
		t.Error("named streams not advertised for a backend that keeps them")
	}
	plain := noStreams{newMemBackend()}
	if fsAttributes(t, plain)&fileNamedStreams != 0 {
		t.Error("named streams advertised for a backend without them")
	}

	c := connectDeleteClient(t, newTestServer(plain))
	c.put("f.txt", "x")
	if _, status := c.open("f.txt:s", wire.FileOverwriteIf, genericAll, 0); status != wire.StatusObjectNameInvalid {
		t.Errorf("stream open on a backend without streams: %#x, want OBJECT_NAME_INVALID", status)
	}
	if plain.inner.exists("f.txt:s") {
		t.Fatal("a file named after the stream was created")
	}
}

// Windows first asks for the stream list with a small buffer; a list that
// does not fit is answered with STATUS_BUFFER_OVERFLOW and only whole
// entries, so the client asks again with more room.
func TestStreamInformationOverflow(t *testing.T) {
	backend := newMemBackend()
	c := connectDeleteClient(t, newTestServer(backend))
	c.put("o.txt", "x")
	s := c.mustOpen("o.txt:note", wire.FileOverwriteIf, genericAll, 0)
	c.write(s, "y")
	c.close(s)
	f := c.mustOpen("o.txt", wire.FileOpen, accessRead, 0)

	for _, tc := range []struct {
		outLen  uint32
		entries int
	}{{32, 0}, {48, 1}, {4096, 2}} {
		msg := buildQueryInfo(c.sessID, c.treeID, f, wire.FileStreamInformation)
		binary.LittleEndian.PutUint32(msg[wire.HeaderSize+4:wire.HeaderSize+8], tc.outLen)
		mustWrite(t, c.fc, msg)
		rh, resp := readReply(t, c.fc)
		wantStatus := wire.StatusBufferOverflow
		if tc.entries == 2 {
			wantStatus = wire.StatusSuccess
		}
		if rh.Status != wantStatus {
			t.Errorf("buffer %d: status %#x, want %#x", tc.outLen, rh.Status, wantStatus)
			continue
		}
		length := binary.LittleEndian.Uint32(resp[64+4 : 64+8])
		if length > tc.outLen {
			t.Errorf("buffer %d: returned %d bytes", tc.outLen, length)
		}
		buf := resp[64+8 : 64+8+int(length)]
		got := 0
		for len(buf) >= 24 {
			got++
			next := binary.LittleEndian.Uint32(buf[0:4])
			if next == 0 {
				break
			}
			buf = buf[next:]
		}
		if got != tc.entries {
			t.Errorf("buffer %d: %d whole entries, want %d", tc.outLen, got, tc.entries)
		}
	}
	c.close(f)
}
