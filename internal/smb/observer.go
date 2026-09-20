package smb

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/oborges/bluestone/internal/metrics"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"
)

// observer turns what the SMB server does into metrics, and keeps the counts
// the health check reports. The library calls it on the path of the request
// it describes, so it only counts.
type observer struct {
	connections atomic.Int64
	sessions    atomic.Int64
}

func (o *observer) ConnectionOpened() {
	metrics.SetSMBConnections(int(o.connections.Add(1)))
}

func (o *observer) ConnectionClosed() {
	metrics.SetSMBConnections(int(o.connections.Add(-1)))
}

func (o *observer) SessionOpened() {
	metrics.SetSMBSessions(int(o.sessions.Add(1)))
}

func (o *observer) SessionClosed() {
	metrics.SetSMBSessions(int(o.sessions.Add(-1)))
}

func (o *observer) RequestCompleted(command uint16, status uint32, took time.Duration) {
	metrics.RecordSMBRequest(commandName(command), statusName(status), took)
}

// Stats is what the SMB server is currently carrying.
type Stats struct {
	// Connections is the number of connected clients.
	Connections int
	// Sessions is the number of authenticated sessions across them.
	Sessions int
	// OpenFiles is the number of files held open through this server, or -1
	// when the server has no share table to ask.
	OpenFiles int
}

// Stats reports what the server is currently carrying, for health checks and
// dashboards.
func (s *Server) Stats() Stats {
	stats := Stats{
		Connections: int(s.observer.connections.Load()),
		Sessions:    int(s.observer.sessions.Load()),
		OpenFiles:   -1,
	}
	if s.opens != nil {
		stats.OpenFiles = s.opens.Len()
	}
	return stats
}

var commandNames = map[uint16]string{
	wire.CmdNegotiate:      "NEGOTIATE",
	wire.CmdSessionSetup:   "SESSION_SETUP",
	wire.CmdLogoff:         "LOGOFF",
	wire.CmdTreeConnect:    "TREE_CONNECT",
	wire.CmdTreeDisconnect: "TREE_DISCONNECT",
	wire.CmdCreate:         "CREATE",
	wire.CmdClose:          "CLOSE",
	wire.CmdFlush:          "FLUSH",
	wire.CmdRead:           "READ",
	wire.CmdWrite:          "WRITE",
	wire.CmdLock:           "LOCK",
	wire.CmdIoctl:          "IOCTL",
	wire.CmdCancel:         "CANCEL",
	wire.CmdEcho:           "ECHO",
	wire.CmdQueryDirectory: "QUERY_DIRECTORY",
	wire.CmdChangeNotify:   "CHANGE_NOTIFY",
	wire.CmdQueryInfo:      "QUERY_INFO",
	wire.CmdSetInfo:        "SET_INFO",
	wire.CmdOplockBreak:    "OPLOCK_BREAK",
	wire.CmdServerNotify:   "SERVER_NOTIFY",
}

// commandName labels a metric with the SMB2 command. Unknown commands share
// one label, so a client sending nonsense cannot grow the metric's
// cardinality.
func commandName(command uint16) string {
	if name, ok := commandNames[command]; ok {
		return name
	}
	return "OTHER"
}

var statusNames = map[uint32]string{
	wire.StatusSuccess:                "STATUS_SUCCESS",
	wire.StatusPending:                "STATUS_PENDING",
	wire.StatusNoMoreFiles:            "STATUS_NO_MORE_FILES",
	wire.StatusCancelled:              "STATUS_CANCELLED",
	wire.StatusNotImplemented:         "STATUS_NOT_IMPLEMENTED",
	wire.StatusInvalidInfoClass:       "STATUS_INVALID_INFO_CLASS",
	wire.StatusInfoLengthMismatch:     "STATUS_INFO_LENGTH_MISMATCH",
	wire.StatusInvalidHandle:          "STATUS_INVALID_HANDLE",
	wire.StatusInvalidParameter:       "STATUS_INVALID_PARAMETER",
	wire.StatusNoSuchFile:             "STATUS_NO_SUCH_FILE",
	wire.StatusInvalidDeviceRequest:   "STATUS_INVALID_DEVICE_REQUEST",
	wire.StatusEndOfFile:              "STATUS_END_OF_FILE",
	wire.StatusMoreProcessingRequired: "STATUS_MORE_PROCESSING_REQUIRED",
	wire.StatusNoMemory:               "STATUS_NO_MEMORY",
	wire.StatusAccessDenied:           "STATUS_ACCESS_DENIED",
	wire.StatusBufferTooSmall:         "STATUS_BUFFER_TOO_SMALL",
	wire.StatusObjectNameNotFound:     "STATUS_OBJECT_NAME_NOT_FOUND",
	wire.StatusObjectNameCollision:    "STATUS_OBJECT_NAME_COLLISION",
	wire.StatusObjectPathNotFound:     "STATUS_OBJECT_PATH_NOT_FOUND",
	wire.StatusSharingViolation:       "STATUS_SHARING_VIOLATION",
	wire.StatusLockConflict:           "STATUS_FILE_LOCK_CONFLICT",
	wire.StatusLockNotGranted:         "STATUS_LOCK_NOT_GRANTED",
	wire.StatusLogonFailure:           "STATUS_LOGON_FAILURE",
	wire.StatusRangeNotLocked:         "STATUS_RANGE_NOT_LOCKED",
	wire.StatusInsufficientResources:  "STATUS_INSUFFICIENT_RESOURCES",
	wire.StatusFileIsADirectory:       "STATUS_FILE_IS_A_DIRECTORY",
	wire.StatusNotSupported:           "STATUS_NOT_SUPPORTED",
	wire.StatusNetworkNameDeleted:     "STATUS_NETWORK_NAME_DELETED",
	wire.StatusBadNetworkName:         "STATUS_BAD_NETWORK_NAME",
	wire.StatusNotADirectory:          "STATUS_NOT_A_DIRECTORY",
	wire.StatusUserSessionDeleted:     "STATUS_USER_SESSION_DELETED",
	wire.StatusDiskFull:               "STATUS_DISK_FULL",
	wire.StatusDirectoryNotEmpty:      "STATUS_DIRECTORY_NOT_EMPTY",
	wire.StatusMediaWriteProtected:    "STATUS_MEDIA_WRITE_PROTECTED",
	wire.StatusObjectNameInvalid:      "STATUS_OBJECT_NAME_INVALID",
	wire.StatusIOTimeout:              "STATUS_IO_TIMEOUT",
	wire.StatusUnexpectedIOError:      "STATUS_UNEXPECTED_IO_ERROR",
	wire.StatusConnectionDisconnected: "STATUS_CONNECTION_DISCONNECTED",
}

// statusName labels a metric with the NT status. A status the server does
// not name is reported by its number, which keeps the label readable in an
// alert without losing which status it was; the server only ever returns
// statuses it knows, so this cannot be driven by a client.
func statusName(status uint32) string {
	if name, ok := statusNames[status]; ok {
		return name
	}
	return fmt.Sprintf("0x%08X", status)
}
