package bff

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleTargetsProxiesGateway(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/targets" {
			t.Errorf("gateway path = %q, want /targets", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"vm1","label":"VM 1","status":"ready"}]`))
	}))
	defer gw.Close()

	bff := httptest.NewServer(NewServer(gw.URL, "").Handler())
	defer bff.Close()

	resp, err := http.Get(bff.URL + "/api/targets")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); got != `[{"id":"vm1","label":"VM 1","status":"ready"}]` {
		t.Fatalf("proxied body = %q", got)
	}
}

func TestHandleTargetsGatewayDownIsBadGateway(t *testing.T) {
	// Point at an address with nothing listening.
	bff := httptest.NewServer(NewServer("http://127.0.0.1:1", "").Handler())
	defer bff.Close()

	resp, err := http.Get(bff.URL + "/api/targets")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestToWS(t *testing.T) {
	cases := map[string]string{
		"http://h:8081":  "ws://h:8081",
		"https://h:8081": "wss://h:8081",
		"http://h/":      "ws://h",
	}
	for in, want := range cases {
		if got := toWS(in); got != want {
			t.Fatalf("toWS(%q) = %q, want %q", in, got, want)
		}
	}
}
