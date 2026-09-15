package posix

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oborges/bluestone/pkg/types"
)

// Metadata keys for POSIX attributes in COS user metadata. The SDK adds the
// x-amz-meta- header prefix itself, so these are bare names.
const (
	MetaKeyMode  = "mode"
	MetaKeyUID   = "uid"
	MetaKeyGID   = "gid"
	MetaKeyAtime = "atime"
	MetaKeyMtime = "mtime"
	MetaKeyCtime = "ctime"
	// MetaKeyBtime stores the creation time with nanosecond precision.
	MetaKeyBtime = "btime"
	// MetaKeyWindowsAttributes stores Windows attribute flags as a decimal.
	MetaKeyWindowsAttributes = "windows-attributes"
)

// Windows file attribute flags kept in WindowsAttributes. Only flags that
// describe settable state are stored; directory-ness comes from the key.
const (
	WindowsAttributeReadOnly uint32 = 0x1
	WindowsAttributeHidden   uint32 = 0x2
	WindowsAttributeSystem   uint32 = 0x4
	WindowsAttributeArchive  uint32 = 0x20

	// WindowsAttributesStored masks the flags that are stored.
	WindowsAttributesStored = WindowsAttributeReadOnly | WindowsAttributeHidden |
		WindowsAttributeSystem | WindowsAttributeArchive
)

// legacyMetaKeyPrefix marks keys written before the SDK's own prefix was
// accounted for: "x-amz-meta-mode" was stored as the user key itself, so the
// object carries header x-amz-meta-x-amz-meta-mode. Such keys still decode.
const legacyMetaKeyPrefix = "x-amz-meta-"

var posixMetaKeys = []string{
	MetaKeyMode, MetaKeyUID, MetaKeyGID, MetaKeyAtime, MetaKeyMtime, MetaKeyCtime,
	MetaKeyBtime, MetaKeyWindowsAttributes,
}

// Default POSIX attributes
const (
	DefaultFileMode os.FileMode = 0644
	DefaultDirMode  os.FileMode = 0755
	DefaultUID      int         = 1000
	DefaultGID      int         = 1000
)

// EncodePOSIXAttributes encodes POSIX attributes to COS metadata. Unset
// (zero) timestamps are omitted.
func EncodePOSIXAttributes(attrs *types.POSIXAttributes) map[string]string {
	if attrs == nil {
		attrs = DefaultAttributes(false)
	}

	metadata := map[string]string{
		MetaKeyMode: fmt.Sprintf("%o", attrs.Mode),
		MetaKeyUID:  strconv.Itoa(attrs.UID),
		MetaKeyGID:  strconv.Itoa(attrs.GID),
	}
	for key, t := range map[string]time.Time{
		MetaKeyAtime: attrs.Atime,
		MetaKeyMtime: attrs.Mtime,
		MetaKeyCtime: attrs.Ctime,
	} {
		if !t.IsZero() {
			metadata[key] = t.Format(time.RFC3339)
		}
	}
	if !attrs.Btime.IsZero() {
		metadata[MetaKeyBtime] = attrs.Btime.UTC().Format(time.RFC3339Nano)
	}
	if flags := attrs.WindowsAttributes & WindowsAttributesStored; flags != 0 {
		metadata[MetaKeyWindowsAttributes] = strconv.FormatUint(uint64(flags), 10)
	}
	return metadata
}

// DecodePOSIXAttributes decodes POSIX attributes from COS metadata. Keys match
// case-insensitively, and attributes stored under legacy keys still decode.
func DecodePOSIXAttributes(metadata map[string]string, isDir bool) *types.POSIXAttributes {
	attrs := DefaultAttributes(isDir)

	if value, ok := metaValue(metadata, MetaKeyMode); ok {
		if mode, err := strconv.ParseUint(value, 8, 32); err == nil {
			attrs.Mode = os.FileMode(mode)
		}
	}
	if value, ok := metaValue(metadata, MetaKeyUID); ok {
		if uid, err := strconv.Atoi(value); err == nil {
			attrs.UID = uid
		}
	}
	if value, ok := metaValue(metadata, MetaKeyGID); ok {
		if gid, err := strconv.Atoi(value); err == nil {
			attrs.GID = gid
		}
	}
	for key, target := range map[string]*time.Time{
		MetaKeyAtime: &attrs.Atime,
		MetaKeyMtime: &attrs.Mtime,
		MetaKeyCtime: &attrs.Ctime,
	} {
		if value, ok := metaValue(metadata, key); ok {
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				*target = t
			}
		}
	}
	if value, ok := metaValue(metadata, MetaKeyBtime); ok {
		if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
			attrs.Btime = t
		}
	}
	if value, ok := metaValue(metadata, MetaKeyWindowsAttributes); ok {
		if flags, err := strconv.ParseUint(value, 10, 32); err == nil {
			attrs.WindowsAttributes = uint32(flags) & WindowsAttributesStored
		}
	}

	return attrs
}

// metaValue looks key up case-insensitively, preferring the current key over
// its legacy prefixed form.
func metaValue(metadata map[string]string, key string) (string, bool) {
	legacy, haveLegacy := "", false
	for k, v := range metadata {
		switch strings.ToLower(k) {
		case key:
			return v, true
		case legacyMetaKeyPrefix + key:
			legacy, haveLegacy = v, true
		}
	}
	return legacy, haveLegacy
}

// isPOSIXMetaKey reports whether k spells a POSIX attribute key, in any case
// or legacy form.
func isPOSIXMetaKey(k string) bool {
	name := strings.TrimPrefix(strings.ToLower(k), legacyMetaKeyPrefix)
	for _, key := range posixMetaKeys {
		if name == key {
			return true
		}
	}
	return false
}

// MergePOSIXMetadata returns existing user metadata with attrs written over
// it. Every spelling of the POSIX keys is replaced by the current keys, so
// legacy keys are not written back; all other keys are kept.
func MergePOSIXMetadata(existing map[string]string, attrs *types.POSIXAttributes) map[string]string {
	merged := make(map[string]string, len(existing)+len(posixMetaKeys))
	for k, v := range existing {
		if !isPOSIXMetaKey(k) {
			merged[k] = v
		}
	}
	for k, v := range EncodePOSIXAttributes(attrs) {
		merged[k] = v
	}
	return merged
}

// AttributeUpdate is a metadata-only attribute change. Nil fields keep the
// current value.
type AttributeUpdate struct {
	Mode              *os.FileMode
	UID, GID          *int
	Atime, Mtime      *time.Time
	Btime             *time.Time
	WindowsAttributes *uint32
}

// Apply writes the update over attrs, stamping Ctime with now when anything
// changed.
func (u AttributeUpdate) Apply(attrs *types.POSIXAttributes, now time.Time) {
	changed := false
	if u.Mode != nil {
		attrs.Mode = *u.Mode
		changed = true
	}
	if u.UID != nil {
		attrs.UID = *u.UID
		changed = true
	}
	if u.GID != nil {
		attrs.GID = *u.GID
		changed = true
	}
	if u.Atime != nil {
		attrs.Atime = *u.Atime
		changed = true
	}
	if u.Mtime != nil {
		attrs.Mtime = *u.Mtime
		changed = true
	}
	if u.Btime != nil {
		attrs.Btime = *u.Btime
		changed = true
	}
	if u.WindowsAttributes != nil {
		attrs.WindowsAttributes = *u.WindowsAttributes & WindowsAttributesStored
		changed = true
	}
	if changed {
		attrs.Ctime = now
	}
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
