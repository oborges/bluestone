package vfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/feature"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"go.uber.org/zap"
)

func TestFileTruncateStagedFile(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want string
	}{
		{name: "shrink to zero", size: 0, want: ""},
		{name: "shrink", size: 5, want: "hello"},
		{name: "zero-extend", size: 14, want: "hello world\x00\x00\x00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestStagingManager(t)
			fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())
			writeTestFile(t, fs, "file.txt", "hello world")

			// The open go-nfs uses to apply SETATTR size.
			f, err := fs.OpenFile("file.txt", os.O_WRONLY|os.O_EXCL, 0)
			if err != nil {
				t.Fatalf("OpenFile() error = %v", err)
			}
			if err := f.Truncate(tt.size); err != nil {
				t.Fatalf("Truncate(%d) error = %v", tt.size, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			if got := readTestFile(t, fs, "file.txt"); got != tt.want {
				t.Fatalf("content after Truncate(%d) = %q, want %q", tt.size, got, tt.want)
			}
			info, err := fs.Stat("file.txt")
			if err != nil {
				t.Fatalf("Stat() error = %v", err)
			}
			if info.Size() != tt.size {
				t.Fatalf("Stat() size = %d, want %d", info.Size(), tt.size)
			}

			// The sync worker uploads the staged file at the dirty size.
			if !manager.IsDirty("/file.txt") {
				t.Fatal("truncated file must be dirty so it syncs")
			}
			if got := dirtySize(t, manager, "/file.txt"); got != tt.size {
				t.Fatalf("dirty metadata size = %d, want %d", got, tt.size)
			}
			session, ok := manager.GetSession("/file.txt")
			if !ok {
				t.Fatal("staging session missing after truncate")
			}
			stagingPath, snapshotSize, _, _, _, _, _, _ := session.Snapshot()
			if snapshotSize != tt.size {
				t.Fatalf("session snapshot size = %d, want %d", snapshotSize, tt.size)
			}
			onDisk, err := os.Stat(stagingPath)
			if err != nil {
				t.Fatalf("stat staged file: %v", err)
			}
			if onDisk.Size() != tt.size {
				t.Fatalf("staged file size on disk = %d, want %d", onDisk.Size(), tt.size)
			}
		})
	}
}

func TestFileTruncateWithoutStaging(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want string
	}{
		{name: "shrink to zero", size: 0, want: ""},
		{name: "shrink", size: 5, want: "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeObjectStore()
			store.put("file.txt", []byte("hello world"))
			fs := newObjectOnlyTestFilesystem(t, store)

			f, err := fs.OpenFile("file.txt", os.O_WRONLY|os.O_EXCL, 0)
			if err != nil {
				t.Fatalf("OpenFile() error = %v", err)
			}
			if err := f.Truncate(tt.size); err != nil {
				t.Fatalf("Truncate(%d) error = %v", tt.size, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			if got := string(store.get("file.txt")); got != tt.want {
				t.Fatalf("object after Truncate(%d) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

func TestFileTruncateReadOnlyHandleIsRejected(t *testing.T) {
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())
	writeTestFile(t, fs, "file.txt", "hello world")

	f, err := fs.Open("file.txt")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer f.Close()

	if err := f.Truncate(0); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Truncate() on read-only handle error = %v, want permission error", err)
	}
	if got := readTestFile(t, fs, "file.txt"); got != "hello world" {
		t.Fatalf("content after rejected truncate = %q, want unchanged", got)
	}
}

func TestFileSeekEndUsesLiveStagedSize(t *testing.T) {
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())

	f, err := fs.Create("file.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if pos, err := f.Seek(0, io.SeekEnd); err != nil || pos != 5 {
		t.Fatalf("Seek(end) after own write = %d, %v; want 5", pos, err)
	}

	other, err := fs.OpenFile("file.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile() second handle error = %v", err)
	}
	if _, err := other.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("Seek(end) on second handle error = %v", err)
	}
	if _, err := other.Write([]byte(" world")); err != nil {
		t.Fatalf("Write() on second handle error = %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("Close() second handle error = %v", err)
	}

	if pos, err := f.Seek(0, io.SeekEnd); err != nil || pos != 11 {
		t.Fatalf("Seek(end) after another handle grew the file = %d, %v; want 11", pos, err)
	}
}

func TestFileWriteAtLeavesOffsetUnchanged(t *testing.T) {
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())

	f, err := fs.Create("file.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := f.Write([]byte("abc")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	writerAt, ok := f.(io.WriterAt)
	if !ok {
		t.Fatalf("%T does not implement io.WriterAt", f)
	}
	if _, err := writerAt.WriteAt([]byte("Z"), 0); err != nil {
		t.Fatalf("WriteAt() error = %v", err)
	}
	if _, err := f.Write([]byte("d")); err != nil {
		t.Fatalf("Write() after WriteAt error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if got := readTestFile(t, fs, "file.txt"); got != "Zbcd" {
		t.Fatalf("content = %q, want %q", got, "Zbcd")
	}
}

func TestFileConcurrentPositionalIOOnOneHandle(t *testing.T) {
	const workers, block = 8, 4096
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())

	f, err := fs.Create("file.bin")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	writerAt := f.(io.WriterAt)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			record := bytes.Repeat([]byte{byte('a' + i)}, block)
			if _, err := writerAt.WriteAt(record, int64(i*block)); err != nil {
				t.Errorf("WriteAt(block %d) error = %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, block)
			if _, err := f.ReadAt(buf, int64(i*block)); err != nil && !errors.Is(err, io.EOF) {
				t.Errorf("ReadAt(block %d) error = %v", i, err)
			}
			if _, err := f.Seek(0, io.SeekEnd); err != nil {
				t.Errorf("Seek(end) error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	assertUniformBlocks(t, readTestFile(t, fs, "file.bin"), workers, block)
}

func TestFileConcurrentWritesOnOneHandleDoNotInterleave(t *testing.T) {
	const workers, block = 8, 4096
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())

	f, err := fs.Create("file.bin")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.Write(bytes.Repeat([]byte{byte('a' + i)}, block)); err != nil {
				t.Errorf("Write(record %d) error = %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Record order depends on scheduling; each record must land whole.
	data := readTestFile(t, fs, "file.bin")
	if len(data) != workers*block {
		t.Fatalf("file size = %d, want %d", len(data), workers*block)
	}
	for i := 0; i < workers; i++ {
		chunk := data[i*block : (i+1)*block]
		if chunk != strings.Repeat(chunk[:1], block) {
			t.Fatalf("record %d interleaved with another write", i)
		}
	}
}

func TestFileCloseIsIdempotentAndHandleUnusableAfter(t *testing.T) {
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())
	writeTestFile(t, fs, "file.txt", "hello")

	f, err := fs.OpenFile("file.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	other, err := fs.OpenFile("file.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile() second handle error = %v", err)
	}
	defer other.Close()

	session, ok := manager.GetSession("/file.txt")
	if !ok {
		t.Fatal("staging session missing")
	}
	before := session.GetRefCount()

	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}
	if got := session.GetRefCount(); got != before-1 {
		t.Fatalf("refcount after double Close() = %d, want %d (one release)", got, before-1)
	}

	if _, err := f.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write() after Close error = %v, want os.ErrClosed", err)
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ReadAt() after Close error = %v, want os.ErrClosed", err)
	}
	if _, err := f.Seek(0, io.SeekStart); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Seek() after Close error = %v, want os.ErrClosed", err)
	}
	if err := f.Truncate(0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Truncate() after Close error = %v, want os.ErrClosed", err)
	}
	if got := readTestFile(t, fs, "file.txt"); got != "hello" {
		t.Fatalf("content = %q, want unchanged", got)
	}
}

func newTestStagingManager(t *testing.T) *staging.StagingManager {
	t.Helper()
	manager, err := staging.NewStagingManager(testStagingConfig(t))
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	t.Cleanup(func() { manager.Shutdown() })
	return manager
}

// newObjectOnlyTestFilesystem builds a filesystem with staging disabled, so
// file operations go straight to the object store.
func newObjectOnlyTestFilesystem(t *testing.T, store *fakeObjectStore) *Filesystem {
	t.Helper()
	perfConfig := &config.PerformanceConfig{
		WriteBufferKB:       4096,
		MaxBufferedWriteMB:  config.DefaultMaxBufferedWriteMB,
		MaxDirectoryEntries: config.DefaultMaxDirectoryEntries,
	}
	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{
		Enabled:    true,
		MaxEntries: 10,
		TTLSeconds: 60,
	})
	ops := posix.NewOperationsHandler(store, metadataCache, nil, perfConfig)
	return NewFilesystem(ops, logging.NewKVLogger(zap.NewNop()), "/", perfConfig, nil, nil, &feature.FeatureFlags{UseStagingPath: false})
}

func writeTestFile(t *testing.T, fs *Filesystem, name, content string) {
	t.Helper()
	f, err := fs.Create(name)
	if err != nil {
		t.Fatalf("Create(%s) error = %v", name, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatalf("Write(%s) error = %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%s) error = %v", name, err)
	}
}

func readTestFile(t *testing.T, fs *Filesystem, name string) string {
	t.Helper()
	f, err := fs.Open(name)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll(%s) error = %v", name, err)
	}
	return string(data)
}

func dirtySize(t *testing.T, manager *staging.StagingManager, path string) int64 {
	t.Helper()
	for _, dirty := range manager.GetDirtyFiles() {
		if dirty.Path == path {
			return dirty.Size
		}
	}
	t.Fatalf("no dirty metadata for %s", path)
	return 0
}

// assertUniformBlocks checks that data is workers blocks, block i filled with
// byte 'a'+i.
func assertUniformBlocks(t *testing.T, data string, workers, block int) {
	t.Helper()
	if len(data) != workers*block {
		t.Fatalf("file size = %d, want %d", len(data), workers*block)
	}
	for i := 0; i < workers; i++ {
		want := bytes.Repeat([]byte{byte('a' + i)}, block)
		if data[i*block:(i+1)*block] != string(want) {
			t.Fatalf("block %d does not hold its write", i)
		}
	}
}
