package nfs

import "io"

type nfs4CommitArgs struct {
	Offset uint64
	Count  uint32
}

func nfs4OnCommit(c *nfs4Compound, args io.Reader, res io.Writer) nfs4Status {
	var req nfs4CommitArgs
	if status := nfs4Decode(args, &req); status != nfs4OK {
		return status
	}
	current, status := c.requireCurrent()
	if status != nfs4OK {
		return status
	}
	// Without a Committer every WRITE was FILE_SYNC, so there is nothing to
	// commit. The whole file is committed, whatever range the client named.
	if committer, ok := current.fs.(Committer); ok {
		if err := committer.Commit(current.fullPath()); err != nil {
			return nfs4StatusFromErr(err)
		}
	}
	return nfs4Encode(res, c.w.Server.ID)
}
