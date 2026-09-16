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
	"testing"
	"time"

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
	data, ok := s.objects[sourceKey]
	if !ok {
		return os.ErrNotExist
	}
	s.objects[destKey] = append([]byte(nil), data...)
	s.metadata[destKey] = s.metadata[sourceKey]
	return nil
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
