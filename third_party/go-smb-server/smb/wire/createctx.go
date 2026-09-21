package wire

import (
	"encoding/binary"
	"fmt"
	"time"
)

// CreateContext is one SMB2_CREATE_CONTEXT of a CREATE request or response
// (MS-SMB2 section 2.2.13.2): a short name tag and its data.
type CreateContext struct {
	Name string
	Data []byte
}

// Create context names.
const (
	CreateContextLease = "RqLs"
)

// parseCreateContexts reads the chain of create contexts at buf. Each
// context's offsets are relative to its own start, and Next links to the
// following one.
func parseCreateContexts(buf []byte) ([]CreateContext, error) {
	var out []CreateContext
	for len(buf) > 0 {
		if len(buf) < 16 {
			return nil, fmt.Errorf("wire: create context shorter than its header")
		}
		next := binary.LittleEndian.Uint32(buf[0:4])
		nameOff := int(binary.LittleEndian.Uint16(buf[4:6]))
		nameLen := int(binary.LittleEndian.Uint16(buf[6:8]))
		dataOff := int(binary.LittleEndian.Uint16(buf[10:12]))
		dataLen := int(binary.LittleEndian.Uint32(buf[12:16]))
		if nameOff+nameLen > len(buf) || (dataLen > 0 && dataOff+dataLen > len(buf)) {
			return nil, fmt.Errorf("wire: create context out of range")
		}
		ctx := CreateContext{Name: string(buf[nameOff : nameOff+nameLen])}
		if dataLen > 0 {
			ctx.Data = buf[dataOff : dataOff+dataLen]
		}
		out = append(out, ctx)
		if next == 0 {
			break
		}
		if int(next) > len(buf) || next < 16 {
			return nil, fmt.Errorf("wire: create context Next out of range")
		}
		buf = buf[next:]
	}
	return out, nil
}

// appendCreateContexts encodes contexts as a chain, each 8-byte aligned.
func appendCreateContexts(dst []byte, contexts []CreateContext) []byte {
	for i, c := range contexts {
		start := len(dst)
		// Header, then the name padded to 8, then the data.
		nameOff := 16
		dataOff := (nameOff + len(c.Name) + 7) &^ 7
		size := dataOff + len(c.Data)
		entry := make([]byte, size)
		binary.LittleEndian.PutUint16(entry[4:6], uint16(nameOff))
		binary.LittleEndian.PutUint16(entry[6:8], uint16(len(c.Name)))
		if len(c.Data) > 0 {
			binary.LittleEndian.PutUint16(entry[10:12], uint16(dataOff))
			binary.LittleEndian.PutUint32(entry[12:16], uint32(len(c.Data)))
		}
		copy(entry[nameOff:], c.Name)
		copy(entry[dataOff:], c.Data)
		dst = append(dst, entry...)
		if i < len(contexts)-1 {
			for (len(dst)-start)%8 != 0 {
				dst = append(dst, 0)
			}
			binary.LittleEndian.PutUint32(dst[start:start+4], uint32(len(dst)-start))
		}
	}
	return dst
}

// Lease states (MS-SMB2 section 2.2.13.2.8).
const (
	LeaseRead   uint32 = 0x01
	LeaseHandle uint32 = 0x02
	LeaseWrite  uint32 = 0x04
)

// Lease flags.
const (
	LeaseFlagBreakInProgress uint32 = 0x02
	LeaseFlagParentKeySet    uint32 = 0x04
)

// OplockLevelLease in a CREATE says the oplock is a lease, described by
// the RqLs create context.
const (
	OplockLevelNone  uint8 = 0x00
	OplockLevelII    uint8 = 0x01
	OplockLevelLease uint8 = 0xFF
)

// LeaseRequest is SMB2_CREATE_REQUEST_LEASE (32 bytes) or its V2 (52 bytes),
// and the same layout carries the server's response.
type LeaseRequest struct {
	Key       [16]byte
	State     uint32
	Flags     uint32
	V2        bool
	ParentKey [16]byte
	Epoch     uint16
}

// ParseLeaseRequest reads a RqLs context's data.
func ParseLeaseRequest(data []byte) (LeaseRequest, error) {
	var r LeaseRequest
	switch {
	case len(data) >= 52:
		r.V2 = true
		copy(r.ParentKey[:], data[32:48])
		r.Epoch = binary.LittleEndian.Uint16(data[48:50])
	case len(data) >= 32:
	default:
		return r, fmt.Errorf("wire: lease context is %d bytes", len(data))
	}
	copy(r.Key[:], data[0:16])
	r.State = binary.LittleEndian.Uint32(data[16:20])
	r.Flags = binary.LittleEndian.Uint32(data[20:24])
	return r, nil
}

// Encode writes the lease as a RqLs response context's data, in the version
// the client asked with.
func (r LeaseRequest) Encode() []byte {
	size := 32
	if r.V2 {
		size = 52
	}
	b := make([]byte, size)
	copy(b[0:16], r.Key[:])
	binary.LittleEndian.PutUint32(b[16:20], r.State)
	binary.LittleEndian.PutUint32(b[20:24], r.Flags)
	if r.V2 {
		copy(b[32:48], r.ParentKey[:])
		binary.LittleEndian.PutUint16(b[48:50], r.Epoch)
	}
	return b
}

// Durable handle create contexts (MS-SMB2 sections 2.2.13.2.3 to
// 2.2.13.2.12). A durable open survives its connection dropping: the client
// reconnects and names it to carry on.
const (
	CreateContextDurableV1          = "DHnQ"
	CreateContextDurableV1Reconnect = "DHnC"
	CreateContextDurableV2          = "DH2Q"
	CreateContextDurableV2Reconnect = "DH2C"
)

// DurableV2Persistent in a DH2Q request asks for a persistent handle, which
// survives the server failing over too.
const DurableV2Persistent uint32 = 0x02

// DurableV2Request is SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2: how long the
// client would like the open kept, and the GUID it names this create by.
type DurableV2Request struct {
	Timeout    uint32 // milliseconds; 0 lets the server choose
	Flags      uint32
	CreateGUID [16]byte
}

func ParseDurableV2Request(data []byte) (DurableV2Request, error) {
	var r DurableV2Request
	if len(data) < 32 {
		return r, fmt.Errorf("wire: DH2Q is %d bytes", len(data))
	}
	r.Timeout = binary.LittleEndian.Uint32(data[0:4])
	r.Flags = binary.LittleEndian.Uint32(data[4:8])
	copy(r.CreateGUID[:], data[16:32])
	return r, nil
}

// DurableV2Response is the DH2Q response: the timeout granted, and flags.
func DurableV2Response(timeout time.Duration) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], uint32(timeout/time.Millisecond))
	return b
}

// DurableV1Response is the DHnQ response, eight reserved bytes.
func DurableV1Response() []byte { return make([]byte, 8) }

// DurableReconnect is what DHnC or DH2C names: the open to reclaim, and for
// V2 the GUID it was created with.
type DurableReconnect struct {
	FileID     [16]byte
	CreateGUID [16]byte
	V2         bool
}

func ParseDurableV1Reconnect(data []byte) (DurableReconnect, error) {
	var r DurableReconnect
	if len(data) < 16 {
		return r, fmt.Errorf("wire: DHnC is %d bytes", len(data))
	}
	copy(r.FileID[:], data[0:16])
	return r, nil
}

func ParseDurableV2Reconnect(data []byte) (DurableReconnect, error) {
	r := DurableReconnect{V2: true}
	if len(data) < 36 {
		return r, fmt.Errorf("wire: DH2C is %d bytes", len(data))
	}
	copy(r.FileID[:], data[0:16])
	copy(r.CreateGUID[:], data[16:32])
	return r, nil
}
