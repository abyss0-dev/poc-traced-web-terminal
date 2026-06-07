package gw

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abyss0-dev/traced-web-terminal/internal/wire"
	"github.com/gorilla/websocket"
)

// pipeStream is an io.ReadCloser whose Read is fed by a pipe writer; Close
// signals via the closed channel so tests can assert teardown.
type pipeStream struct {
	r      *io.PipeReader
	closed chan struct{}
	once   sync.Once
}

func (p *pipeStream) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *pipeStream) Close() error {
	p.once.Do(func() { close(p.closed); _ = p.r.Close() })
	return nil
}

func newPipeStream() (*pipeStream, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return &pipeStream{r: pr, closed: make(chan struct{})}, pw
}

// traceServer wires handleTrace (via the Server) to an httptest endpoint and
// dials it, returning the browser-side WebSocket and the writer feeding the
// guest-side trace stream.
func traceServer(t *testing.T, stream io.ReadCloser) *websocket.Conn {
	t.Helper()
	rt := &fakeRuntime{traceStream: stream}
	srv := httptest.NewServer(NewServer(rt).Handler())
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/trace?id=vm1"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestTraceBridgeForwardsLinesAsText(t *testing.T) {
	stream, pw := newPipeStream()
	c := traceServer(t, stream)

	go func() { _, _ = pw.Write([]byte(`{"event":"a"}` + "\n" + `{"event":"b"}` + "\n")) }()

	for _, want := range []string{`{"event":"a"}`, `{"event":"b"}`} {
		mt, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt != websocket.TextMessage {
			t.Fatalf("frame type = %d, want text", mt)
		}
		if string(data) != want {
			t.Fatalf("line = %q, want %q", data, want)
		}
	}
}

func TestTraceBridgeClosesStreamOnBrowserClose(t *testing.T) {
	stream, _ := newPipeStream()
	c := traceServer(t, stream)

	_ = c.Close()
	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("trace stream not closed after browser disconnect")
	}
}

func TestTraceBridgeDropsOversizedLineWithMeta(t *testing.T) {
	stream, pw := newPipeStream()
	c := traceServer(t, stream)

	oversized := strings.Repeat("x", maxTraceLineBytes+1)
	go func() {
		_, _ = pw.Write([]byte(oversized + "\n"))
		_, _ = pw.Write([]byte(`{"event":"ok"}` + "\n"))
	}()

	// The oversized line is dropped (not forwarded); the next real line is
	// preceded by a trace_meta frame reporting the cumulative drop.
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("meta frame type = %d, want text", mt)
	}
	meta, err := wire.DecodeControl(data)
	if err != nil || meta.Type != wire.MsgTypeTraceMeta {
		t.Fatalf("first frame = %q, want trace_meta", data)
	}

	_, data, err = c.ReadMessage()
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	if string(data) != `{"event":"ok"}` {
		t.Fatalf("line = %q, want the non-oversized line", data)
	}
}

func TestHandleTraceMissingIDIsBadRequest(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeRuntime{}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/trace")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
