// Package proto defines the muse-proxy multiplexed frame protocol.
//
// Every message is: [1 byte type][4 bytes stream id, big endian][payload].
// See PROTOCOL.md at the repository root for the full specification.
package proto

// Frame types, v1. 0x08-0x09 are reserved (previously UDP forwarding).
const (
	FrameOpen  = 0x01 // payload: "host:port" UTF-8
	FrameData  = 0x02 // payload: opaque bytes
	FrameClose = 0x03 // payload: empty
	FramePing  = 0x04 // stream id must be 0
	FramePong  = 0x05 // stream id must be 0

	FrameOpenShell = 0x06 // v2: payload {"cols":N,"rows":M} JSON
	FrameWinch     = 0x07 // v2: payload "COLSxROWS" UTF-8
)

// HeaderLen is the fixed frame header size in bytes.
const HeaderLen = 5

// Encode serializes one frame.
func Encode(typ byte, id uint32, payload []byte) []byte {
	buf := make([]byte, HeaderLen+len(payload))
	buf[0] = typ
	buf[1] = byte(id >> 24)
	buf[2] = byte(id >> 16)
	buf[3] = byte(id >> 8)
	buf[4] = byte(id)
	copy(buf[HeaderLen:], payload)
	return buf
}

// Decode parses one frame. ok is false for messages shorter than HeaderLen.
func Decode(msg []byte) (typ byte, id uint32, payload []byte, ok bool) {
	if len(msg) < HeaderLen {
		return 0, 0, nil, false
	}
	typ = msg[0]
	id = uint32(msg[1])<<24 | uint32(msg[2])<<16 | uint32(msg[3])<<8 | uint32(msg[4])
	return typ, id, msg[HeaderLen:], true
}
