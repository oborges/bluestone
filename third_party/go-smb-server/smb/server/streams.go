package server

import (
	"context"
	"strings"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// streamNameError reports a stream name the server does not accept.
type streamNameError struct{}

func (streamNameError) Error() string { return "invalid stream name" }

// splitStream separates a stream name from a path, as in
// "dir\file.txt:Zone.Identifier:$DATA" (MS-FSCC section 2.1.5). The stream
// type, when given, must be $DATA; "file::$DATA" names the file's own data,
// and returns an empty stream. A colon anywhere but the last component is
// not a stream name, and neither is an empty one.
func splitStream(name string) (base, stream string, err error) {
	sep := strings.LastIndexAny(name, `\/`)
	colon := strings.IndexByte(name[sep+1:], ':')
	if colon < 0 {
		if strings.IndexByte(name, ':') >= 0 {
			return "", "", streamNameError{}
		}
		return name, "", nil
	}
	colon += sep + 1
	base, rest := name[:colon], name[colon+1:]
	if strings.IndexByte(base, ':') >= 0 {
		return "", "", streamNameError{}
	}
	stream, kind, typed := strings.Cut(rest, ":")
	if typed && !strings.EqualFold(kind, "$DATA") {
		return "", "", streamNameError{}
	}
	if stream == "" && !typed {
		// "file:" names nothing.
		return "", "", streamNameError{}
	}
	return base, stream, nil
}

// streamPath is the client form of a named stream's path.
func streamPath(base, stream string) string {
	if stream == "" {
		return base
	}
	return base + ":" + stream
}

// supportsStreams reports whether the share's backend keeps named streams.
func supportsStreams(backend vfs.Backend) bool {
	_, ok := backend.(vfs.StreamOpener)
	return ok
}

// streamInformation builds FILE_STREAM_INFORMATION (MS-FSCC section
// 2.4.44): the file's own data as "::$DATA", which a directory does not
// have, then each named stream as ":name:$DATA". Entries are 8-byte aligned.
// Only whole entries are returned: when they do not all fit in maxLen,
// overflow reports it, and the client asks again with more room.
func streamInformation(ctx context.Context, h vfs.Handle, fi vfs.FileInfo, maxLen uint32) (out []byte, overflow bool, err error) {
	type entry struct {
		name string
		size int64
	}
	var entries []entry
	if !fi.IsDir {
		entries = append(entries, entry{"::$DATA", fi.Size})
	}
	if lister, ok := h.(vfs.StreamLister); ok {
		streams, err := lister.Streams(ctx)
		if err != nil {
			return nil, false, err
		}
		for _, s := range streams {
			entries = append(entries, entry{":" + s.Name + ":$DATA", s.Size})
		}
	}

	prev := -1
	for _, e := range entries {
		start := (len(out) + 7) &^ 7
		name := wire.UTF16ToBytes(e.name)
		if uint64(start)+24+uint64(len(name)) > uint64(maxLen) {
			overflow = true
			break
		}
		for len(out) < start {
			out = append(out, 0)
		}
		if prev >= 0 {
			putLE32(out[prev:prev+4], uint32(start-prev))
		}
		prev = start
		rec := make([]byte, 24+len(name))
		putLE32(rec[4:8], uint32(len(name)))
		put64LE(rec[8:16], uint64(e.size))
		put64LE(rec[16:24], uint64(e.size))
		copy(rec[24:], name)
		out = append(out, rec...)
	}
	if out == nil {
		out = []byte{}
	}
	return out, overflow, nil
}
