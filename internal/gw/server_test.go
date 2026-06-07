package gw

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abyss0-dev/traced-web-terminal/internal/runtime"
	"github.com/gorilla/websocket"
)

type fakeRuntime struct {
	targets   []runtime.Target
	attachErr error
	sess      runtime.Session
	attached  chan string

	traceErr    error
	traceStream io.ReadCloser
	traced      chan string
}

func (f *fakeRuntime) Targets() []runtime.Target { return f.targets }
func (f *fakeRuntime) EnsureStarted() error      { return nil }
func (f *fakeRuntime) Shutdown() error           { return nil }
func (f *fakeRuntime) Attach(id string) (runtime.Session, error) {
	if f.attached != nil {
		f.attached <- id
	}
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return f.sess, nil
}
func (f *fakeRuntime) TraceStream(id string) (io.ReadCloser, error) {
	if f.traced != nil {
		f.traced <- id
	}
	if f.traceErr != nil {
		return nil, f.traceErr
	}
	return f.traceStream, nil
}

func TestHandleTargetsReturnsStatusJSON(t *testing.T) {
	rt := &fakeRuntime{targets: []runtime.Target{
		{ID: "vm1", Label: "VM 1", Status: runtime.StatusReady},
		{ID: "vm2", Label: "VM 2", Status: runtime.StatusBooting},
	}}
	srv := httptest.NewServer(NewServer(rt).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/targets")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var got []runtime.Target
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 || got[0].ID != "vm1" || got[0].Status != runtime.StatusReady {
		t.Fatalf("unexpected targets: %+v", got)
	}
	if got[1].Status != runtime.StatusBooting {
		t.Fatalf("vm2 status = %q, want booting", got[1].Status)
	}
}

func TestHandleAttachMissingIDIsBadRequest(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeRuntime{}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/attach")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleAttachRuntimeErrorIsBadGateway(t *testing.T) {
	rt := &fakeRuntime{attachErr: errors.New("not ready")}
	srv := httptest.NewServer(NewServer(rt).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/attach?id=vm1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestHandleAttachBridgesSession(t *testing.T) {
	sess := newFakeSession()
	rt := &fakeRuntime{sess: sess, attached: make(chan string, 1)}
	srv := httptest.NewServer(NewServer(rt).Handler())
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/attach?id=vm3"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	select {
	case id := <-rt.attached:
		if id != "vm3" {
			t.Fatalf("attached id = %q, want vm3", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach was not called")
	}

	// End-to-end: keystroke in, output out.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-sess.in:
		if string(got) != "x" {
			t.Fatalf("session received %q, want x", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session never received input")
	}
}
