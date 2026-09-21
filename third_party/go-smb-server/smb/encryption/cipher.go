package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
)

// Cipher ids negotiated in SMB 3.1.1's encryption capabilities (MS-SMB2
// section 2.2.3.1.2).
const (
	CipherAES128CCM uint16 = 0x0001
	CipherAES128GCM uint16 = 0x0002
	CipherAES256CCM uint16 = 0x0003
	CipherAES256GCM uint16 = 0x0004
)

// Supported reports whether the server can use a cipher.
func Supported(id uint16) bool {
	switch id {
	case CipherAES128CCM, CipherAES128GCM, CipherAES256CCM, CipherAES256GCM:
		return true
	}
	return false
}

// KeyLen is the key length a cipher uses, in bytes.
func KeyLen(id uint16) int {
	if id == CipherAES256CCM || id == CipherAES256GCM {
		return 32
	}
	return 16
}

// CipherName names a cipher for logs.
func CipherName(id uint16) string {
	switch id {
	case CipherAES128CCM:
		return "AES-128-CCM"
	case CipherAES128GCM:
		return "AES-128-GCM"
	case CipherAES256CCM:
		return "AES-256-CCM"
	case CipherAES256GCM:
		return "AES-256-GCM"
	}
	return "none"
}

// Cipher seals messages into SMB2 TRANSFORM_HEADER messages and opens them.
type Cipher interface {
	Seal(msg []byte, sessionID uint64) ([]byte, error)
	Open(transform []byte) ([]byte, error)
}

// NewCipher returns the cipher with the given id over key.
func NewCipher(id uint16, key []byte) (Cipher, error) {
	switch id {
	case CipherAES128CCM, CipherAES256CCM:
		return NewAESCCM(key)
	case CipherAES128GCM, CipherAES256GCM:
		return NewAESGCM(key)
	}
	return nil, fmt.Errorf("encryption: unsupported cipher %#x", id)
}

// AESGCM is AES-GCM for SMB 3.1.1 (MS-SMB2 section 3.1.4.3): a 12-byte
// nonce in the transform header's Nonce field, and the header from Nonce to
// SessionId as additional data.
type AESGCM struct {
	aead cipher.AEAD
	// Nonces are a random prefix and a counter, so none repeats under one
	// key however many messages are sealed.
	prefix  [4]byte
	counter atomic.Uint64
}

func NewAESGCM(key []byte) (*AESGCM, error) {
	if len(key) != 16 && len(key) != 32 {
		return nil, fmt.Errorf("encryption: AES-GCM requires a 16- or 32-byte key, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	g := &AESGCM{aead: aead}
	if _, err := rand.Read(g.prefix[:]); err != nil {
		return nil, fmt.Errorf("encryption: nonce: %w", err)
	}
	return g, nil
}

func (g *AESGCM) Seal(msg []byte, sessionID uint64) ([]byte, error) {
	out := make([]byte, TransformHeaderSize, TransformHeaderSize+len(msg)+16)
	copy(out[0:4], transformProtocolId[:])
	nonce := out[20:32]
	copy(nonce[0:4], g.prefix[:])
	binary.LittleEndian.PutUint64(nonce[4:12], g.counter.Add(1))
	binary.LittleEndian.PutUint32(out[36:40], uint32(len(msg)))
	binary.LittleEndian.PutUint16(out[42:44], 0x0001) // Flags: encrypted
	binary.LittleEndian.PutUint64(out[44:52], sessionID)

	aad := out[20:TransformHeaderSize]
	sealed := g.aead.Seal(nil, nonce, msg, aad)
	ct, tag := sealed[:len(msg)], sealed[len(msg):]
	copy(out[4:20], tag)
	return append(out, ct...), nil
}

func (g *AESGCM) Open(transform []byte) ([]byte, error) {
	if len(transform) < TransformHeaderSize {
		return nil, errors.New("encryption: transform shorter than header")
	}
	if [4]byte(transform[0:4]) != transformProtocolId {
		return nil, errors.New("encryption: bad transform protocol id")
	}
	origSize := binary.LittleEndian.Uint32(transform[36:40])
	if len(transform)-TransformHeaderSize != int(origSize) {
		return nil, fmt.Errorf("encryption: OriginalMessageSize %d != ciphertext %d", origSize, len(transform)-TransformHeaderSize)
	}
	nonce := transform[20:32]
	aad := transform[20:TransformHeaderSize]
	ct := transform[TransformHeaderSize:]
	sealed := make([]byte, 0, len(ct)+16)
	sealed = append(append(sealed, ct...), transform[4:20]...)
	plain, err := g.aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, errors.New("encryption: message failed authentication")
	}
	return plain, nil
}

// Keys311 derives an SMB 3.1.1 session's keys (MS-SMB2 section 3.3.5.5.3):
// the signing key, and the keys for what the server sends and receives,
// from the session key and the session's pre-authentication integrity hash.
func Keys311(sessionKey, preauthHash []byte, cipherKeyLen int) (signing, serverOut, serverIn []byte) {
	signing = kdf(sessionKey, []byte("SMBSigningKey\x00"), preauthHash, 16)
	serverOut = kdf(sessionKey, []byte("SMBS2CCipherKey\x00"), preauthHash, cipherKeyLen)
	serverIn = kdf(sessionKey, []byte("SMBC2SCipherKey\x00"), preauthHash, cipherKeyLen)
	return signing, serverOut, serverIn
}

// kdf is SP800-108 counter mode with HMAC-SHA256, producing length bytes.
func kdf(ki, label, context []byte, length int) []byte {
	var b []byte
	b = append(b, 0x00, 0x00, 0x00, 0x01)
	b = append(b, label...)
	b = append(b, 0x00)
	b = append(b, context...)
	var bits [4]byte
	binary.BigEndian.PutUint32(bits[:], uint32(length*8))
	b = append(b, bits[:]...)
	return hmacSHA256(ki, b)[:length]
}
