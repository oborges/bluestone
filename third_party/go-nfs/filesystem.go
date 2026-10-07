package nfs

import "time"

// FSStat returns metadata about a file system
type FSStat struct {
	TotalSize      uint64
	FreeSize       uint64
	AvailableSize  uint64
	TotalFiles     uint64
	FreeFiles      uint64
	AvailableFiles uint64
	// CacheHint is called "invarsec" in the nfs standard
	CacheHint time.Duration
}

// Committer is implemented by a filesystem that accepts a write before the
// data is on stable storage. For such a filesystem a write the client asked
// for as UNSTABLE is answered as UNSTABLE, and any other write, and COMMIT,
// call Commit before they answer. A filesystem that does not implement it has
// every write reported as FILE_SYNC and nothing to do on COMMIT.
type Committer interface {
	// Commit returns once everything written to the file is on stable
	// storage. A file with nothing outstanding, or that no longer exists,
	// is not an error.
	Commit(filename string) error
}
