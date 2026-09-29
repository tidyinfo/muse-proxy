package proto

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	payload := []byte("127.0.0.1:22")
	enc := Encode(FrameOpen, 7, payload)
	typ, id, dec, ok := Decode(enc)
	if !ok {
		t.Fatal("decode failed")
	}
	if typ != FrameOpen || id != 7 || !bytes.Equal(dec, payload) {
		t.Fatalf("round trip mismatch: typ=%d id=%d payload=%q", typ, id, dec)
	}
}

func TestDecodeShort(t *testing.T) {
	for _, msg := range [][]byte{nil, {}, {0x01}, {0x01, 0, 0, 0}} {
		if _, _, _, ok := Decode(msg); ok {
			t.Fatalf("Decode(%v) should fail", msg)
		}
	}
}

func TestAllFrameTypes(t *testing.T) {
	types := []byte{FrameOpen, FrameData, FrameClose, FramePing, FramePong,
		FrameOpenShell, FrameWinch}
	for _, typ := range types {
		enc := Encode(typ, 0x01020304, []byte("x"))
		gotTyp, gotID, _, ok := Decode(enc)
		if !ok || gotTyp != typ || gotID != 0x01020304 {
			t.Fatalf("type 0x%02x round trip failed", typ)
		}
	}
	if len(Encode(FramePing, 0, nil)) != HeaderLen {
		t.Fatal("empty payload frame must be exactly HeaderLen bytes")
	}
}

// TestUnknownFrameTypesAreDecodable pins the forward-compatibility contract in
// PROTOCOL.md ("Implementations MUST ignore unknown frame types") and
// CONTRIBUTING.md. Type bytes 0x08/0x09 were UDP frames in an unreleased
// draft, so a peer running that build still puts them on the wire: the codec
// must hand them back intact and let the dispatch layer drop them, rather
// than refusing to decode.
func TestUnknownFrameTypesAreDecodable(t *testing.T) {
	for _, typ := range []byte{0x08, 0x09, 0x0a, 0xff} {
		enc := Encode(typ, 0x01020304, []byte("payload"))
		gotTyp, gotID, payload, ok := Decode(enc)
		if !ok {
			t.Fatalf("type 0x%02x must still decode", typ)
		}
		if gotTyp != typ || gotID != 0x01020304 || string(payload) != "payload" {
			t.Fatalf("type 0x%02x: got typ=%d id=%d payload=%q", typ, gotTyp, gotID, payload)
		}
	}
}
