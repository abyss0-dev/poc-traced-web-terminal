package gw

import (
	"bufio"
	"bytes"
	"io"
	"sync"
	"sync/atomic"

	"github.com/abyss0-dev/traced-web-terminal/internal/wire"
)

const (
	// traceQueueSize bounds the GW-side buffer between the remote tail and the
	// WebSocket consumer. When it fills, the oldest queued line is dropped — the
	// producer-side backstop of the backpressure model (DESIGN §11).
	traceQueueSize = 1024
	// maxTraceLineBytes caps a single forwarded line. A BehaviorEvent with a full
	// Base64 packet/TTY payload can exceed 64 KB, so the reader uses ReadBytes
	// (not a fixed Scanner token); a line beyond this cap is dropped with the
	// counter rather than forwarded (DESIGN §9).
	maxTraceLineBytes = 256 * 1024
	// traceReadBufSize is the initial line-reader buffer; ReadBytes grows past it
	// for oversized lines.
	traceReadBufSize = 64 * 1024
)

// traceBridge couples a read-only trace stream to a WebSocket, one-directional
// (backend → browser). It reassembles complete NDJSON lines, forwards each as a
// text frame, drops oldest under backpressure with a cumulative dropped count
// surfaced via a trace_meta frame, and runs a read pump purely to notice the
// browser's close so the stream (and the remote tail behind it) never leaks.
//
// gorilla/websocket requires a single writer goroutine: the queue→WebSocket loop
// is that sole writer; the read pump only reads.
func traceBridge(ws wsConn, stream io.ReadCloser) {
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			_ = stream.Close()
			_ = ws.Close()
		})
	}

	var dropped atomic.Int64
	queue := make(chan []byte, traceQueueSize)

	// Read pump: the browser sends nothing on this channel but the close
	// handshake. Reading is the only way gorilla surfaces that close (and
	// services ping/pong); without it the close goes unnoticed (DESIGN §6).
	go func() {
		defer shutdown()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Stream → queue: reassemble lines and enqueue, dropping oldest when full.
	go func() {
		defer shutdown()
		defer close(queue)
		br := bufio.NewReaderSize(stream, traceReadBufSize)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
					if len(trimmed) > maxTraceLineBytes {
						dropped.Add(1)
					} else {
						enqueueDropOldest(queue, trimmed, &dropped)
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Queue → WebSocket: the sole writer. Each iteration first flushes a
	// trace_meta frame if the dropped count advanced, then the event line.
	defer shutdown()
	var lastReported int64
	for line := range queue {
		if d := dropped.Load(); d != lastReported {
			lastReported = d
			if meta, err := wire.EncodeTraceMeta(int(d)); err == nil {
				if err := ws.WriteMessage(wsText, meta); err != nil {
					return
				}
			}
		}
		if err := ws.WriteMessage(wsText, line); err != nil {
			return
		}
	}
}

// enqueueDropOldest sends line on q, evicting the oldest queued line and bumping
// dropped if q is full. It never blocks for long: a full queue is drained by one
// before the retry, so the send then succeeds.
func enqueueDropOldest(q chan []byte, line []byte, dropped *atomic.Int64) {
	for {
		select {
		case q <- line:
			return
		default:
			select {
			case <-q:
				dropped.Add(1)
			default:
			}
		}
	}
}
