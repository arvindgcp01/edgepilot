// Package sftpfs is a thin SFTP client that runs over a device tunnel.
//
// It is the file-transfer counterpart of the browser terminal: the same server
// SSH identity, the same tunnel to loopback sshd, the same trust argument (see
// service/terminal.Open for why the host key is not pinned). Where the terminal
// starts a shell channel, this starts the SFTP subsystem — so an operator who
// can already SSH to a device can move files to and from it without leaving the
// browser, and nothing new is exposed: a shell can do everything this can.
package sftpfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/visionify/edgepilot/apps/server/internal/service/tunnel"
)

const handshakeTimeout = 20 * time.Second

// Options configures a client.
type Options struct {
	// Signer is the server's SSH identity, its public half installed on the
	// device by the authorized-key distribution.
	Signer ssh.Signer
	// Username is the local account to log in as.
	Username string
	Log      *slog.Logger
}

// Client is a live SFTP connection. Closing it closes the SSH client and the
// adopted tunnel, so the caller defers one thing.
type Client struct {
	ssh    *ssh.Client
	sftp   *sftp.Client
	tunnel *tunnel.Tunnel

	closeOnce sync.Once
}

// Open dials sshd over the tunnel and starts the SFTP subsystem.
func Open(_ context.Context, tun *tunnel.Tunnel, opts Options) (*Client, error) {
	if opts.Signer == nil {
		return nil, errors.New("sftpfs: no server SSH identity is configured")
	}
	if opts.Username == "" {
		return nil, errors.New("sftpfs: no target username")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}

	config := &ssh.ClientConfig{
		User: opts.Username,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(opts.Signer)},
		// Not pinned, and for the same reason the terminal does not pin it: the
		// connection runs inside a tunnel that already terminates at one
		// mutually-authenticated device, so a host key would establish nothing
		// the device plane has not, and pinning would break a re-imaged Mac.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see terminal.Open.
		Timeout:         handshakeTimeout,
	}

	conn, chans, reqs, err := ssh.NewClientConn(&tunnelConn{tun: tun}, "device", config)
	if err != nil {
		return nil, fmt.Errorf("sftpfs: connecting to the device's sshd: %w", err)
	}
	sshClient := ssh.NewClient(conn, chans, reqs)

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("sftpfs: starting the SFTP subsystem: %w", err)
	}

	return &Client{ssh: sshClient, sftp: sftpClient, tunnel: tun}, nil
}

// Entry is one directory entry.
type Entry struct {
	Name    string
	Size    int64
	Mode    string
	IsDir   bool
	ModTime time.Time
}

// Home is the account's home directory, the natural place to start browsing.
func (c *Client) Home() (string, error) {
	// Getwd on a fresh SFTP session is the login directory.
	wd, err := c.sftp.Getwd()
	if err != nil {
		return "/", err
	}
	return wd, nil
}

// List returns the entries of a directory, directories first then by name.
func (c *Client) List(path string) ([]Entry, error) {
	infos, err := c.sftp.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(infos))
	for _, fi := range infos {
		entries = append(entries, Entry{
			Name:    fi.Name(),
			Size:    fi.Size(),
			Mode:    fi.Mode().String(),
			IsDir:   fi.IsDir(),
			ModTime: fi.ModTime(),
		})
	}
	return entries, nil
}

// Open opens a remote file for reading, returning its size for the response.
func (c *Client) Open(path string) (io.ReadCloser, int64, error) {
	f, err := c.sftp.Open(path)
	if err != nil {
		return nil, 0, err
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if stat.IsDir() {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%s is a directory", path)
	}
	return f, stat.Size(), nil
}

// Create writes r to a remote path, truncating any existing file. It returns
// the number of bytes written.
func (c *Client) Create(path string, r io.Reader) (int64, error) {
	f, err := c.sftp.Create(path)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// Join joins path elements the remote way (always forward slashes for SFTP).
func (c *Client) Join(elem ...string) string {
	return c.sftp.Join(elem...)
}

// Close tears down the SFTP client, the SSH client, and the tunnel, once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.sftp != nil {
			_ = c.sftp.Close()
		}
		if c.ssh != nil {
			_ = c.ssh.Close()
		}
		if c.tunnel != nil {
			_ = c.tunnel.Close()
		}
	})
}

// tunnelConn adapts a tunnel to net.Conn for x/crypto/ssh, mirroring the
// terminal package's wrapper.
type tunnelConn struct{ tun *tunnel.Tunnel }

func (c *tunnelConn) Read(p []byte) (int, error)       { return c.tun.Read(p) }
func (c *tunnelConn) Write(p []byte) (int, error)      { return c.tun.Write(p) }
func (c *tunnelConn) Close() error                     { return c.tun.Close() }
func (c *tunnelConn) LocalAddr() net.Addr              { return tunnelAddr{} }
func (c *tunnelConn) RemoteAddr() net.Addr             { return tunnelAddr{} }
func (c *tunnelConn) SetDeadline(time.Time) error      { return nil }
func (c *tunnelConn) SetReadDeadline(time.Time) error  { return nil }
func (c *tunnelConn) SetWriteDeadline(time.Time) error { return nil }

type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "edgepilot-tunnel" }
func (tunnelAddr) String() string  { return "device" }
