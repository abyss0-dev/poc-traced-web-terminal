// Package runtime defines the transport-neutral Runtime and Session contract
// that the Gateway depends on, together with a QEMU implementation that
// realises a Session as an SSH connection with an allocated PTY.
//
// The contract has two responsibilities — lifecycle (launch and stop backend
// machines) and session (open an interactive byte stream to one machine). A
// future Kata / Kubernetes / cloud implementation can satisfy the same
// interfaces without changing the Gateway, the BFF, or the wire protocol.
package runtime

import "io"

// Status is the lifecycle state of a single target as observed by the Runtime.
type Status string

const (
	// StatusBooting means the backend has been launched but is not yet
	// accepting interactive sessions.
	StatusBooting Status = "booting"
	// StatusReady means the backend is accepting interactive sessions.
	StatusReady Status = "ready"
	// StatusError means the backend failed to launch or never became ready.
	StatusError Status = "error"
)

// Target is the browser-facing view of one backend machine. It deliberately
// omits connection details and credentials: only an opaque id, a human label,
// and a live status cross toward the BFF.
type Target struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status Status `json:"status"`
}

// Session is a transport-neutral interactive session: a bidirectional byte
// stream (Read yields shell output, Write delivers keystrokes) plus a Resize
// operation that reflows the remote PTY. Close releases the session.
type Session interface {
	io.ReadWriteCloser
	// Resize reconciles the remote PTY dimensions with the browser viewport.
	Resize(cols, rows uint16) error
}

// Runtime owns the lifecycle of every configured target and hands out
// interactive sessions on demand.
type Runtime interface {
	// Targets returns every configured target with its current status.
	Targets() []Target
	// EnsureStarted launches all targets and begins readiness checks.
	EnsureStarted() error
	// Attach opens an interactive session to the target with the given id.
	Attach(id string) (Session, error)
	// TraceStream opens a read-only stream of the target's live BehaviorEvent
	// NDJSON lines. It is the observation analogue of Attach: where Attach yields
	// a bidirectional Session, TraceStream yields a read-closable line stream
	// that carries no credentials and no control frames toward the caller.
	// Closing it tears down the underlying connection and the remote tail.
	TraceStream(id string) (io.ReadCloser, error)
	// Shutdown stops every launched target and releases resources.
	Shutdown() error
}
