package vfs

import (
	"testing"

	"github.com/oborges/bluestone/internal/metrics"
)

func TestForProtocolAttributesEveryViewAndHandle(t *testing.T) {
	manager := newTestStagingManager(t)
	fs := newDirtyStagingTestFilesystemWithStore(t, manager, newFakeObjectStore())
	nfsFS := fs.ForProtocol(metrics.ProtocolNFS)

	if got := metrics.ProtocolFrom(fs.requestContext()); got != metrics.ProtocolInternal {
		t.Fatalf("base filesystem protocol = %q, want %q (views must not modify it)", got, metrics.ProtocolInternal)
	}
	if got := metrics.ProtocolFrom(nfsFS.requestContext()); got != metrics.ProtocolNFS {
		t.Fatalf("NFS view protocol = %q, want %q", got, metrics.ProtocolNFS)
	}

	chrooted, err := nfsFS.Chroot("/")
	if err != nil {
		t.Fatalf("Chroot() error = %v", err)
	}
	if got := metrics.ProtocolFrom(chrooted.(*Filesystem).requestContext()); got != metrics.ProtocolNFS {
		t.Fatalf("chrooted view protocol = %q, want %q", got, metrics.ProtocolNFS)
	}

	f, err := nfsFS.Create("file.txt")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer f.Close()
	if got := metrics.ProtocolFrom(f.(*File).requestContext()); got != metrics.ProtocolNFS {
		t.Fatalf("file handle protocol = %q, want %q", got, metrics.ProtocolNFS)
	}
}
