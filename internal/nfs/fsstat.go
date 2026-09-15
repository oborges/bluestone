package nfs

import (
	"context"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/vfs"
	gonfs "github.com/willscott/go-nfs"
)

// fsStatFrom fills an NFS FSSTAT reply from a wrapped filesystem: another NFS
// wrapper forwards its own answer, and the shared filesystem layer reports
// protocol-neutral capacity that is translated here.
func fsStatFrom(ctx context.Context, inner billy.Filesystem, stat *gonfs.FSStat) error {
	switch provider := inner.(type) {
	case interface {
		FSStat(context.Context, *gonfs.FSStat) error
	}:
		return provider.FSStat(ctx, stat)
	case interface{ Capacity() vfs.Capacity }:
		capacity := provider.Capacity()
		stat.TotalSize = capacity.TotalBytes
		stat.FreeSize = capacity.AvailableBytes
		stat.AvailableSize = capacity.AvailableBytes
		stat.TotalFiles = capacity.TotalFiles
		stat.FreeFiles = capacity.AvailableFiles
		stat.AvailableFiles = capacity.AvailableFiles
		stat.CacheHint = time.Second
	}
	return nil
}
