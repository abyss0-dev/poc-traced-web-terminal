package runtime

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// traceKeepaliveInterval bounds how often the GW pokes the long-lived trace
	// connection. A periodic keepalive@openssh.com keeps NAT/SLIRP state warm and
	// actively detects a dead peer, which x/crypto/ssh does not do on its own.
	traceKeepaliveInterval = 30 * time.Second
)

// traceHealth is the observed state of the in-guest tracer at stream-open time.
// It distinguishes a tracer that is merely stopped (an empty stream is the
// correct, graceful outcome) from one that is running but blind — the silent
// offset-drift failure mode that file growth alone would hide (DESIGN §10).
type traceHealth int

const (
	traceHealthy traceHealth = iota // daemon active and task_struct offsets resolved
	traceAbsent                     // daemon not running: empty stream, terminal unaffected
	traceBlind                      // daemon running but offset drift silenced task-scoped capture
)

// watcherClientConfig builds the SSH client configuration for the read-only
// trace connection. It authenticates as the dedicated watcher account, whose
// login uid differs from the operator's, so the kernel auid filter excludes the
// reader and the observer feedback loop never starts (DESIGN §9).
func watcherClientConfig(trace TraceConfig) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            trace.Watcher.User,
		Auth:            []ssh.AuthMethod{ssh.Password(trace.Watcher.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshDialTimeout,
	}
}

// remoteExitZero runs cmd over the connection and reports whether it exited 0.
// A non-zero exit is a normal (false, nil) answer; only a failure to run the
// command at all is an error.
func remoteExitZero(client *ssh.Client, cmd string) (bool, error) {
	sess, err := client.NewSession()
	if err != nil {
		return false, err
	}
	defer sess.Close()
	if err := sess.Run(cmd); err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// checkTraceHealth is the automatic trace-readiness gate (DESIGN §10): over the
// watcher connection it asks whether bloodhound is active and, if so, whether
// its journal shows the resolved-offsets line. It fails open — an
// indeterminate check returns traceHealthy — so the gate can never become a new
// silent failure of its own; only a definitively blind tracer is reported.
func checkTraceHealth(client *ssh.Client) traceHealth {
	active, err := remoteExitZero(client, "systemctl is-active --quiet bloodhound")
	if err != nil {
		return traceHealthy
	}
	if !active {
		return traceAbsent
	}
	resolved, err := remoteExitZero(client,
		`sh -c "journalctl -u bloodhound --no-pager -q | grep -qm1 'Resolved task_struct offsets'"`)
	if err != nil {
		return traceHealthy
	}
	if resolved {
		return traceHealthy
	}
	return traceBlind
}

// sshTraceStream opens the read-only trace stream for one target: it dials a
// dedicated SSH connection as the watcher account (independent of any PTY
// session), runs the health gate, then tails bloodhound's NDJSON output without
// allocating a PTY. The returned io.ReadCloser is the read-only analogue of a
// Session; closing it reaps the remote tail and tears down the connection.
func sshTraceStream(tc TargetConfig, trace TraceConfig) (io.ReadCloser, error) {
	client, err := ssh.Dial("tcp", addr(tc), watcherClientConfig(trace))
	if err != nil {
		return nil, fmt.Errorf("trace ssh dial %s: %w", addr(tc), err)
	}

	switch checkTraceHealth(client) {
	case traceBlind:
		_ = client.Close()
		return nil, fmt.Errorf("trace %q: bloodhound is running but blind "+
			"(no 'Resolved task_struct offsets' in journal — likely offset drift, §10)", tc.ID)
	case traceAbsent:
		slog.Warn("trace: bloodhound not active; stream will be empty",
			slog.String("target", tc.ID))
	}

	sess, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("trace ssh session: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, fmt.Errorf("trace stdout pipe: %w", err)
	}

	// No PTY: a plain exec keeps lines '\n'-terminated, where a PTY would inject
	// CRLF translation and echo. tail -F -n0 follows from the current end, so the
	// subscriber sees events from subscription time forward, and -F survives
	// rotation. `exec` makes tail the session's process directly, so SSH session
	// close on Close() delivers SIGHUP straight to it (DESIGN §5).
	cmd := fmt.Sprintf("exec tail -F -n0 %s", shellQuote(trace.LogPathOrDefault()))
	if err := sess.Start(cmd); err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, fmt.Errorf("trace start tail: %w", err)
	}

	ts := &sshTrace{client: client, sess: sess, stdout: stdout, done: make(chan struct{})}
	go ts.keepalive()
	return ts, nil
}

// sshTrace is the read-only trace stream over an SSH connection.
type sshTrace struct {
	client    *ssh.Client
	sess      *ssh.Session
	stdout    io.Reader
	done      chan struct{}
	closeOnce sync.Once
}

// Read yields raw NDJSON bytes from the remote tail.
func (t *sshTrace) Read(p []byte) (int, error) { return t.stdout.Read(p) }

// Close stops the keepalive, closes the session (which reaps the remote tail via
// SIGHUP), and tears down the connection. It is safe to call more than once.
func (t *sshTrace) Close() error {
	t.closeOnce.Do(func() { close(t.done) })
	_ = t.sess.Close()
	return t.client.Close()
}

// keepalive pokes the connection periodically so a dead peer is detected
// promptly; on failure it tears the stream down so the GW surfaces the close.
func (t *sshTrace) keepalive() {
	ticker := time.NewTicker(traceKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			if _, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = t.Close()
				return
			}
		}
	}
}

// shellQuote wraps a string in single quotes for safe use in a remote command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
