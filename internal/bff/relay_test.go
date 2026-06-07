package bff

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// echoGW is a stand-in Gateway: it accepts a WebSocket and echoes every frame
// back with its type preserved.
func echoGW(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if err := ws.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialBFF(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?id=vm1"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial bff: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRelayPreservesBinaryFrames(t *testing.T) {
	gw := echoGW(t)
	bff := httptest.NewServer(NewServer(gw.URL, "").Handler())
	defer bff.Close()

	c := dialBFF(t, bff)
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("keystroke")); err != nil {
		t.Fatalf("write: %v", err)
	}
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage || string(data) != "keystroke" {
		t.Fatalf("relayed mt=%d data=%q, want binary 'keystroke'", mt, data)
	}
}

func TestRelayPreservesTextFrames(t *testing.T) {
	gw := echoGW(t)
	bff := httptest.NewServer(NewServer(gw.URL, "").Handler())
	defer bff.Close()

	c := dialBFF(t, bff)
	ctrl := `{"type":"resize","cols":100,"rows":30}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(ctrl)); err != nil {
		t.Fatalf("write: %v", err)
	}
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.TextMessage || string(data) != ctrl {
		t.Fatalf("relayed mt=%d data=%q, want text control", mt, data)
	}
}

func TestRelayPropagatesTargetIDToGateway(t *testing.T) {
	var gotPath string
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.Close()
	}))
	defer gw.Close()

	bff := httptest.NewServer(NewServer(gw.URL, "").Handler())
	defer bff.Close()

	url := "ws" + strings.TrimPrefix(bff.URL, "http") + "/ws?id=vm2"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(gotPath, "id=vm2") {
		t.Fatalf("gateway path = %q, want it to carry id=vm2", gotPath)
	}
}
