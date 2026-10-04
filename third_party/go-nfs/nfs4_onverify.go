package nfs

import (
	"bytes"
	"io"
)

type nfs4VerifyArgs struct {
	Attrs nfs4FAttr
}

// nfs4OnVerify lets the compound go on only if the current file's attributes
// are the ones given (RFC 7530 section 16.35).
func nfs4OnVerify(c *nfs4Compound, args io.Reader, _ io.Writer) nfs4Status {
	same, status := nfs4AttrsMatch(c, args)
	if status != nfs4OK {
		return status
	}
	if !same {
		return nfs4ErrNotSame
	}
	return nfs4OK
}

// nfs4OnNVerify lets the compound go on only if the current file's
// attributes differ from the ones given (RFC 7530 section 16.15). A client
// puts it before a GETATTR or READDIR it wants only if the file has changed:
// the AIX client lists directories this way, and retried without end when
// the operation was refused.
func nfs4OnNVerify(c *nfs4Compound, args io.Reader, _ io.Writer) nfs4Status {
	same, status := nfs4AttrsMatch(c, args)
	if status != nfs4OK {
		return status
	}
	if same {
		return nfs4ErrSame
	}
	return nfs4OK
}

// nfs4AttrsMatch reports whether the attributes in a VERIFY or NVERIFY are
// those of the current file. Attributes are compared as encoded: each has
// one encoding.
func nfs4AttrsMatch(c *nfs4Compound, args io.Reader) (bool, nfs4Status) {
	var req nfs4VerifyArgs
	if status := nfs4Decode(args, &req); status != nfs4OK {
		return false, status
	}
	current, status := c.requireCurrent()
	if status != nfs4OK {
		return false, status
	}
	info, err := current.fs.Lstat(current.fullPath())
	if err != nil {
		return false, nfs4StatusFromErr(err)
	}
	have, status := nfs4BuildAttrs(c.ctx, c.handler, current, info, req.Attrs.Mask)
	if status != nfs4OK {
		return false, status
	}
	asked := req.Attrs.Mask.attrs()
	if len(have.Mask.attrs()) != len(asked) {
		// An attribute this server does not keep cannot be compared.
		return false, nfs4ErrAttrNotSupp
	}
	return bytes.Equal(have.Vals, req.Attrs.Vals), nfs4OK
}
