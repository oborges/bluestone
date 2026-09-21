package smb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/IBM/ibm-cos-sdk-go/service/s3"
	client "github.com/hirochachacha/go-smb2"
	"github.com/oborges/bluestone/internal/cache"
	"github.com/oborges/bluestone/internal/config"
	"github.com/oborges/bluestone/internal/feature"
	"github.com/oborges/bluestone/internal/lock"
	"github.com/oborges/bluestone/internal/logging"
	"github.com/oborges/bluestone/internal/metrics"
	"github.com/oborges/bluestone/internal/posix"
	"github.com/oborges/bluestone/internal/staging"
	"github.com/oborges/bluestone/internal/vfs"
	"github.com/oborges/bluestone/pkg/types"
	smbvfs "github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"go.uber.org/zap"
)

// memStore is an in-memory posix.ObjectStore, safe for concurrent use.
type memStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	metadata map[string]map[string]string
	// copies counts bucket-side copies, so a test can tell one from the
	// gateway reading and writing the bytes itself.
	copies int
	// copyErr, when set, fails every copy.
	copyErr error
	// full refuses writes as a bucket over its hard quota does.
	full bool
}

func (s *memStore) setFull(full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.full = full
}

// BucketFull reports the full state, as the COS client does.
func (s *memStore) BucketFull() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full
}

func (s *memStore) copyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copies
}

func newMemStore() *memStore {
	return &memStore{objects: make(map[string][]byte), metadata: make(map[string]map[string]string)}
}

func (s *memStore) put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
}

func (s *memStore) GetObject(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (s *memStore) GetObjectRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	data, err := s.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end], nil
}

func (s *memStore) GetObjectStream(ctx context.Context, key string) (io.ReadCloser, error) {
	data, err := s.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *memStore) PutObject(_ context.Context, key string, data []byte, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return fmt.Errorf("failed to put object: %w", syscall.ENOSPC)
	}
	s.objects[key] = append([]byte(nil), data...)
	s.metadata[key] = metadata
	return nil
}

func (s *memStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	delete(s.metadata, key)
	return nil
}

func (s *memStore) HeadObject(_ context.Context, key string) (*types.ObjectMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &types.ObjectMetadata{Key: key, Size: int64(len(data)), Metadata: s.metadata[key]}, nil
}

func (s *memStore) ListObjects(_ context.Context, prefix string, maxKeys int) ([]*types.ObjectMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var result []*types.ObjectMetadata
	for _, key := range keys {
		if maxKeys > 0 && len(result) >= maxKeys {
			break
		}
		result = append(result, &types.ObjectMetadata{Key: key, Size: int64(len(s.objects[key]))})
	}
	return result, nil
}

func (s *memStore) CopyObject(_ context.Context, sourceKey, destKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.copies++
	if s.copyErr != nil {
		return s.copyErr
	}
	data, ok := s.objects[sourceKey]
	if !ok {
		return os.ErrNotExist
	}
	s.objects[destKey] = append([]byte(nil), data...)
	s.metadata[destKey] = s.metadata[sourceKey]
	return nil
}

func (s *memStore) CopyObjectWithMetadata(ctx context.Context, sourceKey, destKey string, metadata map[string]string) error {
	if err := s.CopyObject(ctx, sourceKey, destKey); err != nil {
		return err
	}
	return s.UpdateObjectMetadata(ctx, destKey, metadata)
}

func (s *memStore) UpdateObjectMetadata(_ context.Context, key string, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; !ok {
		return os.ErrNotExist
	}
	s.metadata[key] = metadata
	return nil
}

type testGateway struct {
	store      *memStore
	manager    *staging.StagingManager
	server     *Server
	share      *client.Share
	filesystem *vfs.Filesystem
}

// startGateway serves a staging-backed filesystem over SMB on a local port.
// Unless configure clears Users or blocks the client, it also mounts the
// share as alice.
func startGateway(t *testing.T, configure ...func(*ServerOptions)) *testGateway {
	t.Helper()

	manager, err := staging.NewStagingManager(&config.StagingConfig{
		Enabled:          true,
		RootDir:          t.TempDir(),
		SyncInterval:     "30s",
		SyncThresholdMB:  1,
		MaxDirtyAge:      "5m",
		MaxStagingSizeGB: 1,
		MaxDirtyFiles:    100,
		SyncWorkerCount:  1,
		SyncQueueSize:    10,
		MaxSyncRetries:   3,
		RetryBackoffInit: "1s",
		RetryBackoffMax:  "5s",
		CleanAfterSync:   true,
		StaleFileAge:     "24h",
	})
	if err != nil {
		t.Fatalf("NewStagingManager() error = %v", err)
	}
	t.Cleanup(func() { manager.Shutdown() })

	store := newMemStore()
	perf := &config.PerformanceConfig{
		WriteBufferKB:       4096,
		MaxBufferedWriteMB:  config.DefaultMaxBufferedWriteMB,
		MaxDirectoryEntries: config.DefaultMaxDirectoryEntries,
		MaxFullObjectReadMB: 16,
	}
	metadataCache := cache.NewMetadataCache(&config.MetadataCacheConfig{Enabled: true, MaxEntries: 100, TTLSeconds: 60})
	ops := posix.NewOperationsHandler(store, metadataCache, nil, perf)
	filesystem := vfs.NewFilesystem(ops, logging.NewKVLogger(zap.NewNop()), "/", perf, manager, nil,
		&feature.FeatureFlags{UseStagingPath: true}).
		WithWindowsNames().
		ForProtocol(metrics.ProtocolSMB)

	opts := ServerOptions{
		Address:   "127.0.0.1:0",
		ShareName: "share",
		Domain:    "BLUESTONE",
		Users:     []User{{Name: "alice", Password: "secret"}},
	}
	for _, apply := range configure {
		apply(&opts)
	}
	srv, err := NewServer(filesystem, opts)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	g := &testGateway{store: store, manager: manager, server: srv, filesystem: filesystem}
	if len(configure) == 0 {
		g.share, err = g.mount(t, "alice", "secret", "BLUESTONE")
		if err != nil {
			t.Fatalf("mount share: %v", err)
		}
	}
	return g
}

// mount connects a real SMB client and mounts the share.
func (g *testGateway) mount(t *testing.T, user, password, domain string) (*client.Share, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", g.server.Address(), 3*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	dialer := &client.Dialer{Initiator: &client.NTLMInitiator{User: user, Password: password, Domain: domain}}
	session, err := dialer.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	share, err := session.Mount("share")
	if err != nil {
		_ = session.Logoff()
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	t.Cleanup(func() {
		_ = share.Umount()
		_ = session.Logoff()
		conn.Close()
	})
	return share, nil
}

func (g *testGateway) readFile(t *testing.T, name string) string {
	t.Helper()
	f, err := g.share.Open(name)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll(%q) error = %v", name, err)
	}
	return string(data)
}

func TestSMBClientFileLifecycle(t *testing.T) {
	g := startGateway(t)

	if err := g.share.Mkdir("Docs", 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	f, err := g.share.Create(`Docs\Report.txt`)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	payload := "quarterly numbers"
	if _, err := f.Write([]byte(payload)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !g.manager.IsDirty("/Docs/Report.txt") {
		t.Fatal("SMB write must be accepted into staging")
	}

	// Names resolve case-insensitively for SMB clients.
	if got := g.readFile(t, `DOCS\report.TXT`); got != payload {
		t.Fatalf("case-insensitive read = %q, want %q", got, payload)
	}
	info, err := g.share.Stat(`Docs\Report.txt`)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size() != int64(len(payload)) || info.IsDir() {
		t.Fatalf("Stat() = size %d dir %v, want size %d file", info.Size(), info.IsDir(), len(payload))
	}

	entries, err := g.share.ReadDir("Docs")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "Report.txt" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("ReadDir(Docs) = %v, want [Report.txt]", names)
	}

	if err := g.share.Rename(`Docs\Report.txt`, `Docs\Final.txt`); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if got := g.readFile(t, `Docs\Final.txt`); got != payload {
		t.Fatalf("renamed read = %q, want %q", got, payload)
	}
	if _, err := g.share.Stat(`Docs\Report.txt`); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(old name) error = %v, want not-exist", err)
	}

	if err := g.share.Remove(`Docs\Final.txt`); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := g.share.Stat(`Docs\Final.txt`); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(removed) error = %v, want not-exist", err)
	}
}

func TestSMBClientMappedNamesAndReadOnlyOpens(t *testing.T) {
	g := startGateway(t)
	g.store.put("notes:v2.txt", []byte("stored in COS"))

	// A key Windows cannot represent is listed and read under its mapped name.
	mapped := "notesv2.txt"
	entries, err := g.share.ReadDir("")
	if err != nil {
		t.Fatalf("ReadDir(root) error = %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == mapped {
			found = true
		}
	}
	if !found {
		t.Fatalf("root listing lacks the mapped name %q", mapped)
	}
	if got := g.readFile(t, mapped); got != "stored in COS" {
		t.Fatalf("mapped read = %q, want the object content", got)
	}

	// Reading an existing object does not download it into staging.
	if _, ok := g.manager.GetSession("/notes:v2.txt"); ok {
		t.Fatal("a read-only SMB open must not create a staging session")
	}

	// Writing through a mapped name stores the real key.
	f, err := g.share.Create("draft.txt")
	if err != nil {
		t.Fatalf("Create(mapped) error = %v", err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !g.manager.IsDirty("/draft*.txt") {
		t.Fatal("mapped create must stage the key draft*.txt")
	}
}

func TestSMBAuthentication(t *testing.T) {
	g := startGateway(t, func(*ServerOptions) {})

	// Usernames match case-insensitively and any client domain is accepted,
	// as Windows clients send their own workgroup or domain.
	if _, err := g.mount(t, "ALICE", "secret", "WORKGROUP"); err != nil {
		t.Fatalf("mount as ALICE from another domain: %v", err)
	}
	if _, err := g.mount(t, "alice", "wrong", "BLUESTONE"); err == nil {
		t.Fatal("mount with a wrong password succeeded")
	}
	if _, err := g.mount(t, "bob", "secret", ""); err == nil {
		t.Fatal("mount as an unknown user succeeded")
	}
}

func TestSMBAllowedClients(t *testing.T) {
	blocked := startGateway(t, func(opts *ServerOptions) { opts.AllowedClients = []string{"10.0.0.0/8"} })
	if _, err := blocked.mount(t, "alice", "secret", "BLUESTONE"); err == nil {
		t.Fatal("mount from an address outside allowed_clients succeeded")
	}

	allowed := startGateway(t, func(opts *ServerOptions) { opts.AllowedClients = []string{"127.0.0.1"} })
	if _, err := allowed.mount(t, "alice", "secret", "BLUESTONE"); err != nil {
		t.Fatalf("mount from an allowed address: %v", err)
	}
}

func TestSMBServerStopClosesOpenSessions(t *testing.T) {
	g := startGateway(t)
	if _, err := g.share.Stat(""); err != nil {
		t.Fatalf("Stat(root) error = %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- g.server.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return while a client session was open")
	}
}

func TestNewServerRequiresUsers(t *testing.T) {
	if _, err := NewServer(nil, ServerOptions{Address: "127.0.0.1:0", ShareName: "share", Domain: "BLUESTONE"}); err == nil {
		t.Fatal("NewServer() without users succeeded")
	}
}

// A directory larger than one response must be listed completely: the server
// pages through it across several requests.
func TestSMBListsLargeDirectoryCompletely(t *testing.T) {
	g := startGateway(t)

	const count = 120
	want := make(map[string]bool, count)
	for i := range count {
		name := fmt.Sprintf("file-%03d-with-a-reasonably-long-name.txt", i)
		g.store.put("bulk/"+name, []byte("x"))
		want[name] = true
	}

	entries, err := g.share.ReadDir("bulk")
	if err != nil {
		t.Fatalf("ReadDir(bulk) error = %v", err)
	}
	for _, entry := range entries {
		delete(want, entry.Name())
	}
	if len(entries) != count || len(want) != 0 {
		t.Fatalf("ReadDir(bulk) returned %d of %d entries; %d missing", len(entries), count, len(want))
	}
}

// Shrinking a file through SMB must drop the old tail, not leave it behind.
func TestSMBTruncateShrinksFile(t *testing.T) {
	g := startGateway(t)

	f, err := g.share.Create("shrink.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := f.Write([]byte("the original, rather long content")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Truncate(5); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	info, err := g.share.Stat("shrink.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size() != 5 {
		t.Fatalf("size after truncate = %d, want 5", info.Size())
	}
	if got := g.readFile(t, "shrink.txt"); got != "the o" {
		t.Fatalf("content after truncate = %q, want %q", got, "the o")
	}
}

// A modification time a client sets must stick, including on a file that is
// still staged, and a later write must move it again.
func TestSMBSetsModificationTime(t *testing.T) {
	g := startGateway(t)

	f, err := g.share.Create("timestamps.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := f.Write([]byte("first")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	want := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := g.share.Chtimes("timestamps.txt", want, want); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	info, err := g.share.Stat("timestamps.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.ModTime().UTC().Equal(want) {
		t.Fatalf("ModTime() = %v, want %v", info.ModTime().UTC(), want)
	}

	// Writing again moves the modification time off the value that was set.
	again, err := g.share.OpenFile("timestamps.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if _, err := again.WriteAt([]byte("second"), 0); err != nil {
		t.Fatalf("WriteAt() error = %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	info, err = g.share.Stat("timestamps.txt")
	if err != nil {
		t.Fatalf("Stat() after write error = %v", err)
	}
	if info.ModTime().UTC().Equal(want) {
		t.Fatal("writing left the modification time at the value set earlier")
	}
}

// openOptions is a CREATE the way a client sends it, with the access it wants
// and what it lets other opens do.
func openOptions(path string, disposition uint32, desired, share uint32) smbvfs.OpenOptions {
	return smbvfs.OpenOptions{
		Path:          path,
		Disposition:   disposition,
		DesiredAccess: desired,
		ShareAccess:   share,
	}
}

// A client that opens a file without sharing it blocks other opens until it
// closes, which is what stops two editors writing over each other.
func TestSMBShareModesRefuseConflictingOpens(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	backend := NewBackend(g.filesystem, lock.NewShareTable(lock.ShareOptions{}))

	const shareNothing = 0
	const shareAll = 0x1 | 0x2 | 0x4
	exclusive, err := backend.Open(ctx, openOptions("exclusive.txt", smbvfs.DispositionCreate, 0x2, shareNothing))
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	if _, err := backend.Open(ctx, openOptions("exclusive.txt", smbvfs.DispositionOpen, 0x1, shareAll)); !errors.Is(err, smbvfs.ErrSharingViolation) {
		t.Fatalf("second Open() error = %v, want a sharing violation", err)
	}
	// A different file is unaffected.
	other, err := backend.Open(ctx, openOptions("other.txt", smbvfs.DispositionCreate, 0x2, shareNothing))
	if err != nil {
		t.Fatalf("Open(other file) error = %v", err)
	}
	if err := other.Close(ctx); err != nil {
		t.Fatalf("Close(other) error = %v", err)
	}

	if err := exclusive.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := backend.Open(ctx, openOptions("exclusive.txt", smbvfs.DispositionOpen, 0x1, shareAll))
	if err != nil {
		t.Fatalf("Open() after close error = %v", err)
	}
	if err := reopened.Close(ctx); err != nil {
		t.Fatalf("Close(reopened) error = %v", err)
	}
}

// Opens that permit each other coexist, and listing a directory never
// conflicts with a file open inside it.
func TestSMBShareModesAllowCompatibleOpens(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	backend := NewBackend(g.filesystem, lock.NewShareTable(lock.ShareOptions{}))
	g.store.put("shared/report.txt", []byte("content"))

	const shareAll = 0x1 | 0x2 | 0x4
	first, err := backend.Open(ctx, openOptions(`shared\report.txt`, smbvfs.DispositionOpen, 0x1, shareAll))
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	second, err := backend.Open(ctx, openOptions(`shared\report.txt`, smbvfs.DispositionOpen, 0x1, shareAll))
	if err != nil {
		t.Fatalf("second Open() error = %v, want both readers admitted", err)
	}
	directory, err := backend.Open(ctx, smbvfs.OpenOptions{Path: "shared", Disposition: smbvfs.DispositionOpen, CreateDir: true})
	if err != nil {
		t.Fatalf("Open(directory) error = %v", err)
	}

	for _, h := range []smbvfs.Handle{first, second, directory} {
		if err := h.Close(ctx); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
}

// A file that was renamed while open keeps its share state under the new name.
func TestSMBShareModesFollowRenames(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	opens := lock.NewShareTable(lock.ShareOptions{})
	backend := NewBackend(g.filesystem, opens)

	held, err := backend.Open(ctx, openOptions("before.txt", smbvfs.DispositionCreate, 0x2, 0))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	renamer, ok := held.(smbvfs.Renamer)
	if !ok {
		t.Fatal("the handle cannot rename")
	}
	if err := renamer.Rename(ctx, "after.txt", false); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}

	if _, err := backend.Open(ctx, openOptions("after.txt", smbvfs.DispositionOpen, 0x1, 0x1|0x2|0x4)); !errors.Is(err, smbvfs.ErrSharingViolation) {
		t.Fatalf("Open(new name) error = %v, want the open to still conflict", err)
	}
	if err := held.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if opens := opens.Opens("/after.txt"); len(opens) != 0 {
		t.Fatalf("Opens() = %v, want none left", opens)
	}
}

// Many reads and writes in flight on one connection must all come back
// correct: the server handles them at the same time rather than in turn.
func TestSMBConcurrentReadsAndWrites(t *testing.T) {
	g := startGateway(t)

	const readers = 16
	payload := bytes.Repeat([]byte("bluestone"), 4096) // 36 KiB
	g.store.put("concurrent/source.bin", payload)

	var wg sync.WaitGroup
	errs := make(chan error, readers*2)

	for i := range readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := g.share.Open(`concurrent\source.bin`)
			if err != nil {
				errs <- fmt.Errorf("reader %d open: %w", i, err)
				return
			}
			defer f.Close()
			got, err := io.ReadAll(f)
			if err != nil {
				errs <- fmt.Errorf("reader %d read: %w", i, err)
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("reader %d read %d bytes, want %d", i, len(got), len(payload))
			}
		}(i)

		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf(`concurrent\writer-%02d.bin`, i)
			content := bytes.Repeat([]byte{byte('a' + i%26)}, 8192)
			f, err := g.share.Create(name)
			if err != nil {
				errs <- fmt.Errorf("writer %d create: %w", i, err)
				return
			}
			if _, err := f.Write(content); err != nil {
				f.Close()
				errs <- fmt.Errorf("writer %d write: %w", i, err)
				return
			}
			if err := f.Close(); err != nil {
				errs <- fmt.Errorf("writer %d close: %w", i, err)
				return
			}
			if got := g.readFile(t, name); got != string(content) {
				errs <- fmt.Errorf("writer %d wrote %d bytes, read back %d", i, len(content), len(got))
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Copying a whole file whose destination is not staged becomes a copy
// inside the bucket: the bytes never reach the gateway, let alone the
// client.
func TestServerSideCopyUsesBucketCopy(t *testing.T) {
	g := startGateway(t)
	const contents = "the file that gets copied"
	g.store.put("source.txt", []byte(contents))

	before := g.store.copyCount()
	srcHandle := &handle{fs: g.filesystem, path: "/source.txt"}
	dstHandle := &handle{fs: g.filesystem, path: "/copy.txt", opens: g.server.opens}
	n, err := dstHandle.CopyChunk(context.Background(), srcHandle, 0, 0, int64(len(contents)))
	if err != nil {
		t.Fatalf("CopyChunk() error = %v", err)
	}
	if n != int64(len(contents)) {
		t.Fatalf("CopyChunk() = %d bytes, want %d", n, len(contents))
	}
	if copies := g.store.copyCount() - before; copies != 1 {
		t.Fatalf("bucket-side copies = %d, want 1", copies)
	}
	if got := g.readFile(t, "copy.txt"); got != contents {
		t.Fatalf("copy contains %q, want %q", got, contents)
	}
}

// While another open still holds the destination's staged file, it could
// write to it at any moment, so the backend declines and the server moves
// the bytes instead: still gateway-side, never out to the client.
func TestServerSideCopyDeclinesStagedDestination(t *testing.T) {
	g := startGateway(t)
	g.store.put("source.txt", []byte("the file that gets copied"))

	dst, err := g.share.Create("copy.txt")
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	defer dst.Close()

	srcHandle := &handle{fs: g.filesystem, path: "/source.txt"}
	dstHandle := &handle{fs: g.filesystem, path: "/copy.txt", opens: g.server.opens}
	if _, err := dstHandle.CopyChunk(context.Background(), srcHandle, 0, 0, 25); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk() with a staged destination = %v, want ErrUnsupported", err)
	}
}

// A partial range cannot be expressed as a bucket-side copy, so the backend
// declines and the server moves the bytes instead.
func TestServerSideCopyDeclinesPartialRange(t *testing.T) {
	g := startGateway(t)
	g.store.put("source.txt", []byte("0123456789"))

	srcHandle := &handle{fs: g.filesystem, path: "/source.txt"}
	dstHandle := &handle{fs: g.filesystem, path: "/copy.txt", opens: g.server.opens}

	if _, err := dstHandle.CopyChunk(context.Background(), srcHandle, 0, 0, 4); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk() of a partial range = %v, want ErrUnsupported", err)
	}
	if _, err := dstHandle.CopyChunk(context.Background(), srcHandle, 2, 0, 8); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk() from an offset = %v, want ErrUnsupported", err)
	}
}

// While the source is staged, the object in the bucket is not what a reader
// would see, so a bucket-side copy would copy the wrong bytes.
func TestServerSideCopyDeclinesStagedSource(t *testing.T) {
	g := startGateway(t)
	f, err := g.share.Create("staged.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := f.Write([]byte("written through the gateway and not yet synced")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	srcHandle := &handle{fs: g.filesystem, path: "/staged.txt"}
	dstHandle := &handle{fs: g.filesystem, path: "/copy.txt", opens: g.server.opens}
	info, err := g.filesystem.Stat("/staged.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dstHandle.CopyChunk(context.Background(), srcHandle, 0, 0, info.Size()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk() with a staged source = %v, want ErrUnsupported", err)
	}
}

// syncStore lets the staging sync worker upload into a memStore. The worker
// names objects by path, which the COS client turns into keys. The files the
// tests sync are far below the multipart size.
type syncStore struct{ *memStore }

func (s syncStore) PutObjectStream(ctx context.Context, path string, body io.ReadSeeker, metadata map[string]string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return s.PutObject(ctx, strings.TrimPrefix(path, "/"), data, metadata)
}

func (syncStore) CreateMultipartUpload(context.Context, string, map[string]string) (string, error) {
	return "", errors.ErrUnsupported
}

func (syncStore) UploadPart(context.Context, string, string, int64, io.ReadSeeker) (string, error) {
	return "", errors.ErrUnsupported
}

func (syncStore) CompleteMultipartUpload(context.Context, string, string, []*s3.CompletedPart) error {
	return errors.ErrUnsupported
}

func (syncStore) AbortMultipartUpload(context.Context, string, string) error {
	return errors.ErrUnsupported
}

// runSyncWorker runs the staging sync worker until it has uploaded the
// object at marker, which proves it has made a pass over every dirty file.
// Every dirty file is old enough to sync, including staged bytes no session
// holds any more.
func (g *testGateway) runSyncWorker(t *testing.T, marker string) {
	t.Helper()
	worker := staging.NewSyncWorker(g.manager, syncStore{g.store}, &config.StagingConfig{
		Enabled:          true,
		SyncInterval:     "50ms",
		SyncThresholdMB:  1,
		MaxDirtyAge:      "1ms",
		SyncWorkerCount:  1,
		MaxSyncRetries:   3,
		RetryBackoffInit: "10ms",
		RetryBackoffMax:  "50ms",
		CleanAfterSync:   true,
	})
	worker.Start()
	defer worker.Stop()

	// The worker leaves a file alone until it has been idle a few seconds.
	deadline := time.Now().Add(20 * time.Second)
	for g.manager.IsDirty(marker) {
		if time.Now().After(deadline) {
			t.Fatalf("sync worker did not sync %s", marker)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// storedObject returns what the bucket holds for key.
func (g *testGateway) storedObject(t *testing.T, key string) string {
	t.Helper()
	data, err := g.store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("object %s: %v", key, err)
	}
	return string(data)
}

// serverCopy copies a range the way the SMB server does: through the
// backend's CopyChunk, and when that declines, by reading and writing the
// bytes gateway-side.
func serverCopy(t *testing.T, dst, src *handle, offset, length int64) {
	t.Helper()
	ctx := context.Background()
	n, err := dst.CopyChunk(ctx, src, offset, offset, length)
	if err == nil {
		if n != length {
			t.Fatalf("CopyChunk(%d, %d) = %d bytes", offset, length, n)
		}
		return
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk(%d, %d) error = %v", offset, length, err)
	}
	buf := make([]byte, length)
	read, err := src.Read(ctx, offset, buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Read(%d) error = %v", offset, err)
	}
	if _, err := dst.Write(ctx, offset, buf[:read]); err != nil {
		t.Fatalf("Write(%d) error = %v", offset, err)
	}
}

// Creating a file through SMB stages it, empty. Copying into it replaces
// that staged file with a copy inside the bucket, and the staged file is
// gone for good: the sync worker must not upload it over the copy later.
func TestServerSideCopyReplacesUnwrittenDestination(t *testing.T) {
	g := startGateway(t)
	const contents = "the file that gets copied"
	g.store.put("source.txt", []byte(contents))

	for _, name := range []string{"copy.txt", "marker.txt"} {
		f, err := g.share.Create(name)
		if err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close(%s) error = %v", name, err)
		}
	}
	if !g.manager.IsDirty("/copy.txt") {
		t.Fatal("a file created through SMB must be staged")
	}

	before := g.store.copyCount()
	srcHandle := &handle{fs: g.filesystem, path: "/source.txt"}
	dstHandle := &handle{fs: g.filesystem, path: "/copy.txt", opens: g.server.opens}
	n, err := dstHandle.CopyChunk(context.Background(), srcHandle, 0, 0, int64(len(contents)))
	if err != nil {
		t.Fatalf("CopyChunk() error = %v", err)
	}
	if n != int64(len(contents)) {
		t.Fatalf("CopyChunk() = %d bytes, want %d", n, len(contents))
	}
	if copies := g.store.copyCount() - before; copies != 1 {
		t.Fatalf("bucket-side copies = %d, want 1", copies)
	}
	if got := g.readFile(t, "copy.txt"); got != contents {
		t.Fatalf("copy contains %q, want %q", got, contents)
	}
	if g.manager.IsDirty("/copy.txt") {
		t.Fatal("the replaced staged file is still waiting to sync")
	}

	// The marker was created alongside the destination; once the worker
	// has synced it, it has had its chance at the destination too.
	g.runSyncWorker(t, "/marker.txt")
	if _, err := g.store.GetObject(context.Background(), "marker.txt"); err != nil {
		t.Fatalf("the sync worker did not upload the marker: %v", err)
	}
	if got := g.storedObject(t, "copy.txt"); got != contents {
		t.Fatalf("after sync the bucket holds %q, want %q", got, contents)
	}
	if got := g.readFile(t, "copy.txt"); got != contents {
		t.Fatalf("after sync the copy reads %q, want %q", got, contents)
	}
}

// Windows' own sequence: create the destination, set its length to the
// source's, then copy in 1 MiB chunks through the same open. The first chunk
// copies the whole file inside the bucket and the rest have nothing left to
// move.
func TestServerSideCopyWindowsSequence(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	source := bytes.Repeat([]byte("0123456789abcdef"), (5<<20)/16+100) // not a whole number of chunks
	g.store.put("source.bin", source)
	size := int64(len(source))

	backend := NewBackend(g.filesystem, lock.NewShareTable(lock.ShareOptions{}))
	src, err := backend.Open(ctx, openOptions("source.bin", smbvfs.DispositionOpen, accessReadData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(source) error = %v", err)
	}
	defer src.Close(ctx)
	dst, err := backend.Open(ctx, openOptions("copy.bin", smbvfs.DispositionOverwriteIf, accessReadData|accessWriteData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(destination) error = %v", err)
	}
	if err := dst.(*handle).SetInfo(ctx, &smbvfs.SetInfoRequest{EndOfFile: &size}); err != nil {
		t.Fatalf("SetInfo(EndOfFile) error = %v", err)
	}

	before := g.store.copyCount()
	const chunk = 1 << 20
	for offset := int64(0); offset < size; offset += chunk {
		length := min(chunk, size-offset)
		n, err := dst.(*handle).CopyChunk(ctx, src, offset, offset, length)
		if err != nil {
			t.Fatalf("CopyChunk(%d) error = %v", offset, err)
		}
		if n != length {
			t.Fatalf("CopyChunk(%d) = %d bytes, want %d", offset, n, length)
		}
	}
	if copies := g.store.copyCount() - before; copies != 1 {
		t.Fatalf("bucket-side copies = %d, want 1", copies)
	}
	if err := dst.Close(ctx); err != nil {
		t.Fatalf("Close(destination) error = %v", err)
	}
	if g.manager.IsDirty("/copy.bin") {
		t.Fatal("the replaced staged file is still waiting to sync")
	}
	if got := g.readFile(t, "copy.bin"); got != string(source) {
		t.Fatalf("copy differs from the source (%d bytes read, want %d)", len(got), len(source))
	}
}

// A destination someone has written to holds data a bucket-side copy would
// throw away, so the backend declines and the server copies the bytes,
// which leaves the destination correct once the staged file syncs.
func TestServerSideCopyDeclinesWrittenDestination(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	source := bytes.Repeat([]byte("s"), 3<<20)
	g.store.put("source.bin", source)
	size := int64(len(source))

	backend := NewBackend(g.filesystem, lock.NewShareTable(lock.ShareOptions{}))
	src, err := backend.Open(ctx, openOptions("source.bin", smbvfs.DispositionOpen, accessReadData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(source) error = %v", err)
	}
	defer src.Close(ctx)
	dst, err := backend.Open(ctx, openOptions("copy.bin", smbvfs.DispositionOverwriteIf, accessReadData|accessWriteData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(destination) error = %v", err)
	}
	dstHandle := dst.(*handle)
	if _, err := dstHandle.Write(ctx, 0, []byte("written")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := dstHandle.SetInfo(ctx, &smbvfs.SetInfoRequest{EndOfFile: &size}); err != nil {
		t.Fatalf("SetInfo(EndOfFile) error = %v", err)
	}

	before := g.store.copyCount()
	if _, err := dstHandle.CopyChunk(ctx, src.(*handle), 0, 0, 1<<20); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk() into a written destination = %v, want ErrUnsupported", err)
	}
	if copies := g.store.copyCount() - before; copies != 0 {
		t.Fatalf("bucket-side copies = %d, want 0", copies)
	}
	// Declining kept what was written.
	got := make([]byte, 7)
	if _, err := dstHandle.Read(ctx, 0, got); err != nil || string(got) != "written" {
		t.Fatalf("destination after the decline reads %q (%v), want %q", got, err, "written")
	}

	for offset := int64(0); offset < size; offset += 1 << 20 {
		serverCopy(t, dstHandle, src.(*handle), offset, min(1<<20, size-offset))
	}
	if copies := g.store.copyCount() - before; copies != 0 {
		t.Fatalf("bucket-side copies = %d, want 0", copies)
	}
	if err := dst.Close(ctx); err != nil {
		t.Fatalf("Close(destination) error = %v", err)
	}
	if got := g.readFile(t, "copy.bin"); got != string(source) {
		t.Fatal("copy differs from the source")
	}

	g.runSyncWorker(t, "/copy.bin")
	if got := g.storedObject(t, "copy.bin"); got != string(source) {
		t.Fatal("after sync the bucket's copy differs from the source")
	}
}

// Once the first chunk has copied the whole file, a later chunk is only
// taken as done while the destination is untouched. A write in between
// means the server copies the rest itself.
func TestServerSideCopyRechecksLaterChunks(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	source := bytes.Repeat([]byte("s"), 3<<20)
	g.store.put("source.bin", source)
	size := int64(len(source))

	backend := NewBackend(g.filesystem, lock.NewShareTable(lock.ShareOptions{}))
	src, err := backend.Open(ctx, openOptions("source.bin", smbvfs.DispositionOpen, accessReadData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(source) error = %v", err)
	}
	defer src.Close(ctx)
	dst, err := backend.Open(ctx, openOptions("copy.bin", smbvfs.DispositionOverwriteIf, accessReadData|accessWriteData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(destination) error = %v", err)
	}
	defer dst.Close(ctx)
	dstHandle := dst.(*handle)
	if err := dstHandle.SetInfo(ctx, &smbvfs.SetInfoRequest{EndOfFile: &size}); err != nil {
		t.Fatalf("SetInfo(EndOfFile) error = %v", err)
	}

	if _, err := dstHandle.CopyChunk(ctx, src.(*handle), 0, 0, 1<<20); err != nil {
		t.Fatalf("first CopyChunk() error = %v", err)
	}
	if _, err := dstHandle.Write(ctx, 1<<20, []byte("changed")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := dstHandle.CopyChunk(ctx, src.(*handle), 1<<20, 1<<20, 1<<20); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CopyChunk() after a write = %v, want ErrUnsupported", err)
	}
}

// If the bucket-side copy fails after the destination's staged file was
// discarded, the destination is staged again as the client left it.
func TestServerSideCopyRestoresDestinationWhenCopyFails(t *testing.T) {
	g := startGateway(t)
	ctx := context.Background()
	source := bytes.Repeat([]byte("s"), 2<<20)
	g.store.put("source.bin", source)
	size := int64(len(source))

	backend := NewBackend(g.filesystem, lock.NewShareTable(lock.ShareOptions{}))
	src, err := backend.Open(ctx, openOptions("source.bin", smbvfs.DispositionOpen, accessReadData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(source) error = %v", err)
	}
	defer src.Close(ctx)
	dst, err := backend.Open(ctx, openOptions("copy.bin", smbvfs.DispositionOverwriteIf, accessReadData|accessWriteData, shareAccessRead))
	if err != nil {
		t.Fatalf("Open(destination) error = %v", err)
	}
	defer dst.Close(ctx)
	dstHandle := dst.(*handle)
	if err := dstHandle.SetInfo(ctx, &smbvfs.SetInfoRequest{EndOfFile: &size}); err != nil {
		t.Fatalf("SetInfo(EndOfFile) error = %v", err)
	}

	failure := errors.New("bucket unavailable")
	g.store.mu.Lock()
	g.store.copyErr = failure
	g.store.mu.Unlock()
	if _, err := dstHandle.CopyChunk(ctx, src.(*handle), 0, 0, 1<<20); !errors.Is(err, failure) {
		t.Fatalf("CopyChunk() error = %v, want %v", err, failure)
	}
	if !g.manager.IsDirty("/copy.bin") {
		t.Fatal("the destination is no longer staged after the failed copy")
	}
	info, err := g.filesystem.Stat("/copy.bin")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size() != size {
		t.Fatalf("destination is %d bytes after the failed copy, want %d", info.Size(), size)
	}
}

// Windows checks the share's free space before a copy, so the share reports
// the staging area writes land in: its size, and the room left before the
// high watermark, which shrinks as unsynced data piles up.
func TestSMBReportsStagingSpace(t *testing.T) {
	g := startGateway(t)

	before, err := g.share.Statfs("")
	if err != nil {
		t.Fatalf("Statfs() error = %v", err)
	}
	const gib = 1 << 30
	if total := before.TotalBlockCount() * before.BlockSize(); total != gib {
		t.Fatalf("share size = %d bytes, want the 1 GiB staging quota", total)
	}
	// The default high watermark is 80%: past it, writes wait on uploads.
	freeBefore := before.AvailableBlockCount() * before.BlockSize()
	if limit := uint64(gib) * 80 / 100; freeBefore > limit || freeBefore < limit-4096 {
		t.Fatalf("free space = %d bytes, want the room below the high watermark (%d)", freeBefore, limit)
	}

	const written = 8 << 20
	f, err := g.share.Create("big.bin")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := f.Write(make([]byte, written)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	after, err := g.share.Statfs("")
	if err != nil {
		t.Fatalf("Statfs() error = %v", err)
	}
	freeAfter := after.AvailableBlockCount() * after.BlockSize()
	if freeBefore-freeAfter < written {
		t.Fatalf("free space fell by %d bytes after staging %d, want at least that much", freeBefore-freeAfter, written)
	}
}

// A write the staging area has no room for reaches Windows as
// STATUS_DISK_FULL, which it reports as "there is not enough space", rather
// than as a permissions or generic I/O failure.
func TestSMBWritePastStagingQuotaIsDiskFull(t *testing.T) {
	g := startGateway(t)

	f, err := g.share.Create("huge.bin")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = f.Close() }()

	// Writing past the end grows the file to the offset: 2 GiB against a
	// 1 GiB staging quota, without the test having to stage that much.
	_, err = f.WriteAt([]byte("x"), 2<<30)
	var respErr *client.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("WriteAt() error = %v, want an SMB error status", err)
	}
	const statusDiskFull = 0xC000007F
	if respErr.Code != statusDiskFull {
		t.Fatalf("WriteAt() status = %#x, want STATUS_DISK_FULL", respErr.Code)
	}
}

// storedStreams decodes the named streams in an object's metadata.
func (g *testGateway) storedStreams(t *testing.T, key string) map[string][]byte {
	t.Helper()
	head, err := g.store.HeadObject(context.Background(), key)
	if err != nil {
		t.Fatalf("object %s: %v", key, err)
	}
	return posix.DecodePOSIXAttributes(head.Metadata, false).Streams
}

// bucketKeys lists every object in the bucket.
func (g *testGateway) bucketKeys() []string {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	keys := make([]string, 0, len(g.store.objects))
	for key := range g.store.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// A named stream is kept in the file's metadata, never as an object of its
// own, and reads back on a staged file and on one already in the bucket.
func TestSMBNamedStreamsStayInMetadata(t *testing.T) {
	g := startGateway(t)
	if err := g.share.WriteFile("dl.exe", []byte("pretend binary"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	const mark = "[ZoneTransfer]\r\nZoneId=3\r\n"
	if err := g.share.WriteFile("dl.exe:Zone.Identifier", []byte(mark), 0o644); err != nil {
		t.Fatalf("writing the stream: %v", err)
	}
	if got := g.readFile(t, "dl.exe:zone.identifier"); got != mark {
		t.Fatalf("stream on the staged file = %q", got)
	}
	if got := g.readFile(t, "dl.exe"); got != "pretend binary" {
		t.Fatalf("file data = %q after writing a stream", got)
	}

	g.runSyncWorker(t, "/dl.exe")
	if keys := g.bucketKeys(); len(keys) != 1 || keys[0] != "dl.exe" {
		t.Fatalf("bucket holds %v, want only the file itself", keys)
	}
	if got := string(g.storedStreams(t, "dl.exe")["Zone.Identifier"]); got != mark {
		t.Fatalf("stream in the object's metadata = %q", got)
	}

	// Once in the bucket, the stream still reads back, and a new stream on
	// the synced file goes into the object's metadata.
	if got := g.readFile(t, "dl.exe:Zone.Identifier"); got != mark {
		t.Fatalf("stream on the synced file = %q", got)
	}
	if err := g.share.WriteFile("dl.exe:AFP_AfpInfo", []byte("finder info"), 0o644); err != nil {
		t.Fatalf("writing a stream on the synced file: %v", err)
	}
	if streams := g.storedStreams(t, "dl.exe"); string(streams["AFP_AfpInfo"]) != "finder info" || string(streams["Zone.Identifier"]) != mark {
		t.Fatalf("streams in metadata after a second write = %q", streams)
	}
	if keys := g.bucketKeys(); len(keys) != 1 {
		t.Fatalf("bucket holds %v, want only the file itself", keys)
	}
}

// Streams follow the file when it is renamed, and go when it is deleted or
// replaced, as on Windows.
func TestSMBNamedStreamsFollowTheFile(t *testing.T) {
	g := startGateway(t)
	write := func(name, data string) {
		t.Helper()
		if err := g.share.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	hasStream := func(name string) bool {
		t.Helper()
		_, err := g.share.ReadFile(name)
		return err == nil
	}

	write("a.txt", "main")
	write("a.txt:note", "stream")
	if err := g.share.Rename("a.txt", "b.txt"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	if got := g.readFile(t, "b.txt:note"); got != "stream" {
		t.Fatalf("stream after rename = %q", got)
	}

	// Creating the file over again replaces it, streams and all.
	write("b.txt", "replaced")
	if hasStream("b.txt:note") {
		t.Fatal("stream survived the file being replaced")
	}

	write("b.txt:note", "again")
	if err := g.share.Remove("b.txt:note"); err != nil {
		t.Fatalf("removing the stream: %v", err)
	}
	if hasStream("b.txt:note") || g.readFile(t, "b.txt") != "replaced" {
		t.Fatal("removing a stream did not leave the file alone")
	}

	write("b.txt:note", "again")
	if err := g.share.Remove("b.txt"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	write("b.txt", "recreated")
	if hasStream("b.txt:note") {
		t.Fatal("a recreated file inherited the deleted file's stream")
	}
}

// Streams are capped, being kept in object metadata; a write past the cap
// fails as disk full and leaves the streams already there alone.
func TestSMBNamedStreamsAreCapped(t *testing.T) {
	g := startGateway(t)
	if err := g.share.WriteFile("big.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := g.share.WriteFile("big.txt:small", []byte("fits"), 0o644); err != nil {
		t.Fatalf("writing a small stream: %v", err)
	}
	err := g.share.WriteFile("big.txt:huge", bytes.Repeat([]byte("y"), config.DefaultMaxStreamBytes), 0o644)
	var respErr *client.ResponseError
	if !errors.As(err, &respErr) || respErr.Code != 0xC000007F {
		t.Fatalf("writing past the cap = %v, want STATUS_DISK_FULL", err)
	}
	if got := g.readFile(t, "big.txt:small"); got != "fits" {
		t.Fatalf("existing stream after a refused write = %q", got)
	}
}

// Changes made through the filesystem, by NFS or by SMB clients, reach a
// watcher on the share with the names SMB clients use, without anything
// listing a directory.
func TestSMBBackendReportsChanges(t *testing.T) {
	g := startGateway(t)
	backend := NewBackend(g.filesystem, nil)
	var mu sync.Mutex
	var got []smbvfs.Change
	stop := backend.NotifyChanges(func(c smbvfs.Change) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, c)
	})
	defer stop()

	// As an NFS client would: through the filesystem directly.
	if err := g.filesystem.MkdirAll("Docs", 0o755); err != nil {
		t.Fatal(err)
	}
	// And through an SMB client.
	if err := g.share.WriteFile(`Docs\a.txt`, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.share.Rename(`Docs\a.txt`, `Docs\b.txt`); err != nil {
		t.Fatal(err)
	}
	if err := g.share.WriteFile(`Docs\b.txt:note`, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.share.Remove(`Docs\b.txt`); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	has := func(want smbvfs.Change) bool {
		for _, c := range got {
			if c.Action == want.Action && c.Path == want.Path && c.OldPath == want.OldPath &&
				c.IsDir == want.IsDir && c.Filter&want.Filter == want.Filter {
				return true
			}
		}
		return false
	}
	for _, want := range []smbvfs.Change{
		{Action: smbvfs.ChangeAdded, Path: "Docs", IsDir: true},
		{Action: smbvfs.ChangeAdded, Path: `Docs\a.txt`},
		{Action: smbvfs.ChangeModified, Path: `Docs\a.txt`, Filter: notifySize | notifyLastWrite},
		{Action: smbvfs.ChangeRenamed, Path: `Docs\b.txt`, OldPath: `Docs\a.txt`},
		{Action: smbvfs.ChangeModified, Path: `Docs\b.txt`, Filter: notifyStreamWrite},
		{Action: smbvfs.ChangeRemoved, Path: `Docs\b.txt`},
	} {
		if !has(want) {
			t.Errorf("missing %+v in %+v", want, got)
		}
	}
}

// Changes made over NFS are marked external, so the SMB server breaks leases
// for them; changes made through the SMB server are not, since it breaks
// leases for those as it makes them.
func TestSMBBackendMarksExternalChanges(t *testing.T) {
	g := startGateway(t)
	backend := NewBackend(g.filesystem, nil)
	var mu sync.Mutex
	external := map[string]bool{}
	stop := backend.NotifyChanges(func(c smbvfs.Change) {
		if c.Action != smbvfs.ChangeModified {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		external[c.Path] = c.External
	})
	defer stop()

	if err := g.share.WriteFile("from-smb.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	nfs := g.filesystem.ForProtocol(metrics.ProtocolNFS)
	f, err := nfs.OpenFile("from-nfs.txt", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	mu.Lock()
	defer mu.Unlock()
	if ext, ok := external["from-smb.txt"]; !ok || ext {
		t.Errorf("SMB write reported external=%v (seen %v), want a change marked internal", ext, ok)
	}
	if ext, ok := external["from-nfs.txt"]; !ok || !ext {
		t.Errorf("NFS write reported external=%v (seen %v), want a change marked external", ext, ok)
	}
}

// A bucket over its hard quota is reported as full: writes fail with
// STATUS_DISK_FULL when they are made, rather than being staged and failing
// to upload later, a new folder cannot be created, and the share shows no
// free space. Once the bucket has room, writes work again.
func TestSMBFullBucketIsDiskFull(t *testing.T) {
	g := startGateway(t)
	g.manager.SetBucketFullCheck(g.store.BucketFull)
	if err := g.share.WriteFile("before.txt", []byte("fits"), 0o644); err != nil {
		t.Fatal(err)
	}

	g.store.setFull(true)
	const statusDiskFull = 0xC000007F
	isDiskFull := func(err error) bool {
		var respErr *client.ResponseError
		return errors.As(err, &respErr) && respErr.Code == statusDiskFull
	}
	if err := g.share.WriteFile("after.txt", []byte("does not fit"), 0o644); !isDiskFull(err) {
		t.Fatalf("write to a full bucket: %v, want STATUS_DISK_FULL", err)
	}
	if err := g.share.Mkdir("folder", 0o755); !isDiskFull(err) {
		t.Fatalf("mkdir in a full bucket: %v, want STATUS_DISK_FULL", err)
	}
	fs, err := g.share.Statfs("")
	if err != nil {
		t.Fatal(err)
	}
	if free := fs.AvailableBlockCount(); free != 0 {
		t.Fatalf("free space in a full bucket = %d blocks, want none", free)
	}
	// Reads and deletes still work, so users can free space.
	if got := g.readFile(t, "before.txt"); got != "fits" {
		t.Fatalf("read from a full bucket = %q", got)
	}

	g.store.setFull(false)
	if err := g.share.WriteFile("after.txt", []byte("fits now"), 0o644); err != nil {
		t.Fatalf("write once the bucket has room: %v", err)
	}
}
