// Package wire defines the framing convention shared by both WebSocket hops
// (browser↔BFF and BFF↔GW) so that frames pass end-to-end unmodified.
//
// The convention is intentionally minimal:
//
//	Binary frame → raw terminal bytes (keystrokes one way, shell output the other)
//	Text frame   → a JSON control message (browser → backend)
//
// Keeping the framing in one place lets the BFF relay frames without
// interpreting payloads while the GW and browser agree on control semantics.
package wire

import "encoding/json"

// MsgTypeResize is the control message that reconciles the remote PTY size with
// the browser viewport. It is the only control type required by the PoC.
const MsgTypeResize = "resize"

// Control is a JSON control message carried in a WebSocket text frame.
//
// Cols and Rows are PTY dimensions in character cells. They are meaningful for
// MsgTypeResize and ignored for other types.
type Control struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// IsResize reports whether the control message is a resize request.
func (c Control) IsResize() bool {
	return c.Type == MsgTypeResize
}

// MsgTypeTraceMeta is the control envelope the GW pushes on the /ws/trace
// channel to report stream meta — currently the cumulative count of event lines
// dropped by the backpressure policy. It is distinguishable from a BehaviorEvent
// line by its "type" field, which BehaviorEvent records do not carry at the top
// level.
const MsgTypeTraceMeta = "trace_meta"

// TraceMeta is a stream-meta control message carried on the /ws/trace channel
// as a text frame, backend → browser only.
type TraceMeta struct {
	Type    string `json:"type"`
	Dropped int    `json:"dropped"`
}

// EncodeTraceMeta serialises a trace-meta frame reporting the cumulative dropped
// count.
func EncodeTraceMeta(dropped int) ([]byte, error) {
	return json.Marshal(TraceMeta{Type: MsgTypeTraceMeta, Dropped: dropped})
}

// EncodeControl serialises a control message to its JSON text-frame payload.
func EncodeControl(c Control) ([]byte, error) {
	return json.Marshal(c)
}

// DecodeControl parses a JSON text-frame payload into a control message. The
// Type field is preserved verbatim so callers can detect unknown control types.
func DecodeControl(b []byte) (Control, error) {
	var c Control
	if err := json.Unmarshal(b, &c); err != nil {
		return Control{}, err
	}
	return c, nil
}
