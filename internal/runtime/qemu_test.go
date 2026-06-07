package runtime

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test doubles -----------------------------------------------------------

type fakeProcess struct {
	stopped atomic.Bool
}

func (p *fakeProcess) Stop() error {
	p.stopped.Store(true)
	return nil
}

type nopSession struct{}

func (nopSession) Read([]byte) (int, error)    { return 0, nil }
func (nopSession) Write(b []byte) (int, error) { return len(b), nil }
func (nopSession) Close() error                { return nil }
func (nopSession) Resize(uint16, uint16) error { return nil }

func testConfig() Config {
	return Config{
		Runtime: "qemu",
		Targets: []TargetConfig{
			{ID: "vm1", Label: "VM 1", Host: "127.0.0.1", Port: 2222, User: "poc"},
			{ID: "vm2", Label: "VM 2", Host: "127.0.0.1", Port: 2223, User: "poc"},
		},
	}
}

// waitForStatus polls until the target reaches want or the deadline passes.
func waitForStatus(t *testing.T, r Runtime, id string, want Status) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, tg := range r.Targets() {
			if tg.ID == id && tg.Status == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("target %q never reached status %q; got %+v", id, want, r.Targets())
}

// --- tests ------------------------------------------------------------------

func TestTargetsReflectConfigOrderAndStartBooting(t *testing.T) {
	r := NewQEMU(testConfig(), Options{})
	got := r.Targets()
	if len(got) != 2 {
		t.Fatalf("len(Targets) = %d, want 2", len(got))
	}
	if got[0].ID != "vm1" || got[1].ID != "vm2" {
		t.Fatalf("order not preserved: %+v", got)
	}
	if got[0].Label != "VM 1" {
		t.Fatalf("label = %q, want %q", got[0].Label, "VM 1")
	}
	for _, tg := range got {
		if tg.Status != StatusBooting {
			t.Fatalf("target %q initial status = %q, want booting", tg.ID, tg.Status)
		}
	}
}

func TestEnsureStartedLaunchesEveryTarget(t *testing.T) {
	var launched int32
	r := NewQEMU(testConfig(), Options{
		Launch: func(TargetConfig) (Process, error) {
			atomic.AddInt32(&launched, 1)
			return &fakeProcess{}, nil
		},
		Probe:        func(TargetConfig) error { return nil },
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if got := atomic.LoadInt32(&launched); got != 2 {
		t.Fatalf("launched %d processes, want 2", got)
	}
}

func TestSuccessfulProbeTransitionsToReady(t *testing.T) {
	r := NewQEMU(testConfig(), Options{
		Launch:       func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:        func(TargetConfig) error { return nil },
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitForStatus(t, r, "vm1", StatusReady)
	waitForStatus(t, r, "vm2", StatusReady)
}

func TestFailingProbeTransitionsToErrorAfterTimeout(t *testing.T) {
	r := NewQEMU(testConfig(), Options{
		Launch:       func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:        func(TargetConfig) error { return errors.New("refused") },
		PollInterval: time.Millisecond,
		ReadyTimeout: 20 * time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitForStatus(t, r, "vm1", StatusError)
}

func TestLaunchFailureMarksTargetError(t *testing.T) {
	r := NewQEMU(testConfig(), Options{
		Launch: func(tc TargetConfig) (Process, error) {
			if tc.ID == "vm1" {
				return nil, errors.New("boom")
			}
			return &fakeProcess{}, nil
		},
		Probe:        func(TargetConfig) error { return nil },
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted should not fail wholesale: %v", err)
	}
	waitForStatus(t, r, "vm1", StatusError)
	waitForStatus(t, r, "vm2", StatusReady)
}

func TestAttachReadyTargetDialsCorrectConfig(t *testing.T) {
	var mu sync.Mutex
	var dialedID string
	r := NewQEMU(testConfig(), Options{
		Launch: func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:  func(TargetConfig) error { return nil },
		Dial: func(tc TargetConfig) (Session, error) {
			mu.Lock()
			dialedID = tc.ID
			mu.Unlock()
			return nopSession{}, nil
		},
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitForStatus(t, r, "vm2", StatusReady)

	sess, err := r.Attach("vm2")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if sess == nil {
		t.Fatal("Attach returned nil session")
	}
	mu.Lock()
	defer mu.Unlock()
	if dialedID != "vm2" {
		t.Fatalf("dialed %q, want vm2", dialedID)
	}
}

func TestAttachUnknownTargetErrors(t *testing.T) {
	r := NewQEMU(testConfig(), Options{
		Launch: func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:  func(TargetConfig) error { return nil },
		Dial:   func(TargetConfig) (Session, error) { return nopSession{}, nil },
	})
	if _, err := r.Attach("nope"); err == nil {
		t.Fatal("expected error attaching unknown target")
	}
}

func TestAttachNotReadyTargetErrors(t *testing.T) {
	r := NewQEMU(testConfig(), Options{
		Launch: func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:  func(TargetConfig) error { return errors.New("never") },
		Dial:   func(TargetConfig) (Session, error) { return nopSession{}, nil },
	})
	// No EnsureStarted: targets are still booting.
	if _, err := r.Attach("vm1"); err == nil {
		t.Fatal("expected error attaching non-ready target")
	}
}

// fakeTrace is an io.ReadCloser standing in for the remote tail.
type fakeTrace struct{ closed atomic.Bool }

func (*fakeTrace) Read(p []byte) (int, error) { return 0, nil }
func (f *fakeTrace) Close() error             { f.closed.Store(true); return nil }

func traceTestConfig() Config {
	c := testConfig()
	c.Trace = TraceConfig{
		Enabled:     true,
		OperatorUID: 1000,
		Watcher:     WatcherConfig{User: "watcher", Password: "watch"},
	}
	return c
}

func TestTraceStreamOpensForReadyTarget(t *testing.T) {
	var mu sync.Mutex
	var tracedID string
	r := NewQEMU(traceTestConfig(), Options{
		Launch: func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:  func(TargetConfig) error { return nil },
		Trace: func(tc TargetConfig, _ TraceConfig) (io.ReadCloser, error) {
			mu.Lock()
			tracedID = tc.ID
			mu.Unlock()
			return &fakeTrace{}, nil
		},
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitForStatus(t, r, "vm1", StatusReady)

	rc, err := r.TraceStream("vm1")
	if err != nil {
		t.Fatalf("TraceStream: %v", err)
	}
	if rc == nil {
		t.Fatal("TraceStream returned nil stream")
	}
	mu.Lock()
	defer mu.Unlock()
	if tracedID != "vm1" {
		t.Fatalf("traced %q, want vm1", tracedID)
	}
}

func TestTraceStreamRefusedWhenDisabled(t *testing.T) {
	cfg := testConfig() // Trace.Enabled defaults to false
	r := NewQEMU(cfg, Options{
		Launch:       func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:        func(TargetConfig) error { return nil },
		Trace:        func(TargetConfig, TraceConfig) (io.ReadCloser, error) { return &fakeTrace{}, nil },
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitForStatus(t, r, "vm1", StatusReady)
	if _, err := r.TraceStream("vm1"); err == nil {
		t.Fatal("expected TraceStream to be refused when trace is disabled")
	}
}

func TestTraceStreamNotReadyTargetErrors(t *testing.T) {
	r := NewQEMU(traceTestConfig(), Options{
		Launch: func(TargetConfig) (Process, error) { return &fakeProcess{}, nil },
		Probe:  func(TargetConfig) error { return errors.New("never") },
		Trace:  func(TargetConfig, TraceConfig) (io.ReadCloser, error) { return &fakeTrace{}, nil },
	})
	// No EnsureStarted: targets are still booting.
	if _, err := r.TraceStream("vm1"); err == nil {
		t.Fatal("expected error tracing a non-ready target")
	}
}

func TestShutdownStopsEveryLaunchedProcess(t *testing.T) {
	var procs []*fakeProcess
	var mu sync.Mutex
	r := NewQEMU(testConfig(), Options{
		Launch: func(TargetConfig) (Process, error) {
			p := &fakeProcess{}
			mu.Lock()
			procs = append(procs, p)
			mu.Unlock()
			return p, nil
		},
		Probe:        func(TargetConfig) error { return nil },
		PollInterval: time.Millisecond,
	})
	if err := r.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitForStatus(t, r, "vm1", StatusReady)
	if err := r.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(procs) != 2 {
		t.Fatalf("launched %d processes, want 2", len(procs))
	}
	for i, p := range procs {
		if !p.stopped.Load() {
			t.Fatalf("process %d not stopped", i)
		}
	}
}
