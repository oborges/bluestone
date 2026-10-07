package nfs

import (
	"errors"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/oborges/bluestone/internal/logging"
	gonfs "github.com/willscott/go-nfs"
	"go.uber.org/zap"
)

type committingFS struct {
	billy.Filesystem
	commits []string
	err     error
}

func (c *committingFS) Commit(filename string) error {
	c.commits = append(c.commits, filename)
	return c.err
}

// The NFS server finds the Committer on the filesystem it is handed, which
// is the outermost wrapper: each must pass a commit, and its failure, down.
func TestWrappersForwardCommit(t *testing.T) {
	logger := logging.NewKVLogger(zap.NewNop())
	inner := &committingFS{Filesystem: memfs.New()}
	var fs billy.Filesystem = NewInstrumentedFilesystem(NewCachedFilesystem(inner, logger, 0), logger)

	committer, ok := fs.(gonfs.Committer)
	if !ok {
		t.Fatal("the wrapped filesystem is not a Committer")
	}
	if err := committer.Commit("dir/file"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if len(inner.commits) != 1 || inner.commits[0] != "dir/file" {
		t.Fatalf("commits reaching the filesystem = %v, want dir/file", inner.commits)
	}

	inner.err = errors.New("disk gone")
	if err := committer.Commit("dir/file"); !errors.Is(err, inner.err) {
		t.Fatalf("Commit() error = %v, want the filesystem's", err)
	}
}
