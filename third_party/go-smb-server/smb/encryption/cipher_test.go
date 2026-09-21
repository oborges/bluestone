package encryption

import (
	"bytes"
	"testing"
)

func TestCiphersRoundTrip(t *testing.T) {
	msg := []byte("\xfeSMB a message the server sends, long enough to span several AES blocks")
	for _, id := range []uint16{CipherAES128CCM, CipherAES128GCM, CipherAES256CCM, CipherAES256GCM} {
		key := bytes.Repeat([]byte{0x42}, KeyLen(id))
		sealer, err := NewCipher(id, key)
		if err != nil {
			t.Fatalf("%s: %v", CipherName(id), err)
		}
		opener, _ := NewCipher(id, key)
		sealed, err := sealer.Seal(msg, 7)
		if err != nil {
			t.Fatalf("%s seal: %v", CipherName(id), err)
		}
		if bytes.Contains(sealed, msg[4:20]) {
			t.Fatalf("%s: plaintext visible in the sealed message", CipherName(id))
		}
		got, err := opener.Open(append([]byte(nil), sealed...))
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("%s open = %q, %v", CipherName(id), got, err)
		}
		tampered := append([]byte(nil), sealed...)
		tampered[len(tampered)-1] ^= 1
		if _, err := opener.Open(tampered); err == nil {
			t.Fatalf("%s accepted a tampered message", CipherName(id))
		}
	}
}

// Two messages sealed under one key never share a GCM nonce.
func TestGCMNoncesDoNotRepeat(t *testing.T) {
	g, err := NewAESGCM(bytes.Repeat([]byte{1}, 16))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 1000 {
		sealed, _ := g.Seal([]byte("x"), 1)
		nonce := string(sealed[20:32])
		if seen[nonce] {
			t.Fatal("nonce repeated")
		}
		seen[nonce] = true
	}
}

// The 3.1.1 KDF is the 3.0 one with a longer context and the key length
// written in, so at 128 bits the same label and context give the 3.0 result.
func TestKDFMatches30Construction(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 16)
	if !bytes.Equal(kdf(key, []byte("SMB2AESCCM\x00"), []byte("ServerOut\x00"), 16), DeriveServerEncryptionKey(key)) {
		t.Fatal("KDF disagrees with the SMB 3.0 derivation")
	}
	if len(kdf(key, []byte("SMBS2CCipherKey\x00"), make([]byte, 64), 32)) != 32 {
		t.Fatal("256-bit key is not 32 bytes")
	}
}
