package gw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abyss0-dev/traced-web-terminal/internal/wire"
	"github.com/gorilla/websocket"
)

// fakeSession is an in-memory runtime.Session for exercising the bridge.
type fakeSession struct {
	in      chan []byte    // keystrokes written by the bridge
	out     chan []byte    // shell output the bridge should forward
	resizes chan [2]uint16 // {cols, rows}
	closed  chan struct{}
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		in:      make(chan []byte, 16),
		out:     make(chan []byte, 16),
		resizes: make(chan [2]uint16, 16),
		closed:  make(chan struct{}),
	}
}

func (f *fakeSession) Read(p []byte) (int, error) {
	select {
	case b := <-f.out:
		return copy(p, b), nil
	case <-f.closed:
		return 0, errClosed
	}
}

func (f *fakeSession) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case f.in <- b:
		return len(p), nil
	case <-f.closed:
		return 0, errClosed
	}
}

func (f *fakeSession) Resize(cols, rows uint16) error {
	f.resizes <- [2]uint16{cols, rows}
	return nil
}

func (f *fakeSession) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

var errClosed = &closedErr{}

type closedErr struct{}

func (*closedErr) Error() string { return "session closed" }

// bridgeServer wires the bridge under test to an httptest WebSocket endpoint.
func bridgeServer(t *testing.T, sess *fakeSession) (*websocket.Conn, *httptest.Server) {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		bridge(ws, sess)
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func TestBridgeForwardsKeystrokesToSession(t *testing.T) {
	sess := newFakeSession()
	c, _ := bridgeServer(t, sess)

	if err := c.WriteMessage(websocket.BinaryMessage, []byte("ls\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-sess.in:
		if string(got) != "ls\n" {
			t.Fatalf("session received %q, want %q", got, "ls\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session never received keystrokes")
	}
}

func TestBridgeForwardsSessionOutputAsBinary(t *testing.T) {
	sess := newFakeSession()
	c, _ := bridgeServer(t, sess)

	sess.out <- []byte("hello world")
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", mt)
	}
	if string(data) != "hello world" {
		t.Fatalf("output = %q, want %q", data, "hello world")
	}
}

func TestBridgeAppliesResizeControl(t *testing.T) {
	sess := newFakeSession()
	c, _ := bridgeServer(t, sess)

	ctrl, _ := wire.EncodeControl(wire.Control{Type: wire.MsgTypeResize, Cols: 120, Rows: 40})
	if err := c.WriteMessage(websocket.TextMessage, ctrl); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-sess.resizes:
		if got != [2]uint16{120, 40} {
			t.Fatalf("resize = %v, want {120 40}", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session never received resize")
	}
}

func TestBridgeClosesSessionWhenClientDisconnects(t *testing.T) {
	sess := newFakeSession()
	c, _ := bridgeServer(t, sess)

	_ = c.Close()
	select {
	case <-sess.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("session not closed after client disconnect")
	}
}
