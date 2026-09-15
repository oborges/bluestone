package metrics

import "context"

// Protocol labels for request metrics.
const (
	// ProtocolNFS marks requests issued by the NFS server.
	ProtocolNFS = "nfs"
	// ProtocolSMB marks requests issued by the SMB server.
	ProtocolSMB = "smb"
	// ProtocolInternal marks requests with no protocol attached, such as
	// background work inside the gateway.
	ProtocolInternal = "internal"
)

type protocolKey struct{}

// WithProtocol returns a context whose requests are attributed to protocol
// in metrics. An empty protocol leaves ctx unchanged.
func WithProtocol(ctx context.Context, protocol string) context.Context {
	if protocol == "" {
		return ctx
	}
	return context.WithValue(ctx, protocolKey{}, protocol)
}

// ProtocolFrom returns the protocol carried by ctx, or ProtocolInternal.
func ProtocolFrom(ctx context.Context) string {
	if ctx != nil {
		if protocol, ok := ctx.Value(protocolKey{}).(string); ok {
			return protocol
		}
	}
	return ProtocolInternal
}
