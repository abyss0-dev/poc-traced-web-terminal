package runtime

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// errBadAuth is returned by the PoC test server (and conceptually by any sshd)
// when password authentication fails.
var errBadAuth = errors.New("authentication failed")

const (
	sshDialTimeout = 5 * time.Second
	// Initial PTY geometry; the browser sends a resize control frame
	// immediately after connecting, so this is only the pre-resize default.
	initialCols = 80
	initialRows = 24
)

// clientConfig builds the SSH client configuration for a target. Host-key
// verification is intentionally disabled for the PoC (see DESIGN §9).
func clientConfig(tc TargetConfig) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            tc.User,
		Auth:            []ssh.AuthMethod{ssh.Password(tc.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshDialTimeout,
	}
}

func addr(tc TargetConfig) string {
	return net.JoinHostPort(tc.Host, strconv.Itoa(tc.Port))
}

// sshProbe reports nil once the target's sshd accepts and authenticates a
// connection. It opens and immediately closes a connection, which is the PoC
// readiness signal (a forwarded TCP port accepts before sshd is ready, so a
// full handshake is required).
func sshProbe(tc TargetConfig) error {
	client, err := ssh.Dial("tcp", addr(tc), clientConfig(tc))
	if err != nil {
		return err
	}
	return client.Close()
}

// sshSession realises a transport-neutral Session over an SSH connection with
// an allocated PTY running an interactive shell.
type sshSession struct {
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.Writer
	stdout io.Reader
}

// sshDial opens an interactive SSH session to the target: it authenticates,
// allocates a PTY, and starts the login shell.
func sshDial(tc TargetConfig) (Session, error) {
	client, err := ssh.Dial("tcp", addr(tc), clientConfig(tc))
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr(tc), err)
	}

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("ssh new session: %w", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", initialRows, initialCols, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("ssh request pty: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}

	if err := sess.Shell(); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("ssh start shell: %w", err)
	}

	return &sshSession{client: client, sess: sess, stdin: stdin, stdout: stdout}, nil
}

// Read yields shell output (stdout, with stderr merged by the remote PTY).
func (s *sshSession) Read(p []byte) (int, error) { return s.stdout.Read(p) }

// Write delivers keystrokes to the shell's stdin.
func (s *sshSession) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Resize reflows the remote PTY to the given dimensions. SSH window-change
// carries (rows, cols) — i.e. height then width.
func (s *sshSession) Resize(cols, rows uint16) error {
	return s.sess.WindowChange(int(rows), int(cols))
}

// Close ends the shell session and tears down the SSH connection.
func (s *sshSession) Close() error {
	sessErr := s.sess.Close()
	clientErr := s.client.Close()
	if sessErr != nil && !errors.Is(sessErr, io.EOF) {
		return sessErr
	}
	return clientErr
}
