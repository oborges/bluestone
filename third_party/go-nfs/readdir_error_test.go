package nfs

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// A listing that fails says what went wrong. Reporting everything as "not a
// directory" sends the client looking at the path instead of the server.
func TestReadDirError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want NFSStatus
	}{
		{"missing", os.ErrNotExist, NFSStatusNoEnt},
		{"refused", os.ErrPermission, NFSStatusAccess},
		{"really not a directory", syscall.ENOTDIR, NFSStatusNotDir},
		{"wrapped not a directory", &fs.PathError{Err: syscall.ENOTDIR}, NFSStatusNotDir},
		{"anything else", errors.New("too many entries to list"), NFSStatusIO},
	} {
		var status *NFSStatusError
		if !errors.As(readDirError(tc.err), &status) {
			t.Fatalf("%s: did not return an NFS status error", tc.name)
		}
		if status.NFSStatus != tc.want {
			t.Errorf("%s: status %v, want %v", tc.name, status.NFSStatus, tc.want)
		}
	}
}
