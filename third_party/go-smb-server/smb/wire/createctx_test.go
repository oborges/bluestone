package wire

import "testing"

// Contexts written as a response chain read back as they were, 8-byte
// aligned and linked.
func TestCreateContextsRoundTrip(t *testing.T) {
	lease := LeaseRequest{Key: [16]byte{1, 2, 3}, State: LeaseRead | LeaseHandle, V2: true, Epoch: 7}
	in := []CreateContext{
		{Name: "MxAc"},
		{Name: CreateContextLease, Data: lease.Encode()},
		{Name: "QFid", Data: []byte{9, 9, 9}},
	}
	buf := appendCreateContexts(nil, in)
	out, err := parseCreateContexts(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("read %d contexts, wrote %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Name != in[i].Name || string(out[i].Data) != string(in[i].Data) {
			t.Errorf("context %d = %q %x, want %q %x", i, out[i].Name, out[i].Data, in[i].Name, in[i].Data)
		}
	}
	got, err := ParseLeaseRequest(out[1].Data)
	if err != nil || got != lease {
		t.Fatalf("lease = %+v, %v; want %+v", got, err, lease)
	}
	if _, err := ParseLeaseRequest(make([]byte, 20)); err == nil {
		t.Fatal("a short lease context parsed")
	}
}

// A create request's contexts are found by name.
func TestCreateRequestContexts(t *testing.T) {
	name := UTF16ToBytes("f.txt")
	msg := make([]byte, 64+56)
	body := msg[64:]
	body[0] = 57
	nameOff := len(msg)
	msg = append(msg, name...)
	for len(msg)%8 != 0 {
		msg = append(msg, 0)
	}
	ctxOff := len(msg)
	msg = appendCreateContexts(msg, []CreateContext{{Name: CreateContextLease, Data: make([]byte, 32)}})
	body = msg[64:]
	body[44], body[46] = byte(nameOff), byte(len(name))
	body[48] = byte(ctxOff)
	body[52] = byte(len(msg) - ctxOff)

	var req CreateRequest
	if err := req.Parse(msg); err != nil {
		t.Fatal(err)
	}
	if data, ok := req.Context(CreateContextLease); !ok || len(data) != 32 {
		t.Fatalf("lease context = %d bytes, found %v", len(data), ok)
	}
	if _, ok := req.Context("DHnQ"); ok {
		t.Fatal("found a context that was not sent")
	}
}
