package nfs

import (
	"net"
	"testing"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// mountV3As serves fs over NFSv3 with permissions enforced, and mounts it
// with AUTH_SYS credentials for uid and gid.
func mountV3As(t *testing.T, fs *ownedFS, uid, gid uint32) *nfsc.Target {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	srv := &Server{Handler: newNFSv4TestHandler(fs), Permissions: &Permissions{}}
	go func() { _ = srv.Serve(listener) }()

	c, err := rpc.DialTCP("tcp", listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var mounter nfsc.Mount
	mounter.Client = c
	target, err := mounter.Mount("/", rpc.NewAuthUnix("client", uid, gid).Auth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounter.Unmount() })
	return target
}

// The same rules hold over NFSv3, through a real client.
func TestNFSv3Permissions(t *testing.T) {
	fs := permFS(t)
	bob := mountV3As(t, fs, 1001, 1001)

	if _, err := bob.Create("/closed/x", 0o644); err == nil {
		t.Error("bob created a file in a directory he may not write")
	}
	if _, err := bob.Create("/shared/x", 0o644); err != nil {
		t.Fatalf("create in a world-writable directory: %v", err)
	}
	if owner := fs.owners["/shared/x"]; owner != [2]uint32{1001, 1001} {
		t.Errorf("new file owned by %v, want its creator 1001:1001", owner)
	}
	if _, err := bob.Mkdir("/shared/d", 0o755); err != nil {
		t.Fatalf("mkdir in a world-writable directory: %v", err)
	}
	if owner := fs.owners["/shared/d"]; owner != [2]uint32{1001, 1001} {
		t.Errorf("new directory owned by %v, want its creator 1001:1001", owner)
	}

	f, err := bob.OpenFile("/f", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Error("bob wrote another user's 0644 file")
	}
	_ = f.Close()

	mode := uint32(0o600)
	if err := bob.Setattr("/f", nfsc.Sattr3{Mode: nfsc.SetMode{SetIt: true, Mode: mode}}); err == nil {
		t.Error("bob changed the mode of another user's file")
	}
	if err := bob.Remove("/f"); err == nil {
		t.Error("bob removed a file from root's 0755 directory")
	}

	// ACCESS shares accessMask with NFSv4, tested there: the client library
	// here reports every ACCESS answer as 0.

	root := mountV3As(t, fs, 0, 0)
	f, err = root.OpenFile("/secret", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("root")); err != nil {
		t.Errorf("root writing any file: %v", err)
	}
	_ = f.Close()
}
