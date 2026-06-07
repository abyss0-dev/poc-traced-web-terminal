package bff

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// traceGW is a stand-in Gateway /trace endpoint: it records the request path and
// pushes one text frame outward, modelling the backend → browser trace channel.
func traceGW(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.URL.RequestURI()
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"event":"x"}`))
		// Hold open until the browser side closes.
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTraceRelayForwardsEventLine(t *testing.T) {
	gw := traceGW(t, nil)
	bff := httptest.NewServer(NewServer(gw.URL, "").Handler())
	defer bff.Close()

	url := "ws" + strings.TrimPrefix(bff.URL, "http") + "/ws/trace?id=vm1"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.TextMessage || string(data) != `{"event":"x"}` {
		t.Fatalf("relayed mt=%d data=%q, want text event line", mt, data)
	}
}

func TestTraceRelayRoutesToTraceEndpointWithID(t *testing.T) {
	var gotPath string
	gw := traceGW(t, &gotPath)
	bff := httptest.NewServer(NewServer(gw.URL, "").Handler())
	defer bff.Close()

	url := "ws" + strings.TrimPrefix(bff.URL, "http") + "/ws/trace?id=vm2"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond)

	if !strings.Contains(gotPath, "/trace") || !strings.Contains(gotPath, "id=vm2") {
		t.Fatalf("gateway path = %q, want /trace?id=vm2", gotPath)
	}
}
