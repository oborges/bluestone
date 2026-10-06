package nfs

import (
	"fmt"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/oborges/bluestone/internal/logging"
	"go.uber.org/zap"
)

// The per-directory call records must not outlive their use: one kept for
// every directory ever listed grows with the life of the gateway.
func TestInstrumentedFilesystemTracksOnlyRecentlyListedDirectories(t *testing.T) {
	base := memfs.New()
	fs := NewInstrumentedFilesystem(base, logging.NewKVLogger(zap.NewNop()))

	const dirs = 3 * maxTrackedPaths
	for i := 0; i < dirs; i++ {
		dir := fmt.Sprintf("d%05d", i)
		if err := base.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
		if _, err := fs.ReadDir(dir); err != nil {
			t.Fatalf("ReadDir(%s) error = %v", dir, err)
		}
	}

	if got := len(fs.GetAllPathStats()); got != maxTrackedPaths {
		t.Fatalf("tracking %d directories after listing %d, want %d", got, dirs, maxTrackedPaths)
	}
	if calls := fs.GetPathStats("d00000")["calls"]; calls != 0 {
		t.Fatalf("the first directory listed is still tracked: %v", fs.GetPathStats("d00000"))
	}

	// A directory listed over and over is still counted, which is what the
	// records are for.
	last := fmt.Sprintf("d%05d", dirs-1)
	for i := 0; i < 4; i++ {
		if _, err := fs.ReadDir(last); err != nil {
			t.Fatalf("ReadDir(%s) error = %v", last, err)
		}
	}
	if calls := fs.GetPathStats(last)["total_calls"]; calls != int64(5) {
		t.Fatalf("calls recorded for %s = %v, want 5", last, calls)
	}
}
