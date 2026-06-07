package wire

import (
	"encoding/json"
	"testing"
)

func TestEncodeDecodeResizeRoundTrip(t *testing.T) {
	in := Control{Type: MsgTypeResize, Cols: 120, Rows: 40}

	b, err := EncodeControl(in)
	if err != nil {
		t.Fatalf("EncodeControl: %v", err)
	}

	out, err := DecodeControl(b)
	if err != nil {
		t.Fatalf("DecodeControl: %v", err)
	}

	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestDecodeControlRejectsInvalidJSON(t *testing.T) {
	if _, err := DecodeControl([]byte("not json")); err == nil {
		t.Fatal("expected error decoding invalid JSON, got nil")
	}
}

func TestDecodeControlUnknownTypePreserved(t *testing.T) {
	// Decoding must not invent a type; an unknown control type is surfaced
	// verbatim so the caller decides how to handle it.
	c, err := DecodeControl([]byte(`{"type":"future","cols":1,"rows":2}`))
	if err != nil {
		t.Fatalf("DecodeControl: %v", err)
	}
	if c.Type != "future" {
		t.Fatalf("type = %q, want %q", c.Type, "future")
	}
}

func TestEncodeTraceMeta(t *testing.T) {
	b, err := EncodeTraceMeta(7)
	if err != nil {
		t.Fatalf("EncodeTraceMeta: %v", err)
	}
	var got TraceMeta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != MsgTypeTraceMeta || got.Dropped != 7 {
		t.Fatalf("trace meta = %+v, want {trace_meta 7}", got)
	}
}

func TestIsResize(t *testing.T) {
	if !(Control{Type: MsgTypeResize}).IsResize() {
		t.Fatal("resize control reported as non-resize")
	}
	if (Control{Type: "other"}).IsResize() {
		t.Fatal("non-resize control reported as resize")
	}
}
