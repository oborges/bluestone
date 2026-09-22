package nfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/go-git/go-billy/v5"
	nfsc "github.com/willscott/go-nfs-client/nfs"
)

// failRemoveFS fails every Remove and Rename with err.
type failRemoveFS struct {
	billy.Filesystem
	err error
}

func (f *failRemoveFS) Remove(name string) error {
	return &os.PathError{Op: "remove", Path: name, Err: f.err}
}

func (f *failRemoveFS) Rename(from, _ string) error {
	return &os.PathError{Op: "rename", Path: from, Err: f.err}
}

var (
	errNotEmpty = fmt.Errorf("directory not empty: %w", syscall.ENOTEMPTY)
	errBusy     = fmt.Errorf("data is syncing: %w", syscall.EBUSY)
)

func TestMapErrToNFS4Status(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want nfs4Status
	}{
		// ENOTEMPTY also reads as os.ErrExist, which must not win.
		{&os.PathError{Op: "remove", Path: "d", Err: errNotEmpty}, nfs4ErrNotEmpty},
		{&os.PathError{Op: "remove", Path: "d", Err: errBusy}, nfs4ErrDelay},
		{&os.PathError{Op: "mkdir", Path: "d", Err: os.ErrExist}, nfs4ErrExist},
		{&NFSStatusError{NFSStatusJukebox, nil}, nfs4ErrDelay},
		{&NFSStatusError{NFSStatusNotEmpty, nil}, nfs4ErrNotEmpty},
		{errors.New("mystery"), nfs4ErrIO},
	} {
		if got := mapErrToNFS4Status(tc.err); got != tc.want {
			t.Errorf("mapErrToNFS4Status(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestNFSv4RemoveReportsNotEmptyAndDelay(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want nfs4Status
	}{
		{errNotEmpty, nfs4ErrNotEmpty},
		{errBusy, nfs4ErrDelay},
	} {
		fs := newOwnedFS(t)
		fs.Filesystem = &failRemoveFS{Filesystem: fs.Filesystem, err: tc.err}
		resp := runNFSv4As(t, fs, nil, authSys(0, 0), 2, func(req *nfs4Writer) {
			req.writeUint32(uint32(opPutRootFH))
			req.writeUint32(uint32(opRemove))
			req.writeOpaque([]byte("f"))
		})
		assertOpStatus(t, resp, opPutRootFH)
		if got := opStatus(t, resp, opRemove); got != tc.want {
			t.Errorf("REMOVE failing with %v = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// NFSv3 already reported EBUSY as JUKEBOX, which this client library cannot
// decode, so only NOTEMPTY is checked here.
func TestNFSv3RemoveReportsNotEmpty(t *testing.T) {
	fs := newOwnedFS(t)
	if err := fs.MkdirAll("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	fs.Filesystem = &failRemoveFS{Filesystem: fs.Filesystem, err: errNotEmpty}
	root := mountV3As(t, fs, 0, 0)

	for name, call := range map[string]func() error{
		"RMDIR":  func() error { return root.RmDir("/d") },
		"REMOVE": func() error { return root.Remove("/f") },
		"RENAME": func() error { return root.Rename("/d", "/e") },
	} {
		if err := call(); !nfsc.IsNotEmptyError(err) {
			t.Errorf("%s of a non-empty directory: got %v, want NFS3ERR_NOTEMPTY", name, err)
		}
	}
}
