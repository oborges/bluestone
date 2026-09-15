package posix

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/oborges/bluestone/pkg/types"
)

// AttributeKey constants for COS metadata
const (
	MetaKeyMode  = "x-amz-meta-mode"
	MetaKeyUID   = "x-amz-meta-uid"
	MetaKeyGID   = "x-amz-meta-gid"
	MetaKeyAtime = "x-amz-meta-atime"
	MetaKeyMtime = "x-amz-meta-mtime"
	MetaKeyCtime = "x-amz-meta-ctime"
)

// Default POSIX attributes
const (
	DefaultFileMode os.FileMode = 0644
	DefaultDirMode  os.FileMode = 0755
	DefaultUID      int         = 1000
	DefaultGID      int         = 1000
)

// EncodePOSIXAttributes encodes POSIX attributes to COS metadata
func EncodePOSIXAttributes(attrs *types.POSIXAttributes) map[string]string {
	if attrs == nil {
		attrs = DefaultAttributes(false)
	}

	metadata := make(map[string]string)
	metadata[MetaKeyMode] = fmt.Sprintf("%o", attrs.Mode)
	metadata[MetaKeyUID] = strconv.Itoa(attrs.UID)
	metadata[MetaKeyGID] = strconv.Itoa(attrs.GID)
	metadata[MetaKeyAtime] = attrs.Atime.Format(time.RFC3339)
	metadata[MetaKeyMtime] = attrs.Mtime.Format(time.RFC3339)
	metadata[MetaKeyCtime] = attrs.Ctime.Format(time.RFC3339)

	return metadata
}

// DecodePOSIXAttributes decodes POSIX attributes from COS metadata
func DecodePOSIXAttributes(metadata map[string]string, isDir bool) *types.POSIXAttributes {
	attrs := DefaultAttributes(isDir)

	// Parse mode
	if modeStr, ok := metadata[MetaKeyMode]; ok {
		if mode, err := strconv.ParseUint(modeStr, 8, 32); err == nil {
			attrs.Mode = os.FileMode(mode)
		}
	}

	// Parse UID
	if uidStr, ok := metadata[MetaKeyUID]; ok {
		if uid, err := strconv.Atoi(uidStr); err == nil {
			attrs.UID = uid
		}
	}

	// Parse GID
	if gidStr, ok := metadata[MetaKeyGID]; ok {
		if gid, err := strconv.Atoi(gidStr); err == nil {
			attrs.GID = gid
		}
	}

	// Parse timestamps
	if atimeStr, ok := metadata[MetaKeyAtime]; ok {
		if atime, err := time.Parse(time.RFC3339, atimeStr); err == nil {
			attrs.Atime = atime
		}
	}

	if mtimeStr, ok := metadata[MetaKeyMtime]; ok {
		if mtime, err := time.Parse(time.RFC3339, mtimeStr); err == nil {
			attrs.Mtime = mtime
		}
	}

	if ctimeStr, ok := metadata[MetaKeyCtime]; ok {
		if ctime, err := time.Parse(time.RFC3339, ctimeStr); err == nil {
			attrs.Ctime = ctime
		}
	}

	return attrs
}

var defaultStartupTime = time.Now()

// DefaultAttributes returns default POSIX attributes
func DefaultAttributes(isDir bool) *types.POSIXAttributes {
	mode := DefaultFileMode
	if isDir {
		mode = DefaultDirMode | os.ModeDir
	}

	return &types.POSIXAttributes{
		Mode:  mode,
		UID:   DefaultUID,
		GID:   DefaultGID,
		Atime: defaultStartupTime,
		Mtime: defaultStartupTime,
		Ctime: defaultStartupTime,
	}
}

// UpdateAttributes updates specific attributes
func UpdateAttributes(attrs *types.POSIXAttributes, updates *types.POSIXAttributes) *types.POSIXAttributes {
	if attrs == nil {
		attrs = DefaultAttributes(false)
	}

	if updates == nil {
		return attrs
	}

	// Create a copy
	result := &types.POSIXAttributes{
		Mode:  attrs.Mode,
		UID:   attrs.UID,
		GID:   attrs.GID,
		Atime: attrs.Atime,
		Mtime: attrs.Mtime,
		Ctime: attrs.Ctime,
	}

	// Apply updates
	if updates.Mode != 0 {
		result.Mode = updates.Mode
		result.Ctime = time.Now()
	}

	if updates.UID != 0 {
		result.UID = updates.UID
		result.Ctime = time.Now()
	}

	if updates.GID != 0 {
		result.GID = updates.GID
		result.Ctime = time.Now()
	}

	if !updates.Atime.IsZero() {
		result.Atime = updates.Atime
	}

	if !updates.Mtime.IsZero() {
		result.Mtime = updates.Mtime
	}

	return result
}

// ValidateMode validates a file mode
func ValidateMode(mode os.FileMode) error {
	// Check if mode is within valid range
	if mode > 0777 {
		return fmt.Errorf("invalid mode: %o", mode)
	}
	return nil
}

// ValidateUID validates a user ID
func ValidateUID(uid int) error {
	if uid < 0 {
		return fmt.Errorf("invalid UID: %d", uid)
	}
	return nil
}

// ValidateGID validates a group ID
func ValidateGID(gid int) error {
	if gid < 0 {
		return fmt.Errorf("invalid GID: %d", gid)
	}
	return nil
}

// Made with Bob
