package nfs

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/vfs"
	gonfs "github.com/willscott/go-nfs"
	"go.uber.org/zap"
)

// capacityFS stands in for the shared filesystem layer's capacity report.
type capacityFS struct {
	billy.Filesystem
	capacity vfs.Capacity
}

func (c capacityFS) Capacity() vfs.Capacity { return c.capacity }

func TestFSStatThroughWrappersReportsSharedFilesystemCapacity(t *testing.T) {
	inner := capacityFS{capacity: vfs.Capacity{
		TotalBytes:     1000,
		AvailableBytes: 400,
		TotalFiles:     50,
		AvailableFiles: 20,
	}}
	logger := logging.NewKVLogger(zap.NewNop())
	// Same wrapper order as the server wiring in cmd/bluestone.
	wrapped := NewInstrumentedFilesystem(NewCachedFilesystem(inner, logger, time.Second), logger)

	var stat gonfs.FSStat
	if err := wrapped.FSStat(context.Background(), &stat); err != nil {
		t.Fatalf("FSStat() error = %v", err)
	}
	want := gonfs.FSStat{
		TotalSize:      1000,
		FreeSize:       400,
		AvailableSize:  400,
		TotalFiles:     50,
		FreeFiles:      20,
		AvailableFiles: 20,
		CacheHint:      time.Second,
	}
	if stat != want {
		t.Fatalf("FSStat() = %+v, want %+v", stat, want)
	}
}
