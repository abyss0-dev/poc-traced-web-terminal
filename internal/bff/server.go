// Package bff implements the browser-facing front: it serves the static
// frontend, proxies the target list from the Gateway, and relays WebSocket
// frames 1:1 between the browser and the Gateway without interpreting payloads.
package bff

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

// Server is the BFF HTTP surface. It knows only the Gateway address; no
// credentials ever reach it.
type Server struct {
	gwHTTP   string // Gateway base URL, e.g. http://127.0.0.1:8081
	gwWS     string // Gateway base URL with ws scheme
	webDir   string // static assets directory; empty disables static serving
	upgrader websocket.Upgrader
	dialer   *websocket.Dialer
}

// NewServer builds a BFF over the given Gateway base URL (http scheme). webDir
// may be empty to disable static serving (used in tests).
func NewServer(gwBaseURL, webDir string) *Server {
	return &Server{
		gwHTTP:   strings.TrimRight(gwBaseURL, "/"),
		gwWS:     toWS(gwBaseURL),
		webDir:   webDir,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		dialer:   websocket.DefaultDialer,
	}
}

// toWS rewrites an http(s) base URL to its ws(s) equivalent.
func toWS(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return base
	}
}

// Handler returns the BFF's HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/targets", s.handleTargets)
	// "/ws/trace" is registered before "/ws"; both are exact patterns, so the
	// trace channel is never swallowed by the PTY route.
	mux.HandleFunc("/ws/trace", s.handleTraceWS)
	mux.HandleFunc("/ws", s.handleWS)
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}

// handleTargets proxies the Gateway target list to the browser. The Gateway
// response carries status but no credentials, so it is forwarded verbatim.
func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	resp, err := http.Get(s.gwHTTP + "/targets")
	if err != nil {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleWS upgrades the browser connection, opens a corresponding WebSocket to
// the Gateway for the chosen target, and relays frames in both directions.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing target id", http.StatusBadRequest)
		return
	}

	gwURL := s.gwWS + "/attach?id=" + url.QueryEscape(id)
	gwConn, _, err := s.dialer.Dial(gwURL, nil)
	if err != nil {
		http.Error(w, "gateway connection failed", http.StatusBadGateway)
		return
	}

	browserConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = gwConn.Close()
		return
	}

	relay(browserConn, gwConn)
}

// handleTraceWS upgrades the browser connection, opens a corresponding trace
// WebSocket to the Gateway, and relays frames. The trace channel is
// backend → browser only; the same bidirectional relay is reused so the
// browser→gateway pump still observes the browser's close and tears the stream
// down (the browser sends nothing else on it).
func (s *Server) handleTraceWS(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing target id", http.StatusBadRequest)
		return
	}

	gwURL := s.gwWS + "/trace?id=" + url.QueryEscape(id)
	gwConn, _, err := s.dialer.Dial(gwURL, nil)
	if err != nil {
		http.Error(w, "gateway connection failed", http.StatusBadGateway)
		return
	}

	browserConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = gwConn.Close()
		return
	}

	relay(browserConn, gwConn)
}
