package server

import (
	"encoding/binary"
	"errors"

	"github.com/sonroyaalmerol/go-smb-server/smb/encryption"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func (c *conn) openTransform(transform []byte) ([]byte, error) {
	if len(transform) < encryption.TransformHeaderSize {
		return nil, errors.New("server: transform shorter than header")
	}
	sessID := binary.LittleEndian.Uint64(transform[44:52])
	sess := c.getSession(sessID)
	if sess == nil || sess.decCipher == nil {
		return nil, errors.New("server: no decryption key for session")
	}
	return sess.decCipher.Open(transform)
}

func (c *conn) maybeSealResponse(out []byte) ([]byte, bool) {
	if len(out) < wire.HeaderSize {
		return nil, false
	}
	cmd := binary.LittleEndian.Uint16(out[12:14])
	if cmd == wire.CmdNegotiate || cmd == wire.CmdSessionSetup {
		return nil, false
	}
	sessID := binary.LittleEndian.Uint64(out[40:48])
	sess := c.getSession(sessID)
	if sess == nil || sess.encCipher == nil || !sess.requireEncrypt {
		return nil, false
	}
	sealed, err := sess.encCipher.Seal(out, sessID)
	if err != nil {
		c.log.Debug("seal response", "err", err)
		return nil, false
	}
	return sealed, true
}

func (c *conn) negotiateCapabilities(dialect uint16) uint32 {
	caps := wire.CapLargeMTU
	if c.srv.leasesEnabled {
		caps |= wire.CapLeasing
	}
	// SMB 3.1.1 negotiates encryption with a negotiate context instead of
	// this capability, and clients reject it set.
	if c.srv.requireEnc && dialect != wire.DialectSMB311 {
		caps |= wire.CapEncryption
	}
	return caps
}
