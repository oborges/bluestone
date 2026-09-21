package posix

import (
	"reflect"
	"testing"

	"github.com/oborges/bluestone/pkg/types"
)

func TestStreamsRoundTripThroughMetadata(t *testing.T) {
	streams := map[string][]byte{
		"Zone.Identifier": []byte("[ZoneTransfer]\r\nZoneId=3\r\n"),
		"AFP_AfpInfo":     append([]byte("AFP\x00"), make([]byte, 56)...),
		"empty":           nil,
		"ünïcode":         {0, 1, 2, 255},
	}
	attrs := DefaultAttributes(false)
	attrs.Streams = streams
	metadata := EncodePOSIXAttributes(attrs)
	for key, value := range metadata {
		for _, r := range value {
			if r > 0x7E || r < 0x20 {
				t.Fatalf("metadata %s holds %q, which is not printable ASCII", key, r)
			}
		}
	}
	got := DecodePOSIXAttributes(metadata, false).Streams
	if len(got) != len(streams) {
		t.Fatalf("decoded %d streams, want %d", len(got), len(streams))
	}
	for name, data := range streams {
		if string(got[name]) != string(data) {
			t.Errorf("stream %q = %q, want %q", name, got[name], data)
		}
	}
	if size := StreamsSize(streams); size != len(metadata[MetaKeyStreams]) {
		t.Errorf("StreamsSize = %d, encoded value is %d bytes", size, len(metadata[MetaKeyStreams]))
	}
}

// Removing the last stream removes the metadata key, and other metadata
// survives a merge.
func TestStreamsMergeAndRemove(t *testing.T) {
	existing := map[string]string{"other": "kept", MetaKeyStreams: EncodeStreams(map[string][]byte{"a": []byte("b")})}
	attrs := DecodePOSIXAttributes(existing, false)
	AttributeUpdate{Streams: map[string][]byte{}}.Apply(attrs, attrs.Mtime)
	merged := MergePOSIXMetadata(existing, attrs)
	if _, ok := merged[MetaKeyStreams]; ok {
		t.Error("streams key kept after the last stream was removed")
	}
	if merged["other"] != "kept" {
		t.Error("unrelated metadata lost in the merge")
	}
}

// A damaged value yields no streams rather than failing the file.
func TestDecodeStreamsIgnoresDamage(t *testing.T) {
	for _, value := range []string{"!!!not base64", EncodeStreams(map[string][]byte{"a": []byte("bcd")})[:3]} {
		if got := DecodeStreams(value); got != nil {
			t.Errorf("DecodeStreams(%q) = %v, want nothing", value, got)
		}
	}
}

func TestCloneStreamsDoesNotShare(t *testing.T) {
	original := map[string][]byte{"a": []byte("x")}
	clone := types.CloneStreams(original)
	clone["a"][0] = 'y'
	if !reflect.DeepEqual(original, map[string][]byte{"a": []byte("x")}) {
		t.Fatal("changing a clone changed the original")
	}
}
