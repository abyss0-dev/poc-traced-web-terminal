package runtime

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Process is a launched backend machine that can be stopped. It abstracts the
// underlying OS process so the runtime lifecycle can be exercised without QEMU.
type Process interface {
	// Stop terminates the backend and releases its resources.
	Stop() error
}

// Options configures a QEMU runtime. Every function field has a production
// default (real QEMU launch, real SSH probe/dial); tests inject fakes. Zero
// durations fall back to PoC-appropriate defaults.
type Options struct {
	// Launch starts one backend machine. Default: launch a QEMU guest.
	Launch func(TargetConfig) (Process, error)
	// Probe reports nil once a target accepts interactive sessions.
	// Default: perform an SSH handshake and authenticate.
	Probe func(TargetConfig) error
	// Dial opens an interactive session to a target.
	// Default: SSH connection with an allocated PTY running a shell.
	Dial func(TargetConfig) (Session, error)
	// Trace opens the read-only BehaviorEvent stream for a target.
	// Default: dedicated non-PTY SSH connection tailing bloodhound's output.
	Trace func(TargetConfig, TraceConfig) (io.ReadCloser, error)
	// PollInterval is the readiness poll cadence. Default: 2s.
	PollInterval time.Duration
	// ReadyTimeout bounds how long a target may take to become ready.
	// Default: 60s.
	ReadyTimeout time.Duration
}

type targetState struct {
	cfg    TargetConfig
	status Status
	proc   Process
}

// QEMU is the QEMU implementation of Runtime. It owns the lifecycle of every
// configured guest and realises each Session as an SSH connection with a PTY.
type QEMU struct {
	launch       func(TargetConfig) (Process, error)
	probe        func(TargetConfig) error
	dial         func(TargetConfig) (Session, error)
	trace        func(TargetConfig, TraceConfig) (io.ReadCloser, error)
	traceCfg     TraceConfig
	pollInterval time.Duration
	readyTimeout time.Duration

	mu     sync.Mutex
	states []*targetState
	byID   map[string]*targetState

	wg       sync.WaitGroup
	done     chan struct{}
	stopOnce sync.Once
}

var _ Runtime = (*QEMU)(nil)

// NewQEMU builds a QEMU runtime from configuration, filling any unset Options
// fields with production defaults.
func NewQEMU(cfg Config, opts Options) *QEMU {
	if opts.Launch == nil {
		opts.Launch = launchQEMU
	}
	if opts.Probe == nil {
		opts.Probe = sshProbe
	}
	if opts.Dial == nil {
		opts.Dial = sshDial
	}
	if opts.Trace == nil {
		opts.Trace = sshTraceStream
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 60 * time.Second
	}

	q := &QEMU{
		launch:       opts.Launch,
		probe:        opts.Probe,
		dial:         opts.Dial,
		trace:        opts.Trace,
		traceCfg:     cfg.Trace,
		pollInterval: opts.PollInterval,
		readyTimeout: opts.ReadyTimeout,
		byID:         make(map[string]*targetState, len(cfg.Targets)),
		done:         make(chan struct{}),
	}
	for _, tc := range cfg.Targets {
		st := &targetState{cfg: tc, status: StatusBooting}
		q.states = append(q.states, st)
		q.byID[tc.ID] = st
	}
	return q
}

// Targets returns every configured target with its current status, in
// configuration order.
func (q *QEMU) Targets() []Target {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Target, 0, len(q.states))
	for _, st := range q.states {
		out = append(out, Target{ID: st.cfg.ID, Label: st.cfg.Label, Status: st.status})
	}
	return out
}

// EnsureStarted launches every configured target and begins readiness polling.
// A single target failing to launch is recorded as an error on that target and
// does not abort the others.
func (q *QEMU) EnsureStarted() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, st := range q.states {
		proc, err := q.launch(st.cfg)
		if err != nil {
			st.status = StatusError
			continue
		}
		st.proc = proc
		q.wg.Add(1)
		go q.pollReady(st)
	}
	return nil
}

// pollReady drives one target from booting to ready or error.
func (q *QEMU) pollReady(st *targetState) {
	defer q.wg.Done()
	deadline := time.Now().Add(q.readyTimeout)
	ticker := time.NewTicker(q.pollInterval)
	defer ticker.Stop()

	for {
		if err := q.probe(st.cfg); err == nil {
			q.setStatus(st, StatusReady)
			return
		}
		if time.Now().After(deadline) {
			q.setStatus(st, StatusError)
			return
		}
		select {
		case <-q.done:
			return
		case <-ticker.C:
		}
	}
}

func (q *QEMU) setStatus(st *targetState, s Status) {
	q.mu.Lock()
	st.status = s
	q.mu.Unlock()
}

// Attach opens an interactive session to a ready target. Attaching to an
// unknown or not-yet-ready target is an error.
func (q *QEMU) Attach(id string) (Session, error) {
	q.mu.Lock()
	st, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return nil, fmt.Errorf("attach: unknown target %q", id)
	}
	status := st.status
	cfg := st.cfg
	q.mu.Unlock()

	if status != StatusReady {
		return nil, fmt.Errorf("attach: target %q is %s, not ready", id, status)
	}
	return q.dial(cfg)
}

// TraceStream opens the read-only BehaviorEvent stream for a ready target. It is
// independent of any interactive session, so the trace pane works whether or not
// a terminal is attached. It is refused when tracing is disabled, the target is
// unknown, or the target is not yet ready.
func (q *QEMU) TraceStream(id string) (io.ReadCloser, error) {
	q.mu.Lock()
	st, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return nil, fmt.Errorf("trace: unknown target %q", id)
	}
	status := st.status
	cfg := st.cfg
	q.mu.Unlock()

	if !q.traceCfg.Enabled {
		return nil, fmt.Errorf("trace: not enabled for this runtime")
	}
	if status != StatusReady {
		return nil, fmt.Errorf("trace: target %q is %s, not ready", id, status)
	}
	return q.trace(cfg, q.traceCfg)
}

// Shutdown stops readiness polling and terminates every launched target. It is
// safe to call more than once.
func (q *QEMU) Shutdown() error {
	q.stopOnce.Do(func() { close(q.done) })
	q.wg.Wait()

	q.mu.Lock()
	procs := make([]Process, 0, len(q.states))
	for _, st := range q.states {
		if st.proc != nil {
			procs = append(procs, st.proc)
			st.proc = nil
		}
	}
	q.mu.Unlock()

	var firstErr error
	for _, p := range procs {
		if err := p.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
