package wire

import "fmt"

const (
	StatusSuccess                uint32 = 0x00000000
	StatusPending                uint32 = 0x00000103
	StatusCancelled              uint32 = 0xC0000120
	StatusInvalidHandle          uint32 = 0xC0000008
	StatusNotImplemented         uint32 = 0xC0000002
	StatusInvalidParameter       uint32 = 0xC000000D
	StatusNoSuchFile             uint32 = 0xC000000F
	StatusInvalidDeviceRequest   uint32 = 0xC0000010
	StatusEndOfFile              uint32 = 0xC0000011
	StatusMoreProcessingRequired uint32 = 0xC0000016
	StatusNoMemory               uint32 = 0xC0000017
	StatusAccessDenied           uint32 = 0xC0000022
	StatusInternalError          uint32 = 0xC00000E5
	StatusBufferTooSmall         uint32 = 0xC0000023
	StatusBufferOverflow         uint32 = 0x80000005
	StatusObjectNameNotFound     uint32 = 0xC0000034
	StatusObjectNameCollision    uint32 = 0xC0000035
	StatusObjectPathNotFound     uint32 = 0xC000003A
	StatusInsufficientResources  uint32 = 0xC000009A
	StatusFileIsADirectory       uint32 = 0xC00000BA
	StatusNotSupported           uint32 = 0xC00000BB
	StatusInvalidInfoClass       uint32 = 0xC0000003
	StatusInfoLengthMismatch     uint32 = 0xC0000004
	StatusSharingViolation       uint32 = 0xC0000043
	StatusLockNotGranted         uint32 = 0xC0000055
	StatusNotADirectory          uint32 = 0xC0000103
	StatusLogonFailure           uint32 = 0xC000006D
	StatusBadNetworkName         uint32 = 0xC00000CC
	StatusNetworkNameDeleted     uint32 = 0xC00000C9
	StatusLockConflict           uint32 = 0xC0000054
	StatusRangeNotLocked         uint32 = 0xC000007A
	StatusNoMoreFiles            uint32 = 0x80000006
	StatusUserSessionDeleted     uint32 = 0xC0000203
	StatusDiskFull               uint32 = 0xC000007F
	StatusDirectoryNotEmpty      uint32 = 0xC0000101
	StatusDeletePending          uint32 = 0xC0000056
	StatusCannotDelete           uint32 = 0xC0000121
	StatusMediaWriteProtected    uint32 = 0xC00000A2
	StatusObjectNameInvalid      uint32 = 0xC0000033
	StatusIOTimeout              uint32 = 0xC00000B5
	StatusUnexpectedIOError      uint32 = 0xC00000E9
	StatusConnectionDisconnected uint32 = 0xC000020C
)

type NTError struct {
	Status uint32
}

func (e *NTError) Error() string {
	return fmt.Sprintf("ntstatus 0x%08X", e.Status)
}

func NewNTError(status uint32) *NTError { return &NTError{Status: status} }
