package server

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"testing"

	"github.com/sonroyaalmerol/go-smb-server/smb/encryption"
	"github.com/sonroyaalmerol/go-smb-server/smb/kerberos"
	"github.com/sonroyaalmerol/go-smb-server/smb/signing"
	"github.com/sonroyaalmerol/go-smb-server/smb/transport"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// negotiateContext encodes one negotiate context's data.
func negotiateContext(ctxType uint16, data []byte) wire.NegotiateContext {
	return wire.NegotiateContext{Type: ctxType, Data: data}
}

func uint16s(vals ...uint16) []byte {
	b := make([]byte, 2+2*len(vals))
	binary.LittleEndian.PutUint16(b[0:2], uint16(len(vals)))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2+2*i:], v)
	}
	return b
}

// buildNegotiate311 is a NEGOTIATE offering 3.0.2 and 3.1.1 with contexts.
func buildNegotiate311(contexts []wire.NegotiateContext) []byte {
	hdr := wire.NewHeader(wire.CmdNegotiate)
	hdr.Credit = 1
	dialects := []uint16{wire.DialectSMB302, wire.DialectSMB311}
	body := make([]byte, 36+2*len(dialects))
	binary.LittleEndian.PutUint16(body[0:2], 36)
	binary.LittleEndian.PutUint16(body[2:4], uint16(len(dialects)))
	body[12] = 7 // ClientGuid
	for i, d := range dialects {
		binary.LittleEndian.PutUint16(body[36+2*i:], d)
	}
	for (wire.HeaderSize+len(body))%8 != 0 {
		body = append(body, 0)
	}
	binary.LittleEndian.PutUint32(body[28:32], uint32(wire.HeaderSize+len(body)))
	binary.LittleEndian.PutUint16(body[32:34], uint16(len(contexts)))
	for i, c := range contexts {
		entry := make([]byte, 8+len(c.Data))
		binary.LittleEndian.PutUint16(entry[0:2], c.Type)
		binary.LittleEndian.PutUint16(entry[2:4], uint16(len(c.Data)))
		copy(entry[8:], c.Data)
		body = append(body, entry...)
		if i < len(contexts)-1 {
			for (wire.HeaderSize+len(body))%8 != 0 {
				body = append(body, 0)
			}
		}
	}
	return append(hdr.Append(nil), body...)
}

func preauthRequestContext() wire.NegotiateContext {
	data := make([]byte, 6+32)
	binary.LittleEndian.PutUint16(data[0:2], 1)
	binary.LittleEndian.PutUint16(data[2:4], 32)
	binary.LittleEndian.PutUint16(data[4:6], wire.HashSHA512)
	return negotiateContext(wire.CtxPreauthIntegrity, data)
}

// responseContexts311 reads a NEGOTIATE response's contexts by type.
func responseContexts311(t *testing.T, resp []byte) map[uint16][]byte {
	t.Helper()
	body := resp[wire.HeaderSize:]
	count := int(binary.LittleEndian.Uint16(body[6:8]))
	off := int(binary.LittleEndian.Uint32(body[60:64]))
	out := map[uint16][]byte{}
	for range count {
		ctxType := binary.LittleEndian.Uint16(resp[off : off+2])
		n := int(binary.LittleEndian.Uint16(resp[off+2 : off+4]))
		out[ctxType] = resp[off+8 : off+8+n]
		off = (off + 8 + n + 7) &^ 7
	}
	return out
}

func chain(hash, msg []byte) []byte {
	h := sha512.New()
	h.Write(hash)
	h.Write(msg)
	return h.Sum(nil)
}

// A client negotiating 3.1.1 gets SHA-512 pre-authentication integrity with a
// salt, the first cipher it offers that the server supports, AES-CMAC
// signing, and no CAP_ENCRYPTION.
func TestNegotiate311(t *testing.T) {
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	srv, err := New(WithShares(vfs.NewDiskShare("share", newMemBackend())), WithLogger(discardLogger()), WithEncryptionRequired())
	if err != nil {
		t.Fatal(err)
	}
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)

	mustWrite(t, fc, buildNegotiate311([]wire.NegotiateContext{
		preauthRequestContext(),
		negotiateContext(wire.CtxEncryption, uint16s(0x0009, encryption.CipherAES128GCM, encryption.CipherAES128CCM)),
		negotiateContext(wire.CtxSigning, uint16s(wire.SigningAESGMAC, wire.SigningAESCMAC)),
	}))
	rh, resp := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("negotiate: %#x", rh.Status)
	}
	if dialect := binary.LittleEndian.Uint16(resp[64+4 : 64+6]); dialect != wire.DialectSMB311 {
		t.Fatalf("dialect %#x, want 3.1.1", dialect)
	}
	if caps := binary.LittleEndian.Uint32(resp[64+24 : 64+28]); caps&wire.CapEncryption != 0 {
		t.Error("CAP_ENCRYPTION set on a 3.1.1 negotiation")
	}
	ctxs := responseContexts311(t, resp)
	pre := ctxs[wire.CtxPreauthIntegrity]
	if len(pre) < 6 || binary.LittleEndian.Uint16(pre[4:6]) != wire.HashSHA512 || binary.LittleEndian.Uint16(pre[2:4]) != 32 {
		t.Errorf("preauth context %x, want SHA-512 with a 32-byte salt", pre)
	}
	if enc := ctxs[wire.CtxEncryption]; len(enc) < 4 || binary.LittleEndian.Uint16(enc[2:4]) != encryption.CipherAES128GCM {
		t.Errorf("encryption context %x, want AES-128-GCM, the first supported cipher offered", enc)
	}
	if sig := ctxs[wire.CtxSigning]; len(sig) < 4 || binary.LittleEndian.Uint16(sig[2:4]) != wire.SigningAESCMAC {
		t.Errorf("signing context %x, want AES-CMAC", sig)
	}
}

// Without pre-authentication integrity there is no 3.1.1.
func TestNegotiate311NeedsPreauth(t *testing.T) {
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	srv, err := New(WithShares(vfs.NewDiskShare("share", newMemBackend())), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)
	mustWrite(t, fc, buildNegotiate311(nil))
	if rh, _ := readReply(t, fc); rh.Status == wire.StatusSuccess {
		t.Fatal("3.1.1 negotiated without a preauth integrity context")
	}
}

// The session's keys come from its pre-authentication integrity hash: the
// NEGOTIATE request and response, then the SESSION_SETUP request, as the
// client saw them. A client working the hash out on its own side verifies
// the server's signed SESSION_SETUP response and is accepted signing with
// the same key.
func TestSession311KeysFromPreauthHash(t *testing.T) {
	kt := newKerbTestKeytab(t)
	token, sessionKey := buildKerbToken(t, kt)
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	defer serveOn(newKerbTestServer(t, newMemBackend(), kt), srvConn)()
	fc := transport.NewFramedConn(client)

	negotiate := buildNegotiate311([]wire.NegotiateContext{
		preauthRequestContext(),
		negotiateContext(wire.CtxEncryption, uint16s(encryption.CipherAES128GCM)),
	})
	hash := chain(make([]byte, 64), negotiate)
	mustWrite(t, fc, negotiate)
	negResp, err := fc.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	hash = chain(hash, negResp)

	setup := buildSessionSetup(token)
	hash = chain(hash, setup)
	mustWrite(t, fc, setup)
	setupResp, err := fc.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var h wire.Header
	_ = h.Parse(setupResp)
	if h.Status != wire.StatusSuccess {
		t.Fatalf("session setup: %#x", h.Status)
	}

	signKey, _, _ := encryption.Keys311(sessionKey, hash, 16)
	verifier, err := signing.NewSigner(signKey)
	if err != nil {
		t.Fatal(err)
	}
	if h.Flags&wire.FlagSigned == 0 {
		t.Fatal("final SESSION_SETUP response is not signed")
	}
	if ok, err := verifier.Verify(append([]byte(nil), setupResp...)); err != nil || !ok {
		t.Fatal("SESSION_SETUP response signature does not verify with the key from the preauth hash")
	}
	mustWrite(t, fc, signedTreeConnect(h.SessionId, 2, `\\server\share`, signKey))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("tree connect signed with the 3.1.1 key: %#x", rh.Status)
	}
	// And the 3.0 key is not the one in use.
	if bytes.Equal(signKey, signing.DeriveSigningKey(sessionKey[:16])) {
		t.Fatal("3.1.1 signing key equals the 3.0 one")
	}
}

// A 3.0 client flags FSCTL_VALIDATE_NEGOTIATE_INFO as signed even when it
// encrypts it, with a signature the server cannot check. A request that
// arrived encrypted is authenticated by its transform, so its signature is
// not verified: Windows failed its tree connects with encryption required
// until it was not.
func TestEncryptedRequestSkipsSignature(t *testing.T) {
	kt := newKerbTestKeytab(t)
	token, sessionKey := buildKerbToken(t, kt)
	client, srvConn := newPipeConns()
	defer func() { _ = client.Close() }()
	srv, err := New(WithAuth(kerberos.NewServer(kt)), WithShares(vfs.NewDiskShare("share", newMemBackend())),
		WithLogger(discardLogger()), WithEncryptionRequired(), WithDialect(wire.DialectSMB302))
	if err != nil {
		t.Fatal(err)
	}
	defer serveOn(srv, srvConn)()
	fc := transport.NewFramedConn(client)

	mustWrite(t, fc, buildNegotiate311(nil))
	if rh, _ := readReply(t, fc); rh.Status != wire.StatusSuccess {
		t.Fatalf("negotiate: %#x", rh.Status)
	}
	mustWrite(t, fc, buildSessionSetup(token))
	rh, _ := readReply(t, fc)
	if rh.Status != wire.StatusSuccess {
		t.Fatalf("session setup: %#x", rh.Status)
	}

	req := buildTreeConnect(rh.SessionId, `\\server\share`)
	binary.LittleEndian.PutUint32(req[16:20], wire.FlagSigned)
	copy(req[48:64], bytes.Repeat([]byte{0xAB}, 16))
	seal, err := encryption.NewAESCCM(encryption.DeriveServerDecryptionKey(sessionKey[:16]))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := seal.Seal(req, rh.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, fc, sealed)
	resp, err := fc.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	open, err := encryption.NewAESCCM(encryption.DeriveServerEncryptionKey(sessionKey[:16]))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := open.Open(resp)
	if err != nil {
		t.Fatalf("response is not sealed with the session's key: %v", err)
	}
	var h wire.Header
	_ = h.Parse(plain)
	if h.Status != wire.StatusSuccess {
		t.Fatalf("encrypted tree connect flagged as signed: %#x", h.Status)
	}
}
