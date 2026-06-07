package bff

import "sync"

// wsConn is the subset of *websocket.Conn the relay needs.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// relay copies WebSocket frames 1:1 between the browser and the Gateway,
// preserving frame type so control and data frames pass end-to-end unmodified.
// It returns once either side closes; both connections are then torn down.
//
// Each connection has exactly one reader goroutine and one writer goroutine,
// which is the concurrency contract gorilla/websocket requires.
func relay(browser, gw wsConn) {
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			_ = browser.Close()
			_ = gw.Close()
		})
	}

	go pump(browser, gw, shutdown) // browser → gateway
	pump(gw, browser, shutdown)    // gateway → browser
}

// pump forwards every frame from src to dst until either errors, then triggers
// shutdown of both connections.
func pump(src, dst wsConn, shutdown func()) {
	defer shutdown()
	for {
		mt, data, err := src.ReadMessage()
		if err != nil {
			return
		}
		if err := dst.WriteMessage(mt, data); err != nil {
			return
		}
	}
}
