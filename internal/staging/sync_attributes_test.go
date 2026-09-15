package staging

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oborges/bluestone/internal/posix"
)

func TestSyncUploadCarriesEveryStagedAttribute(t *testing.T) {
	cfg := createTestConfig(t)
	manager, err := NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	cosClient := NewMockCOSClient()
	worker := NewSyncWorker(manager, cosClient, cfg)

	var mu sync.Mutex
	var uploaded map[string]string
	cosClient.putObjectFn = func(ctx context.Context, key string, data []byte, metadata map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		uploaded = metadata
		return nil
	}

	path := "/docs/report.txt"
	data := []byte("staged bytes")
	session, err := manager.GetOrCreateSession(path)
	if err != nil {
		t.Fatalf("GetOrCreateSession() error = %v", err)
	}
	if _, err := session.Write(data, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := session.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	btime := time.Date(2022, 7, 8, 9, 10, 11, 120000000, time.UTC)
	session.SeedAttributes(StagedAttributes{
		Mode:              0640,
		UID:               42,
		GID:               7,
		Btime:             btime,
		WindowsAttributes: posix.WindowsAttributeHidden | posix.WindowsAttributeArchive,
	})
	manager.MarkDirty(path, int64(len(data)))

	if err := worker.syncFile(path); err != nil {
		t.Fatalf("syncFile() error = %v", err)
	}

	mu.Lock()
	metadata := uploaded
	mu.Unlock()
	if metadata == nil {
		t.Fatal("no upload recorded")
	}
	got := posix.DecodePOSIXAttributes(metadata, false)
	if got.Mode != 0640 || got.UID != 42 || got.GID != 7 {
		t.Fatalf("uploaded mode/owner = %o/%d/%d, want 640/42/7", got.Mode, got.UID, got.GID)
	}
	if !got.Btime.Equal(btime) {
		t.Fatalf("uploaded creation time = %v, want %v", got.Btime, btime)
	}
	if want := posix.WindowsAttributeHidden | posix.WindowsAttributeArchive; got.WindowsAttributes != want {
		t.Fatalf("uploaded Windows attributes = %#x, want %#x", got.WindowsAttributes, want)
	}
}
