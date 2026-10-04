package nfs

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// Clients that cannot be told which port to use (AIX's NFSv3 mount has no
// mountport option) ask the server's portmapper where the NFS and MOUNT
// programs listen. The gateway serves both on its one NFS port, so it can
// tell the host's portmapper (rpcbind) so.

const (
	portmapAddr = "127.0.0.1:111"

	rpcProgPortmap = 100000
	rpcProgNFS     = 100003
	rpcProgMount   = 100005

	pmapProcSet   = 1
	pmapProcUnset = 2

	ipProtoTCP = 6
)

// portmapPrograms are the program versions an NFSv3 client looks up.
var portmapPrograms = [][2]uint32{
	{rpcProgNFS, 3},
	{rpcProgMount, 3},
	{rpcProgMount, 1},
}

// registerPortmap tells the host's portmapper that the NFSv3 programs are on
// port. It returns an error if the portmapper is not there or refuses.
func registerPortmap(port int) error {
	for _, p := range portmapPrograms {
		// A stale mapping, from an earlier run on another port, would
		// make SET fail.
		_, _ = portmapCall(portmapAddr, pmapProcUnset, p[0], p[1], 0)
		ok, err := portmapCall(portmapAddr, pmapProcSet, p[0], p[1], uint32(port))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("portmapper refused program %d version %d", p[0], p[1])
		}
	}
	return nil
}

// unregisterPortmap withdraws what registerPortmap set.
func unregisterPortmap() {
	for _, p := range portmapPrograms {
		_, _ = portmapCall(portmapAddr, pmapProcUnset, p[0], p[1], 0)
	}
}

// portmapCall makes one PMAPPROC_SET or PMAPPROC_UNSET call (RFC 1833,
// version 2) over UDP and returns the portmapper's answer.
func portmapCall(addr string, proc, prog, vers, port uint32) (bool, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	xid := uint32(time.Now().UnixNano())
	call := []uint32{
		xid, 0 /* CALL */, 2 /* RPC version */, rpcProgPortmap, 2, proc,
		0, 0, // AUTH_NONE credential
		0, 0, // AUTH_NONE verifier
		prog, vers, ipProtoTCP, port,
	}
	msg := make([]byte, 4*len(call))
	for i, v := range call {
		binary.BigEndian.PutUint32(msg[4*i:], v)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		return false, err
	}
	reply := make([]byte, 64)
	n, err := conn.Read(reply)
	if err != nil {
		return false, fmt.Errorf("no answer from the portmapper at %s: %w", addr, err)
	}
	// xid, REPLY, MSG_ACCEPTED, verifier (flavor, length), SUCCESS, bool
	if n < 28 || binary.BigEndian.Uint32(reply[0:]) != xid ||
		binary.BigEndian.Uint32(reply[8:]) != 0 || binary.BigEndian.Uint32(reply[20:]) != 0 {
		return false, fmt.Errorf("portmapper at %s rejected the call", addr)
	}
	return binary.BigEndian.Uint32(reply[24:]) == 1, nil
}
