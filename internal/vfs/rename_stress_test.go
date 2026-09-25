package vfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"

	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/staging"
)

// TestRenameUnderSyncPressureKeepsTheFile reproduces the soak's mixed cycle:
// write a file, read it back, rename it, read it back, move it into a fresh
// subdirectory and read it back again, all while sync workers upload the
// staged bytes underneath. The soak lost files at the read after the move
// roughly once in 100,000 cycles, on every protocol, so the race is here
// rather than in any one protocol's server.
func TestRenameUnderSyncPressureKeepsTheFile(t *testing.T) {
	cfg := testStagingConfig(t)
	// The gateway's own settings, with the sync interval pulled in so the
	// uploads that the rename races against happen constantly.
	cfg.SyncInterval = "20ms"
	cfg.SyncThresholdMB = 1
	cfg.SyncWorkerCount = 8
	cfg.SyncQueueSize = 1000
	cfg.MaxDirtyFiles = 50000
	cfg.CleanAfterSync = true

	manager, err := staging.NewStagingManager(cfg)
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	defer manager.Shutdown()

	store := newFakeObjectStore()
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, store)

	worker := staging.NewSyncWorker(manager, &plainUploadStore{fakeObjectStore: store}, cfg)
	worker.SetObjectMutatedCallback(fs.ops.InvalidateFileMutation)
	worker.SetObjectSyncedCallback(fs.ops.InvalidateObjectAfterSync)
	worker.Start()
	defer worker.Stop()

	workers := 8
	cycles := 400
	if testing.Short() {
		cycles = 40
	}

	var mu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dir := fmt.Sprintf("/w%d", w)
			if err := fs.MkdirAll(dir, 0755); err != nil {
				fail("MkdirAll(%s): %v", dir, err)
				return
			}
			for i := 0; i < cycles; i++ {
				if !oneRenameCycle(fs, dir, w, i, fail) {
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d cycles per worker across %d workers lost files:\n  %s",
			cycles, workers, joinLines(failures))
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n  "
		}
		out += l
	}
	return out
}

// oneRenameCycle runs a single write/rename/move/verify cycle and reports
// whether it survived. It returns false once it has recorded a failure.
func oneRenameCycle(fs *Filesystem, dir string, w, i int, fail func(string, ...any)) bool {
	name := fmt.Sprintf("file-%d-%d.bin", w, i)
	src := path.Join(dir, name)
	moved := src + ".moved"
	sub := path.Join(dir, fmt.Sprintf("sub-%d-%d", w, i))
	final := path.Join(sub, "final.bin")

	payload := bytes.Repeat([]byte{byte(w), byte(i), 0x5a}, 4096+i%997)

	f, err := fs.Create(src)
	if err != nil {
		fail("Create(%s): %v", src, err)
		return false
	}
	if _, err := f.Write(payload); err != nil {
		fail("Write(%s): %v", src, err)
		return false
	}
	if err := f.Close(); err != nil {
		fail("Close(%s): %v", src, err)
		return false
	}

	if !verify(fs, src, payload, "after the write", fail) {
		return false
	}
	if err := fs.Rename(src, moved); err != nil {
		fail("Rename(%s -> %s): %v", src, moved, err)
		return false
	}
	if !verify(fs, moved, payload, "after the rename", fail) {
		return false
	}
	if err := fs.MkdirAll(sub, 0755); err != nil {
		fail("MkdirAll(%s): %v", sub, err)
		return false
	}
	if err := fs.Rename(moved, final); err != nil {
		fail("Rename(%s -> %s): %v", moved, final, err)
		return false
	}
	if !verify(fs, final, payload, "after the move", fail) {
		return false
	}
	if err := fs.Remove(final); err != nil {
		fail("Remove(%s): %v", final, err)
		return false
	}
	return true
}

// verify reads a path back and reports whether it still holds what was
// written. A missing file says so explicitly, and says whether it is still
// missing a moment later, because the soak's question was whether the file
// was merely invisible or actually gone.
func verify(fs *Filesystem, at string, want []byte, when string, fail func(string, ...any)) bool {
	f, err := fs.Open(at)
	if err != nil {
		if os.IsNotExist(err) {
			time.Sleep(250 * time.Millisecond)
			_, again := fs.Stat(at)
			fail("%s: %s is missing (%v); 250ms later it is %s", when, at, err,
				map[bool]string{true: "still missing", false: "back"}[again != nil])
			return false
		}
		fail("%s: Open(%s): %v", when, at, err)
		return false
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		fail("%s: ReadAll(%s): %v", when, at, err)
		return false
	}
	if !bytes.Equal(got, want) {
		fail("%s: %s holds %d bytes, want %d", when, at, len(got), len(want))
		return false
	}
	return true
}

// plainUploadStore is the sync worker's view of the fake store: uploads land
// straight away, and the files here are far below the multipart threshold.
type plainUploadStore struct {
	*fakeObjectStore
}

func (s *plainUploadStore) PutObjectStream(ctx context.Context, key string, body io.ReadSeeker, metadata map[string]string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.putWithMetadata(strings.TrimPrefix(key, "/"), data, metadata)
	return nil
}

func (s *plainUploadStore) CreateMultipartUpload(ctx context.Context, key string, metadata map[string]string) (string, error) {
	return "", errNoMultipart
}

func (s *plainUploadStore) UploadPart(ctx context.Context, key, uploadID string, partNumber int64, body io.ReadSeeker) (string, error) {
	return "", errNoMultipart
}

func (s *plainUploadStore) CompleteMultipartUpload(ctx context.Context, key, uploadID string, completedParts []*s3.CompletedPart) error {
	return errNoMultipart
}

func (s *plainUploadStore) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	return errNoMultipart
}

var _ = config.StagingConfig{}
