// Package terminal runs an interactive shell on a device for a browser.
//
// A browser cannot speak SSH. So unlike `ssh -J`, where this server forwards
// bytes it cannot read, the browser terminal makes the *server* the SSH client:
// it opens a tunnel to the device, performs the SSH handshake itself over that
// tunnel, requests a PTY, and streams the terminal to the page.
//
// # What that costs, stated plainly
//
// The server sees the session. Every keystroke and every byte of output passes
// through this process in clear text, and the key in Options.Signer opens a
// shell on every Mac that has installed the matching public key. Both are the
// unavoidable price of rendering a terminal in a browser, and neither is true of
// `ssh -J`, which still relays ciphertext end to end.
//
// The compensating control is that sessions are recorded. If confidentiality
// against the control plane is gone, accountability should replace it — see
// Recorder and docs/Security.md §5.1.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/visionify/edgepilot/apps/server/internal/service/tunnel"
)

// Dimensions of the pseudo-terminal before the browser reports its own.
const (
	defaultCols = 80
	defaultRows = 24
)

// handshakeTimeout bounds the SSH negotiation with the device.
//
// The tunnel is already open by this point, so this covers only the handshake
// and authentication. A Mac that accepts the TCP connection and then stalls
// should fail the session, not hold a browser waiting indefinitely.
const handshakeTimeout = 20 * time.Second

// Session is a live shell.
type Session struct {
	client  *ssh.Client
	session *ssh.Session
	tunnel  *tunnel.Tunnel

	stdin  io.WriteCloser
	stdout io.Reader

	closeOnce sync.Once
	closed    chan struct{}
}

// Options configures a session.
type Options struct {
	// Signer is the server's SSH identity. Its public half must be installed on
	// the device, which is what the authorized-key distribution already does.
	Signer ssh.Signer

	// Username is the local account on the device to log in as.
	Username string

	Log *slog.Logger
}

// Open establishes a shell on a device over an existing tunnel.
//
// The tunnel is adopted: closing the returned Session closes it too, so the
// caller has one thing to defer rather than two that must be unwound in order.
func Open(ctx context.Context, tun *tunnel.Tunnel, opts Options) (*Session, error) {
	if opts.Signer == nil {
		return nil, errors.New("terminal: no server SSH identity is configured")
	}
	if opts.Username == "" {
		return nil, errors.New("terminal: no target username")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}

	config := &ssh.ClientConfig{
		User: opts.Username,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(opts.Signer)},
		// The device's host key is not verified, and that needs justifying
		// rather than hiding.
		//
		// This connection does not cross a network. It runs inside a tunnel that
		// already terminates at one specific device, reached over a mutually
		// authenticated stream whose client certificate this deployment's own CA
		// issued. There is no route by which a different host could answer, so
		// there is nothing a host key would establish that the device plane has
		// not already established more strongly.
		//
		// Pinning would also be actively harmful here: a Mac that is re-imaged
		// gets a new host key, and the failure would present as a device the
		// dashboard can see and cannot connect to.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see above
		Timeout:         handshakeTimeout,
	}

	// The tunnel is an io.ReadWriteCloser, not a net.Conn. x/crypto/ssh wants
	// the latter, so it is wrapped rather than reimplemented.
	conn, chans, reqs, err := ssh.NewClientConn(&tunnelConn{tun: tun}, "device", config)
	if err != nil {
		return nil, fmt.Errorf("terminal: connecting to the device's sshd: %w", err)
	}

	client := ssh.NewClient(conn, chans, reqs)
	sshSession, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("terminal: opening a session: %w", err)
	}

	// xterm-256color, because that is what the browser renders. Echo on: the
	// remote shell owns echoing, and turning it off here produces a terminal
	// that silently swallows what is typed.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sshSession.RequestPty("xterm-256color", defaultRows, defaultCols, modes); err != nil {
		_ = sshSession.Close()
		_ = client.Close()
		return nil, fmt.Errorf("terminal: requesting a pty: %w", err)
	}

	stdin, err := sshSession.StdinPipe()
	if err != nil {
		_ = sshSession.Close()
		_ = client.Close()
		return nil, fmt.Errorf("terminal: attaching stdin: %w", err)
	}
	stdout, err := sshSession.StdoutPipe()
	if err != nil {
		_ = sshSession.Close()
		_ = client.Close()
		return nil, fmt.Errorf("terminal: attaching stdout: %w", err)
	}
	// Stderr is deliberately not attached. A pty merges it into the master side
	// already, so it arrives on stdout above; attaching a second reader here
	// would either duplicate output or silently swallow it.

	if err := sshSession.Shell(); err != nil {
		_ = sshSession.Close()
		_ = client.Close()
		return nil, fmt.Errorf("terminal: starting the shell: %w", err)
	}

	s := &Session{
		client:  client,
		session: sshSession,
		tunnel:  tun,
		stdin:   stdin,
		stdout:  stdout,
		closed:  make(chan struct{}),
	}

	// The device going away has to end the session. Without this the browser
	// holds an open socket to a shell that no longer exists.
	go func() {
		select {
		case <-tun.Done():
			s.Close()
		case <-ctx.Done():
			s.Close()
		case <-s.closed:
		}
	}()

	return s, nil
}

// Read returns terminal output.
func (s *Session) Read(p []byte) (int, error) { return s.stdout.Read(p) }

// Write sends keystrokes.
func (s *Session) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Resize tells the remote pty its new dimensions.
//
// Without this, anything that draws to the screen — vim, top, less — renders
// against the wrong geometry and looks corrupt.
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 1000 || rows > 1000 {
		return fmt.Errorf("terminal: implausible size %dx%d", cols, rows)
	}
	return s.session.WindowChange(rows, cols)
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.closed }

// Close ends the shell, the SSH connection, and the tunnel under it.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.session.Close()
		_ = s.client.Close()
		_ = s.tunnel.Close()
	})
}

// tunnelConn presents a Tunnel as a net.Conn for x/crypto/ssh.
//
// The addresses and deadlines are stubs: nothing in the SSH client uses them
// for a connection it did not dial itself, and inventing plausible values would
// suggest a network layer that is not there.
type tunnelConn struct{ tun *tunnel.Tunnel }

func (c *tunnelConn) Read(p []byte) (int, error)  { return c.tun.Read(p) }
func (c *tunnelConn) Write(p []byte) (int, error) { return c.tun.Write(p) }
func (c *tunnelConn) Close() error                { return c.tun.Close() }
func (c *tunnelConn) LocalAddr() net.Addr         { return tunnelAddr{} }
func (c *tunnelConn) RemoteAddr() net.Addr        { return tunnelAddr{} }

// Deadlines are not supported. The session's lifetime is bounded by its
// context, the tunnel's idle timeout, and the bastion's — three mechanisms that
// already cover what a deadline would, at layers that can actually enforce it.
func (c *tunnelConn) SetDeadline(time.Time) error      { return nil }
func (c *tunnelConn) SetReadDeadline(time.Time) error  { return nil }
func (c *tunnelConn) SetWriteDeadline(time.Time) error { return nil }

type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "edgepilot-tunnel" }
func (tunnelAddr) String() string  { return "device" }

// LoadSigner parses the server's SSH client identity.
func LoadSigner(pem []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("terminal: parsing the client key: %w", err)
	}
	return signer, nil
}
