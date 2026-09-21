package posix

import (
	"encoding/base64"
	"encoding/binary"
	"sort"
)

// Named data streams are kept in one metadata value: each stream as its
// name and its bytes, both length-prefixed, sorted by name, the whole
// base64-encoded because metadata values must be ASCII. Keeping them in the
// object's metadata means they move with renames and copies, go with
// deletes, and never appear in the bucket as objects of their own. The cost
// is size: object stores cap metadata at a few kilobytes, so protocols
// enforce a limit (see StreamsSize).

// EncodeStreams encodes named streams as a metadata value.
func EncodeStreams(streams map[string][]byte) string {
	return base64.RawStdEncoding.EncodeToString(appendStreams(nil, streams))
}

func appendStreams(dst []byte, streams map[string][]byte) []byte {
	names := make([]string, 0, len(streams))
	for name := range streams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dst = binary.AppendUvarint(dst, uint64(len(name)))
		dst = append(dst, name...)
		dst = binary.AppendUvarint(dst, uint64(len(streams[name])))
		dst = append(dst, streams[name]...)
	}
	return dst
}

// DecodeStreams decodes a metadata value written by EncodeStreams. A value
// that does not decode yields no streams rather than an error: the file's
// data is intact either way.
func DecodeStreams(value string) map[string][]byte {
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	streams := make(map[string][]byte)
	for len(raw) > 0 {
		name, rest, ok := takeField(raw)
		if !ok {
			return nil
		}
		data, rest, ok := takeField(rest)
		if !ok {
			return nil
		}
		streams[string(name)] = append([]byte(nil), data...)
		raw = rest
	}
	if len(streams) == 0 {
		return nil
	}
	return streams
}

func takeField(b []byte) (field, rest []byte, ok bool) {
	n, size := binary.Uvarint(b)
	if size <= 0 || n > uint64(len(b)-size) {
		return nil, nil, false
	}
	b = b[size:]
	return b[:n], b[n:], true
}

// StreamsSize is the number of bytes streams occupy in metadata once
// encoded, which is what the object store's limit applies to.
func StreamsSize(streams map[string][]byte) int {
	if len(streams) == 0 {
		return 0
	}
	return base64.RawStdEncoding.EncodedLen(len(appendStreams(nil, streams)))
}
