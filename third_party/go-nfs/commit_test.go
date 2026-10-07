package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
	"github.com/willscott/go-nfs/helpers/memfs"
)

// committingFS is a memfs that holds writes back until Commit, as a
// filesystem staging them on local disk does. It records what was committed.
type committingFS struct {
	billy.Filesystem
	commits []string
	err     error
}

func (c *committingFS) Commit(filename string) error {
	c.commits = append(c.commits, filename)
	return c.err
}

func newCommittingFS(t *testing.T) *committingFS {
	t.Helper()
	fs := &committingFS{Filesystem: memfs.New()}
	f, err := fs.Create("/f")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return fs
}

// nfs4Write sends one WRITE of the file f with the given stability and
// returns its status and result.
func nfs4Write(t *testing.T, fs billy.Filesystem, stable writeStability) (nfs4Status, nfs4WriteRes) {
	t.Helper()
	write := nfs4TestOp{nfs4OpWrite, nfs4WriteArgs{Stable: uint32(stable), Data: []byte("x")}}
	_, resp := runNFSv4As(t, fs, nil, authSys(0, 0), append(putFile("f"), write)...)
	nfs4ExpectOp(t, resp, nfs4OpPutRootFH, nfs4OK, nil)
	nfs4ExpectOp(t, resp, nfs4OpLookup, nfs4OK, nil)
	var header nfs4ResultHeader
	if err := xdr.Read(resp, &header); err != nil || header.Op != nfs4OpWrite {
		t.Fatalf("result = %+v, %v; want a WRITE result", header, err)
	}
	var res nfs4WriteRes
	if header.Status == nfs4OK {
		if err := xdr.Read(resp, &res); err != nil {
			t.Fatalf("failed to read WRITE result: %v", err)
		}
	}
	return header.Status, res
}

// A write is reported only as durable as it is: UNSTABLE is left for COMMIT,
// and anything stronger is committed before the client hears of it.
func TestNFSv4WriteStability(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stable      writeStability
		want        writeStability
		wantCommits int
	}{
		{"unstable", unstable, unstable, 0},
		{"data sync", dataSync, fileSync, 1},
		{"file sync", fileSync, fileSync, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newCommittingFS(t)
			status, res := nfs4Write(t, fs, tc.stable)
			if status != nfs4OK {
				t.Fatalf("WRITE status = %d", status)
			}
			if res.Committed != uint32(tc.want) {
				t.Errorf("committed = %d, want %d", res.Committed, tc.want)
			}
			if len(fs.commits) != tc.wantCommits {
				t.Errorf("commits = %v, want %d", fs.commits, tc.wantCommits)
			}
		})
	}
}

// A filesystem with nothing to commit has every write on stable storage.
func TestNFSv4WriteWithoutCommitterIsFileSync(t *testing.T) {
	fs := newCommittingFS(t).Filesystem
	status, res := nfs4Write(t, fs, unstable)
	if status != nfs4OK || res.Committed != uint32(fileSync) {
		t.Fatalf("WRITE = status %d committed %d, want OK and FILE_SYNC", status, res.Committed)
	}
}

// A stable write that cannot be committed fails: answering it would tell the
// client the data is safe.
func TestNFSv4WriteFailsWhenCommitFails(t *testing.T) {
	fs := newCommittingFS(t)
	fs.err = errors.New("disk gone")
	if status, _ := nfs4Write(t, fs, fileSync); status != nfs4ErrIO {
		t.Fatalf("WRITE status = %d, want IO", status)
	}
	if status, _ := nfs4Write(t, fs, unstable); status != nfs4OK {
		t.Fatalf("UNSTABLE WRITE status = %d, want OK: nothing was promised", status)
	}
}

func TestNFSv4CommitCommitsTheFile(t *testing.T) {
	fs := newCommittingFS(t)
	commit := nfs4TestOp{nfs4OpCommit, nfs4CommitArgs{}}
	status, resp := runNFSv4As(t, fs, nil, authSys(0, 0), append(putFile("f"), commit)...)
	if status != nfs4OK {
		t.Fatalf("COMMIT compound status = %d", status)
	}
	nfs4ExpectOp(t, resp, nfs4OpPutRootFH, nfs4OK, nil)
	nfs4ExpectOp(t, resp, nfs4OpLookup, nfs4OK, nil)
	nfs4ExpectOp(t, resp, nfs4OpCommit, nfs4OK, nil)
	if len(fs.commits) != 1 || fs.commits[0] != fs.Join("f") {
		t.Fatalf("commits = %v, want the file once", fs.commits)
	}

	fs.err = errors.New("disk gone")
	if status, _ := runNFSv4As(t, fs, nil, authSys(0, 0), append(putFile("f"), commit)...); status != nfs4ErrIO {
		t.Fatalf("COMMIT that failed: status = %d, want IO", status)
	}
}

// nfs3Call runs one NFSv3 procedure against fs and returns the reply after
// the RPC header, or the error the procedure failed with.
func nfs3Call(t *testing.T, fs billy.Filesystem, proc HandleFunc, args ...interface{}) ([]byte, error) {
	t.Helper()
	handler := newNFSv4TestHandler(fs)
	var body bytes.Buffer
	if err := xdr.Write(&body, handler.ToHandle(fs, []string{"f"})); err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if err := xdr.Write(&body, arg); err != nil {
			t.Fatal(err)
		}
	}
	w := &response{
		conn:     &conn{Server: &Server{Handler: handler}},
		req:      &request{xid: 1, Body: bytes.NewReader(body.Bytes())},
		errorFmt: basicErrorFormatter,
		writer:   bytes.NewBuffer(nil),
	}
	if err := proc(context.Background(), w, handler); err != nil {
		return nil, err
	}
	return w.writer.Bytes(), nil
}

// nfs3Write sends one NFSv3 WRITE of the file f and returns the stability
// the server reported for it.
func nfs3Write(t *testing.T, fs billy.Filesystem, how writeStability) (writeStability, error) {
	t.Helper()
	data := []byte("x")
	reply, err := nfs3Call(t, fs, onWrite, uint64(0), uint32(len(data)), uint32(how), data)
	if err != nil {
		return 0, err
	}
	// The reply ends with the count, the stability and the 8-byte verifier.
	return writeStability(binary.BigEndian.Uint32(reply[len(reply)-12:])), nil
}

func TestNFSv3WriteStability(t *testing.T) {
	for _, tc := range []struct {
		name        string
		how         writeStability
		want        writeStability
		wantCommits int
	}{
		{"unstable", unstable, unstable, 0},
		{"data sync", dataSync, fileSync, 1},
		{"file sync", fileSync, fileSync, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newCommittingFS(t)
			committed, err := nfs3Write(t, fs, tc.how)
			if err != nil {
				t.Fatalf("WRITE: %v", err)
			}
			if committed != tc.want {
				t.Errorf("committed = %d, want %d", committed, tc.want)
			}
			if len(fs.commits) != tc.wantCommits {
				t.Errorf("commits = %v, want %d", fs.commits, tc.wantCommits)
			}
		})
	}

	committed, err := nfs3Write(t, newCommittingFS(t).Filesystem, unstable)
	if err != nil || committed != fileSync {
		t.Errorf("without a Committer: committed = %d, %v; want FILE_SYNC", committed, err)
	}
}

func TestNFSv3WriteFailsWhenCommitFails(t *testing.T) {
	fs := newCommittingFS(t)
	fs.err = errors.New("disk gone")
	_, err := nfs3Write(t, fs, fileSync)
	var status *NFSStatusError
	if !errors.As(err, &status) || status.NFSStatus != NFSStatusIO {
		t.Fatalf("WRITE error = %v, want NFS3ERR_IO", err)
	}
}

func TestNFSv3CommitCommitsTheFile(t *testing.T) {
	fs := newCommittingFS(t)
	if _, err := nfs3Call(t, fs, onCommit, uint64(0), uint32(0)); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if len(fs.commits) != 1 || fs.commits[0] != fs.Join("f") {
		t.Fatalf("commits = %v, want the file once", fs.commits)
	}

	fs.err = errors.New("disk gone")
	_, err := nfs3Call(t, fs, onCommit, uint64(0), uint32(0))
	var status *NFSStatusError
	if !errors.As(err, &status) || status.NFSStatus != NFSStatusIO {
		t.Fatalf("COMMIT that failed: error = %v, want NFS3ERR_IO", err)
	}
}
