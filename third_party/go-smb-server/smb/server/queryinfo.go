package server

import (
	"context"
	"encoding/binary"
	"strings"

	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

func (c *request) handleQueryInfo(ctx context.Context, msg []byte, tr *tree) uint32 {
	var req wire.QueryInfoRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle)
	}

	switch req.InfoType {
	case wire.InfoFile:
		fi, err := oh.h.Stat(ctx)
		if err != nil {
			return c.errBody(osErrToStatus(err))
		}
		basic := wire.FileBasicInformation{
			CreationTime:   wire.TimeToFiletime(fi.CreationTime),
			LastAccessTime: wire.TimeToFiletime(fi.LastAccess),
			LastWriteTime:  wire.TimeToFiletime(fi.LastWrite),
			ChangeTime:     wire.TimeToFiletime(fi.ChangeTime),
			FileAttributes: toFileAttributes(fi),
		}
		standard := wire.FileStandardInformation{
			AllocationSize: uint64(fi.Size),
			EndOfFile:      uint64(fi.Size),
			NumberOfLinks:  1,
			DeletePending:  boolToU8(c.srv.fileTable().isDeletePending(oh)),
			Directory:      boolToU8(fi.IsDir),
		}
		var info []byte
		switch req.FileInfoClass {
		case wire.FileBasicInfoClass:
			info = basic.Append(nil)
		case wire.FileStandardInfoClass:
			info = standard.Append(nil)
		case wire.FileInternalInformation:
			info = make([]byte, 8)
			put64LE(info, pathIndexNumber(oh.currentPath()))
		case wire.FileEaInformation:
			info = make([]byte, 4)
		case wire.FilePositionInformation:
			info = make([]byte, 8)
		case wire.FileModeInformation:
			info = make([]byte, 4)
		case wire.FileAlternateNameInformation:
			// No 8.3 aliases are kept, so the name itself is the answer.
			name := wire.UTF16ToBytes(pathBase(oh.currentPath()))
			info = make([]byte, 4+len(name))
			putLE32(info[0:4], uint32(len(name)))
			copy(info[4:], name)
		case wire.FileNameInformation, wire.FileNormalizedNameInformation:
			name := wire.UTF16ToBytes(smbPath(oh.currentPath()))
			info = make([]byte, 4+len(name))
			putLE32(info[0:4], uint32(len(name)))
			copy(info[4:], name)
		case wire.FileAttributeTagInformation:
			info = make([]byte, 8)
			putLE32(info[0:4], toFileAttributes(fi))
		case wire.FileStreamInformation:
			info, overflow, err := streamInformation(ctx, oh.h, fi, req.OutputBufferLength)
			if err != nil {
				return c.errBody(osErrToStatus(err))
			}
			c.out = wire.QueryInfoResponseAppend(c.out, info)
			if overflow {
				// The client asks with a small buffer first and retries
				// with a larger one when told the list did not fit.
				return wire.StatusBufferOverflow
			}
			return wire.StatusSuccess
		case wire.FileAllInformation:
			info = wire.FileAllInformationAppend(nil, basic, standard, pathIndexNumber(oh.currentPath()), smbPath(oh.currentPath()))
		case wire.FileNetworkOpenInformation:
			info = networkOpenInfo(basic, fi.Size)
		default:
			c.log.Debug("unsupported file info class", "class", req.FileInfoClass)
			return c.errBody(wire.StatusInvalidParameter)
		}
		if uint32(len(info)) > req.OutputBufferLength {
			info = info[:req.OutputBufferLength]
		}
		c.out = wire.QueryInfoResponseAppend(c.out, info)
		return wire.StatusSuccess

	case wire.InfoFilesystem:
		space, err := shareSpace(ctx, tr.share.Backend())
		if err != nil {
			return c.errBody(osErrToStatus(err))
		}
		info := filesystemInfo(req.FileInfoClass, space, supportsStreams(tr.share.Backend()))
		if info == nil {
			c.log.Debug("unsupported filesystem info class", "class", req.FileInfoClass)
			return c.errBody(wire.StatusInvalidParameter)
		}
		if uint32(len(info)) > req.OutputBufferLength {
			info = info[:req.OutputBufferLength]
		}
		c.out = wire.QueryInfoResponseAppend(c.out, info)
		return wire.StatusSuccess

	case wire.InfoSecurity:
		fi, err := oh.h.Stat(ctx)
		if err != nil {
			return c.errBody(osErrToStatus(err))
		}
		descriptor := wire.SecurityDescriptor(req.AdditionalInfo, fi.IsDir)
		if uint32(len(descriptor)) > req.OutputBufferLength {
			// The client asks with a small buffer first and retries with
			// the size the server reports (MS-SMB2 3.3.5.20.3), so the
			// error carries that size rather than a truncated descriptor.
			return c.securityBufferTooSmall(uint32(len(descriptor)))
		}
		c.out = wire.QueryInfoResponseAppend(c.out, descriptor)
		return wire.StatusSuccess

	default:
		c.log.Debug("unsupported query-info type", "info_type", req.InfoType, "class", req.FileInfoClass)
		return c.errBody(wire.StatusNotSupported)
	}
}

// securityBufferTooSmall answers a security query whose buffer is too small
// with the size the client should ask for.
func (c *request) securityBufferTooSmall(needed uint32) uint32 {
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], needed)
	resp := wire.ErrorResponse{ErrorData: size[:]}
	c.out = resp.Append(c.out)
	return wire.StatusBufferTooSmall
}

func networkOpenInfo(basic wire.FileBasicInformation, size int64) []byte {
	out := make([]byte, 56)
	putLE64(out[0:8], basic.CreationTime)
	putLE64(out[8:16], basic.LastAccessTime)
	putLE64(out[16:24], basic.LastWriteTime)
	putLE64(out[24:32], basic.ChangeTime)
	putLE64(out[32:40], uint64(size))
	putLE64(out[40:48], uint64(size))
	out[48], out[49], out[50], out[51] = byte(basic.FileAttributes), byte(basic.FileAttributes>>8), byte(basic.FileAttributes>>16), byte(basic.FileAttributes>>24)
	return out
}

// Reported geometry of the share.
const (
	bytesPerSector           = 4096
	sectorsPerAllocationUnit = 1
)

// nominalSpace is reported for a backend that does not say how much room it
// has: a large share with plenty free.
var nominalSpace = vfs.Space{TotalBytes: 1 << 40, AvailableBytes: 1 << 39}

// shareSpace asks the backend how much room the share has, so a client
// checking before a copy sees a share that is filling up rather than a
// fixed number that only turns into STATUS_DISK_FULL partway through.
func shareSpace(ctx context.Context, backend vfs.Backend) (vfs.Space, error) {
	reporter, ok := backend.(vfs.SpaceReporter)
	if !ok {
		return nominalSpace, nil
	}
	space, err := reporter.Space(ctx)
	if err != nil {
		return vfs.Space{}, err
	}
	if space.AvailableBytes > space.TotalBytes {
		// Windows shows used space as the difference, so free space past
		// the total would show a negative used amount.
		space.TotalBytes = space.AvailableBytes
	}
	return space, nil
}

func filesystemInfo(class uint8, space vfs.Space, namedStreams bool) []byte {
	const unitBytes = bytesPerSector * sectorsPerAllocationUnit
	totalUnits := space.TotalBytes / unitBytes
	availableUnits := space.AvailableBytes / unitBytes

	switch class {
	case wire.FileFsAttributeInformation:
		// Case-preserving but not case-sensitive, which is how the share
		// behaves for Windows clients.
		name := wire.UTF16ToBytes("NTFS")
		out := make([]byte, 12+len(name))
		flags := uint32(0x00000002 | 0x00000004) // CASE_PRESERVED_NAMES | UNICODE_ON_DISK
		if namedStreams {
			// FILE_NAMED_STREAMS: macOS stores Finder information and
			// extended attributes as streams when this is set, and writes
			// AppleDouble "._" files beside each file when it is not.
			flags |= 0x00040000
		}
		putLE32(out[0:4], flags)
		putLE32(out[4:8], 255) // MaximumComponentNameLength
		putLE32(out[8:12], uint32(len(name)))
		copy(out[12:], name)
		return out
	case wire.FileFsSizeInformation:
		out := make([]byte, 24)
		put64LE(out[0:8], totalUnits)
		put64LE(out[8:16], availableUnits)
		putLE32(out[16:20], sectorsPerAllocationUnit)
		putLE32(out[20:24], bytesPerSector)
		return out
	case wire.FileFsFullSizeInformation:
		// Caller and actual available are the same: the gateway keeps no
		// per-user quotas that would make them differ.
		out := make([]byte, 32)
		put64LE(out[0:8], totalUnits)
		put64LE(out[8:16], availableUnits)
		put64LE(out[16:24], availableUnits)
		putLE32(out[24:28], sectorsPerAllocationUnit)
		putLE32(out[28:32], bytesPerSector)
		return out
	case wire.FileFsDeviceInformation:
		out := make([]byte, 8)
		putLE32(out[0:4], 0x00000007) // FILE_DEVICE_DISK
		putLE32(out[4:8], 0x00000010) // FILE_REMOTE_DEVICE
		return out
	case wire.FileFsSectorSizeInformation:
		out := make([]byte, 28)
		putLE32(out[0:4], bytesPerSector)
		putLE32(out[4:8], bytesPerSector)
		putLE32(out[8:12], bytesPerSector)
		putLE32(out[12:16], bytesPerSector)
		putLE32(out[16:20], 0)
		putLE32(out[20:24], 0)
		putLE32(out[24:28], 0)
		return out
	case wire.FileFsVolumeInformation:
		// FILE_FS_VOLUME_INFORMATION (MS-FSCC section 2.5.9): creation time,
		// serial number, label length, SupportsObjects, Reserved, then the
		// label at offset 18. Upstream wrote the label at 16 and sized the
		// buffer two bytes short, which Windows rejects.
		label := wire.UTF16ToBytes("Bluestone")
		out := make([]byte, 18+len(label))
		putLE32(out[8:12], 0x424C5545) // VolumeSerialNumber
		putLE32(out[12:16], uint32(len(label)))
		out[16] = 0 // SupportsObjects
		copy(out[18:], label)
		return out
	default:
		return nil
	}
}

// putLE32 writes a little-endian uint32.
func putLE32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// smbPath renders a backend path the way clients expect it in
// FileNameInformation: share-relative and backslash-separated.
func smbPath(path string) string {
	if path == "" {
		return "\\"
	}
	return "\\" + path
}

// pathBase is the last component of a share-relative path.
func pathBase(path string) string {
	if i := strings.LastIndexAny(path, "\\/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// pathIndexNumber derives a stable file index (inode) from a path. The path
// is normalized first: the same file must hash the same whether it was named
// with backslashes in a query or built from a directory listing, or clients
// see the index change and report a stale handle.
func pathIndexNumber(path string) uint64 {
	normalized := strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	hash := uint64(offset64)
	for i := 0; i < len(normalized); i++ {
		hash ^= uint64(normalized[i])
		hash *= prime64
	}
	return hash
}

func (c *request) handleSetInfo(ctx context.Context, msg []byte, tr *tree) uint32 {
	var req wire.SetInfoRequest
	if err := req.Parse(msg); err != nil {
		return c.errBody(wire.StatusInvalidParameter)
	}
	oh, ok := tr.open(req.FileId)
	if !ok {
		return c.errBody(wire.StatusInvalidHandle)
	}

	if req.InfoType == wire.InfoFile {
		switch req.FileInfoClass {
		case wire.FileDispositionInformation:
			if len(req.Buffer) < 1 {
				return c.errBody(wire.StatusInvalidParameter)
			}
			if status := c.setDisposition(ctx, oh, req.Buffer[0] != 0); status != wire.StatusSuccess {
				return c.errBody(status)
			}

		case wire.FileBasicInfoClass:
			var bi wire.FileBasicInformation
			if err := bi.Parse(req.Buffer); err != nil {
				return c.errBody(wire.StatusInvalidParameter)
			}
			if si, ok := oh.h.(vfs.SetInfoer); ok {
				var req vfs.SetInfoRequest
				if bi.CreationTime != 0 {
					t := wire.FiletimeToTime(bi.CreationTime)
					req.CreationTime = &t
				}
				if bi.LastAccessTime != 0 {
					t := wire.FiletimeToTime(bi.LastAccessTime)
					req.LastAccessTime = &t
				}
				if bi.LastWriteTime != 0 {
					t := wire.FiletimeToTime(bi.LastWriteTime)
					req.LastWriteTime = &t
				}
				if bi.ChangeTime != 0 {
					t := wire.FiletimeToTime(bi.ChangeTime)
					req.ChangeTime = &t
				}
				if bi.FileAttributes != 0 {
					req.Attributes = &bi.FileAttributes
				}
				if err := si.SetInfo(ctx, &req); err != nil {
					return c.errBody(osErrToStatus(err))
				}
			}

		case wire.FileAllocationInformation:
			// Setting an allocation size smaller than the file truncates it
			// (MS-FSCC section 2.4.4). Windows relies on this: PowerShell's
			// Set-Content empties a file this way before writing, and
			// ignoring it silently appended to the old contents.
			if len(req.Buffer) < 8 {
				return c.errBody(wire.StatusInvalidParameter)
			}
			allocation := readLE64(req.Buffer[0:8])
			fi, err := oh.h.Stat(ctx)
			if err != nil {
				return c.errBody(osErrToStatus(err))
			}
			if allocation < fi.Size {
				size := allocation
				c.log.Debug("truncate for allocation size", "path", oh.currentPath(), "size", size)
				if si, ok := oh.h.(vfs.SetInfoer); ok {
					if err := si.SetInfo(ctx, &vfs.SetInfoRequest{EndOfFile: &size}); err != nil {
						return c.errBody(osErrToStatus(err))
					}
				}
			}

		case wire.FilePositionInformation, wire.FileModeInformation:
			// The client's own file pointer and caching mode change nothing
			// on the backend.

		case wire.FileEndOfFileInformation:
			if len(req.Buffer) < 8 {
				return c.errBody(wire.StatusInvalidParameter)
			}
			newSize := readLE64(req.Buffer[0:8])
			c.log.Debug("set end of file", "path", oh.currentPath(), "size", newSize)
			if si, ok := oh.h.(vfs.SetInfoer); ok {
				if err := si.SetInfo(ctx, &vfs.SetInfoRequest{EndOfFile: &newSize}); err != nil {
					return c.errBody(osErrToStatus(err))
				}
			}

		case wire.FileRenameInformation:
			if len(req.Buffer) < 20 {
				return c.errBody(wire.StatusInvalidParameter)
			}
			replaceIfExist := req.Buffer[0] != 0
			fnLen := le32(req.Buffer[16:20])
			if int(20+fnLen) > len(req.Buffer) {
				return c.errBody(wire.StatusInvalidParameter)
			}
			newName := wire.UTF16FromBytes(req.Buffer[20 : 20+fnLen])
			rn, ok := oh.h.(vfs.Renamer)
			if !ok || oh.stream {
				// Renaming a named stream within its file is not supported.
				return c.errBody(wire.StatusNotSupported)
			}
			files := c.srv.fileTable()
			if pending, busy := files.checkRename(oh, newName); pending {
				return c.errBody(wire.StatusDeletePending)
			} else if busy {
				// A directory with files open inside it, or a target
				// someone has open: Windows refuses both, and renaming
				// either would leave those handles naming the wrong file.
				return c.errBody(wire.StatusAccessDenied)
			}
			if err := rn.Rename(ctx, newName, replaceIfExist); err != nil {
				return c.errBody(osErrToStatus(err))
			}
			// Every handle to the file now names it where it is, so a
			// delete through one of them removes this file and not
			// whatever takes the old name next.
			oldPath := oh.currentPath()
			oldKey := files.keyOf(oh)
			files.renamed(oh, newName)
			if c.srv.leasesEnabled {
				c.srv.leaseTable().renamed(oldKey, files.keyOf(oh))
			}
			c.srv.selfNotify(tr, vfs.Change{Action: vfs.ChangeRenamed, Path: newName, OldPath: oldPath, IsDir: oh.isDir})

		default:
			c.log.Debug("unsupported set-info class", "class", req.FileInfoClass)
			return c.errBody(wire.StatusNotSupported)
		}
		var changed uint32
		switch req.FileInfoClass {
		case wire.FileBasicInfoClass:
			changed = FileNotifyChangeAttributes | FileNotifyChangeLastWrite | FileNotifyChangeLastAccess | FileNotifyChangeCreation
		case wire.FileAllocationInformation, wire.FileEndOfFileInformation:
			changed = FileNotifyChangeSize | FileNotifyChangeLastWrite
			c.srv.dataChanged(oh)
		}
		if changed != 0 && !oh.stream {
			c.srv.selfNotify(tr, vfs.Change{Action: vfs.ChangeModified, Path: oh.currentPath(), IsDir: oh.isDir, Filter: changed})
		}
		c.out = wire.SetInfoResponseAppend(c.out)
		return wire.StatusSuccess
	}
	if req.InfoType == wire.InfoSecurity {
		// Refused rather than accepted and dropped: the gateway keeps no
		// Windows ACLs, and a client told its change was saved would show
		// permissions that are not enforced anywhere.
		c.log.Debug("refusing to set a security descriptor", "path", oh.currentPath())
		return c.errBody(wire.StatusNotSupported)
	}
	c.log.Debug("unsupported set-info type", "info_type", req.InfoType, "class", req.FileInfoClass)
	return c.errBody(wire.StatusNotSupported)
}

func (c *request) handleFlush(_ context.Context, _ []byte, _ *tree) uint32 {
	c.out = wire.FlushResponseAppend(c.out)
	return wire.StatusSuccess
}

func boolToU8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func put64LE(dst []byte, v uint64) {
	for i := range 8 {
		dst[i] = byte(v >> (8 * i))
	}
}

func putLE64(dst []byte, v uint64) {
	for i := range 8 {
		dst[i] = byte(v >> (8 * i))
	}
}

func readLE64(b []byte) int64 {
	return int64(uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56)
}

// setDisposition sets or clears the delete-pending state of oh's file, as
// FileDispositionInformation asks (MS-FSA section 2.1.5.14.3). Deleting
// needs delete access, and is refused for a read-only file and a directory
// with anything in it; clearing is always allowed.
func (c *request) setDisposition(ctx context.Context, oh *openHandle, pending bool) uint32 {
	files := c.srv.fileTable()
	if !pending {
		files.setDeletePending(oh, false)
		return wire.StatusSuccess
	}
	if oh.access&(accessDelete|accessGenericAll|accessMaximumAllowed) == 0 {
		return wire.StatusAccessDenied
	}
	fi, err := oh.h.Stat(ctx)
	if err != nil {
		return osErrToStatus(err)
	}
	if fi.IsDir {
		nonEmpty, err := dirHasEntries(ctx, oh.h)
		if err != nil {
			return osErrToStatus(err)
		}
		if nonEmpty {
			return wire.StatusDirectoryNotEmpty
		}
	} else if fi.Attributes&attrReadOnly != 0 {
		return wire.StatusCannotDelete
	}
	files.setDeletePending(oh, true)
	return wire.StatusSuccess
}
