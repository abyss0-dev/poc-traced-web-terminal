package gw

import (
	"sync"

	"github.com/abyss0-dev/traced-web-terminal/internal/runtime"
	"github.com/abyss0-dev/traced-web-terminal/internal/wire"
)

// readBufSize bounds a single session→WebSocket forward chunk.
const readBufSize = 32 * 1024

// bridge couples a WebSocket connection to an interactive Session, relaying
// frames in both directions per the wire convention:
//
//	binary frame ↔ raw session I/O
//	text frame   → control message (resize)
//
// It returns once either side closes; both the WebSocket and the Session are
// torn down exactly once.
func bridge(ws wsConn, sess runtime.Session) {
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			_ = sess.Close()
			_ = ws.Close()
		})
	}

	// Session output → browser (binary frames).
	go func() {
		defer shutdown()
		buf := make([]byte, readBufSize)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(wsBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Browser input → session (binary) / resize (text control).
	defer shutdown()
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		switch mt {
		case wsBinary:
			if _, err := sess.Write(data); err != nil {
				return
			}
		case wsText:
			ctrl, err := wire.DecodeControl(data)
			if err != nil {
				continue // ignore malformed control frames
			}
			if ctrl.IsResize() {
				_ = sess.Resize(ctrl.Cols, ctrl.Rows)
			}
		}
	}
}
