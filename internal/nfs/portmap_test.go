package nfs

import (
	"encoding/binary"
	"net"
	"testing"
)

// fakePortmapper answers SET and UNSET calls and records them.
func fakePortmapper(t *testing.T, answer uint32) (string, chan [5]uint32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	calls := make(chan [5]uint32, 16)
	go func() {
		buf := make([]byte, 128)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 56 {
				continue
			}
			word := func(i int) uint32 { return binary.BigEndian.Uint32(buf[4*i:]) }
			// procedure, then program, version, protocol and port
			calls <- [5]uint32{word(5), word(10), word(11), word(12), word(13)}
			reply := make([]byte, 28)
			copy(reply, buf[:4])
			binary.BigEndian.PutUint32(reply[4:], 1) // REPLY
			binary.BigEndian.PutUint32(reply[24:], answer)
			_, _ = conn.WriteTo(reply, from)
		}
	}()
	return conn.LocalAddr().String(), calls
}

func TestPortmapCallSetsAMapping(t *testing.T) {
	addr, calls := fakePortmapper(t, 1)
	ok, err := portmapCall(addr, pmapProcSet, rpcProgMount, 3, 2049)
	if err != nil || !ok {
		t.Fatalf("portmapCall = %v, %v; want true, nil", ok, err)
	}
	if got, want := <-calls, [5]uint32{pmapProcSet, rpcProgMount, 3, ipProtoTCP, 2049}; got != want {
		t.Fatalf("portmapper saw %v, want %v", got, want)
	}
}

func TestPortmapCallReportsARefusal(t *testing.T) {
	addr, _ := fakePortmapper(t, 0)
	if ok, err := portmapCall(addr, pmapProcSet, rpcProgNFS, 3, 2049); err != nil || ok {
		t.Fatalf("portmapCall = %v, %v; want false, nil", ok, err)
	}
}
