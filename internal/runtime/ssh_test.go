package runtime

import (
	"bufio"
	"crypto/ed25519"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testSSHServer is a minimal in-process sshd: password auth, a single session
// channel that allocates a (fake) PTY, echoes stdin to stdout, and reports the
// dimensions of any window-change request on winCh.
type testSSHServer struct {
	addr  string
	user  string
	pass  string
	winCh chan [2]uint16 // {cols, rows}
	ln    net.Listener
}

func startTestSSHServer(t *testing.T, user, pass string) *testSSHServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(p) == pass {
				return nil, nil
			}
			return nil, errBadAuth
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &testSSHServer{
		addr:  ln.Addr().String(),
		user:  user,
		pass:  pass,
		winCh: make(chan [2]uint16, 8),
		ln:    ln,
	}
	go s.serve(cfg)
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *testSSHServer) serve(cfg *ssh.ServerConfig) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn, cfg)
	}
}

func (s *testSSHServer) handleConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go s.handleChannel(ch, chReqs)
	}
}

func (s *testSSHServer) handleChannel(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			go func() {
				_, _ = io.Copy(ch, ch) // echo
				_ = ch.Close()
			}()
		case "window-change":
			cols := binary.BigEndian.Uint32(req.Payload[0:4])
			rows := binary.BigEndian.Uint32(req.Payload[4:8])
			s.winCh <- [2]uint16{uint16(cols), uint16(rows)}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func tcFor(s *testSSHServer) TargetConfig {
	host, port := splitHostPort(s.addr)
	return TargetConfig{ID: "t", Host: host, Port: port, User: s.user, Password: s.pass}
}

func splitHostPort(addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	var port int
	for _, c := range p {
		port = port*10 + int(c-'0')
	}
	return h, port
}

// --- tests ------------------------------------------------------------------

func TestSSHDialEchoRoundTrip(t *testing.T) {
	srv := startTestSSHServer(t, "poc", "secret")
	sess, err := sshDial(tcFor(srv))
	if err != nil {
		t.Fatalf("sshDial: %v", err)
	}
	defer sess.Close()

	if _, err := sess.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	r := bufio.NewReader(sess)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if line != "hello\n" {
		t.Fatalf("echo = %q, want %q", line, "hello\n")
	}
}

func TestSSHResizeSendsWindowChange(t *testing.T) {
	srv := startTestSSHServer(t, "poc", "secret")
	sess, err := sshDial(tcFor(srv))
	if err != nil {
		t.Fatalf("sshDial: %v", err)
	}
	defer sess.Close()

	if err := sess.Resize(132, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	select {
	case got := <-srv.winCh:
		if got != [2]uint16{132, 50} {
			t.Fatalf("window-change = %v, want {132 50}", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received window-change")
	}
}

func TestSSHProbeSucceedsWithValidCredentials(t *testing.T) {
	srv := startTestSSHServer(t, "poc", "secret")
	if err := sshProbe(tcFor(srv)); err != nil {
		t.Fatalf("sshProbe: %v", err)
	}
}

func TestSSHProbeFailsWithBadCredentials(t *testing.T) {
	srv := startTestSSHServer(t, "poc", "secret")
	tc := tcFor(srv)
	tc.Password = "wrong"
	if err := sshProbe(tc); err == nil {
		t.Fatal("expected probe to fail with bad credentials")
	}
}
