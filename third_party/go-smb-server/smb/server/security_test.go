package server

import (
	"encoding/binary"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// buildQuerySecurity asks for a file's security descriptor, naming the parts
// wanted and the buffer the client is offering.
func buildQuerySecurity(sessID uint64, treeID uint32, fid [16]byte, additional, outputLen uint32) []byte {
	hdr := wire.NewHeader(wire.CmdQueryInfo)
	hdr.SessionId = sessID
	hdr.TreeId = treeID
	hdr.MessageId = 30
	hdr.Credit = 1
	var body [41]byte
	binary.LittleEndian.PutUint16(body[0:2], 41)
	body[2] = wire.InfoSecurity
	binary.LittleEndian.PutUint32(body[4:8], outputLen)
	binary.LittleEndian.PutUint32(body[16:20], additional)
	copy(body[24:40], fid[:])
	return append(hdr.Append(nil), body[:]...)
}

// Windows asks for a file's security descriptor to show its Security tab,
// and some applications ask on open. Answering STATUS_NOT_SUPPORTED makes
// Windows report that it cannot read the file's security information.
func TestQuerySecurityDescriptor(t *testing.T) {
	backend := newMemBackend()
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newTestServer(backend), srvConn)()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	mustWrite(t, fc, buildCreate(sessID, treeID, "secured.txt", wire.FileOpenIf))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("create: %#x", rh.Status)
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])

	const additional = wire.OwnerSecurityInformation | wire.GroupSecurityInformation | wire.DACLSecurityInformation
	mustWrite(t, fc, buildQuerySecurity(sessID, treeID, fid, additional, 4096))
	rh, resp = readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("query security: status %#x, want success", rh.Status)
	}
	bufLen := binary.LittleEndian.Uint32(resp[64+4 : 64+8])
	if bufLen < 20 {
		t.Fatalf("descriptor is %d bytes, too short to be one", bufLen)
	}
	sd := resp[64+8 : 64+8+int(bufLen)]
	if sd[0] != 1 {
		t.Errorf("descriptor revision = %d, want 1", sd[0])
	}
	if control := binary.LittleEndian.Uint16(sd[2:4]); control&0x8000 == 0 {
		t.Errorf("control = %#x, want the self-relative bit", control)
	}
	// Owner and DACL offsets point inside the descriptor.
	for _, field := range []struct {
		name   string
		offset uint32
	}{
		{"owner", binary.LittleEndian.Uint32(sd[4:8])},
		{"dacl", binary.LittleEndian.Uint32(sd[16:20])},
	} {
		if field.offset == 0 || field.offset >= bufLen {
			t.Errorf("%s offset = %d, outside a descriptor of %d bytes", field.name, field.offset, bufLen)
		}
	}
}

// A client asks with a small buffer first and retries with the size the
// server reports (MS-SMB2 3.3.5.20.3).
func TestQuerySecurityBufferTooSmall(t *testing.T) {
	backend := newMemBackend()
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newTestServer(backend), srvConn)()

	fc := transport.NewFramedConn(client)
	negotiate(t, fc)
	sessID := sessionSetup(t, fc)
	treeID := treeConnect(t, fc, sessID)
	mustWrite(t, fc, buildCreate(sessID, treeID, "secured.txt", wire.FileOpenIf))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("create: %#x", rh.Status)
	}
	var fid [16]byte
	copy(fid[:], resp[64+64:64+80])

	const additional = wire.OwnerSecurityInformation | wire.DACLSecurityInformation
	mustWrite(t, fc, buildQuerySecurity(sessID, treeID, fid, additional, 4))
	rh, resp = readReply(t, fc)
	if rh.Status != wire.StatusBufferTooSmall {
		t.Fatalf("query with a 4-byte buffer: status %#x, want BUFFER_TOO_SMALL", rh.Status)
	}
	// The error carries the size to ask for.
	if len(resp) < 64+12 {
		t.Fatalf("error response is %d bytes, too short to carry the required size", len(resp))
	}
	needed := binary.LittleEndian.Uint32(resp[64+8 : 64+12])
	if needed < 20 {
		t.Fatalf("required size = %d, want the descriptor's real size", needed)
	}

	// Asking again with that size succeeds.
	mustWrite(t, fc, buildQuerySecurity(sessID, treeID, fid, additional, needed))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("retry with the reported size: status %#x, want success", rh.Status)
	}
}

// AdditionalInformation sits at offset 16 of the request, before the FileId
// (MS-SMB2 2.2.37). Reading it from the FileId's bytes made every request
// look like it asked for whatever the handle's first bytes happened to be.
func TestQueryInfoParsesAdditionalInformation(t *testing.T) {
	var fid [16]byte
	for i := range fid {
		fid[i] = 0xEE
	}
	msg := buildQuerySecurity(1, 2, fid, wire.GroupSecurityInformation, 1234)

	var req wire.QueryInfoRequest
	if err := req.Parse(msg); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if req.AdditionalInfo != wire.GroupSecurityInformation {
		t.Errorf("AdditionalInfo = %#x, want %#x", req.AdditionalInfo, wire.GroupSecurityInformation)
	}
	if req.OutputBufferLength != 1234 {
		t.Errorf("OutputBufferLength = %d, want 1234", req.OutputBufferLength)
	}
	if req.FileId != fid {
		t.Errorf("FileId = %x, want %x", req.FileId, fid)
	}
}
