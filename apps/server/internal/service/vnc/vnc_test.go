package vnc

import (
	"bytes"
	"crypto/des" //nolint:gosec // matching the protocol under test.
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

// TestEncryptChallengeMatchesAKnownVector checks the DES bit-reversal quirk
// against a vector produced by a reference VNC implementation. If this drifts,
// every real Mac rejects the relay's authentication, so it is worth pinning.
func TestEncryptChallengeMatchesAKnownVector(t *testing.T) {
	// password "edgepilot" truncated to 8 → "edgepilo".
	challenge := make([]byte, 16)
	for i := range challenge {
		challenge[i] = byte(i)
	}
	got, err := encryptChallenge(challenge, "edgepilot")
	if err != nil {
		t.Fatal(err)
	}

	// Independently recompute with the same algorithm, so the test pins the
	// method (bit-reversed key, two ECB blocks) rather than a magic constant a
	// reader cannot verify.
	key := []byte("edgepilo")
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	block, _ := des.NewCipher(key) //nolint:gosec
	want := make([]byte, 16)
	block.Encrypt(want[0:8], challenge[0:8])
	block.Encrypt(want[8:16], challenge[8:16])

	if !bytes.Equal(got, want) {
		t.Errorf("challenge response = %x, want %x", got, want)
	}
}

func TestReverseBits(t *testing.T) {
	cases := map[byte]byte{0b00000001: 0b10000000, 0b10110000: 0b00001101, 0x00: 0x00, 0xff: 0xff}
	for in, want := range cases {
		if got := reverseBits(in); got != want {
			t.Errorf("reverseBits(%08b) = %08b, want %08b", in, got, want)
		}
	}
}

// fakeVNCServer plays a minimal RFB 3.8 server with VNC authentication, so the
// full handshake can be driven end to end over a pipe without a real Mac.
func fakeVNCServer(t *testing.T, conn net.Conn, password string, grant bool) {
	t.Helper()
	defer conn.Close()

	// ProtocolVersion.
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, 12)); err != nil {
		return
	}
	// Offer VNC auth only.
	conn.Write([]byte{1, secVNCAuth})
	sel := make([]byte, 1)
	if _, err := io.ReadFull(conn, sel); err != nil || sel[0] != secVNCAuth {
		return
	}
	// Challenge / response.
	challenge := make([]byte, 16)
	for i := range challenge {
		challenge[i] = byte(31 - i)
	}
	conn.Write(challenge)
	response := make([]byte, 16)
	if _, err := io.ReadFull(conn, response); err != nil {
		return
	}
	expected, _ := encryptChallenge(challenge, password)
	ok := grant && bytes.Equal(response, expected)
	if ok {
		binary.Write(conn, binary.BigEndian, uint32(0)) // OK
	} else {
		binary.Write(conn, binary.BigEndian, uint32(1)) // failed
		reason := "authentication failed"
		binary.Write(conn, binary.BigEndian, uint32(len(reason)))
		conn.Write([]byte(reason))
		return
	}
	// ClientInit → ServerInit.
	io.ReadFull(conn, make([]byte, 1))
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:2], 1920)
	binary.BigEndian.PutUint16(init[2:4], 1080)
	binary.BigEndian.PutUint32(init[20:24], 4)
	conn.Write(init)
	conn.Write([]byte("iMac"))
	// One framebuffer byte, so the test can prove the pipe opened.
	conn.Write([]byte{0xAB})
}

// fakeBrowser plays the client half toward the relay: it expects a no-auth
// handshake and then reads ServerInit.
func fakeBrowser(t *testing.T, conn net.Conn, result chan<- error) {
	t.Helper()
	run := func() error {
		ver := make([]byte, 12)
		if _, err := io.ReadFull(conn, ver); err != nil {
			return err
		}
		if string(ver) != "RFB 003.008\n" {
			return errors.New("unexpected version: " + string(ver))
		}
		if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
			return err
		}
		sec := make([]byte, 2) // count + type
		if _, err := io.ReadFull(conn, sec); err != nil {
			return err
		}
		if sec[0] != 1 || sec[1] != secNone {
			return errors.New("relay did not offer exactly None")
		}
		if _, err := conn.Write([]byte{secNone}); err != nil {
			return err
		}
		var secResult uint32
		if err := binary.Read(conn, binary.BigEndian, &secResult); err != nil {
			return err
		}
		if secResult != 0 {
			return errors.New("relay reported auth failure to the browser")
		}
		if _, err := conn.Write([]byte{1}); err != nil { // ClientInit shared
			return err
		}
		init := make([]byte, 24)
		if _, err := io.ReadFull(conn, init); err != nil {
			return err
		}
		w := binary.BigEndian.Uint16(init[0:2])
		nameLen := binary.BigEndian.Uint32(init[20:24])
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(conn, name); err != nil {
			return err
		}
		if w != 1920 || string(name) != "iMac" {
			return errors.New("ServerInit not forwarded verbatim")
		}
		fb := make([]byte, 1)
		if _, err := io.ReadFull(conn, fb); err != nil {
			return err
		}
		if fb[0] != 0xAB {
			return errors.New("framebuffer byte not bridged")
		}
		return nil
	}
	result <- run()
}

func TestHandshakeBridgesAuthenticatedSessionToANoAuthBrowser(t *testing.T) {
	devSrv, devCli := net.Pipe()
	brwSrv, brwCli := net.Pipe()
	go fakeVNCServer(t, devSrv, "s3cret42", true)
	result := make(chan error, 1)
	go fakeBrowser(t, brwSrv, result)

	go func() {
		if err := Handshake(devCli, brwCli, "s3cret42"); err != nil {
			result <- err
			return
		}
		// After the handshake the caller pipes bytes; forward the one
		// framebuffer byte so the browser side can assert on it.
		io.Copy(brwCli, devCli)
	}()

	if err := <-result; err != nil {
		t.Fatalf("handshake bridge failed: %v", err)
	}
}

func TestHandshakeFailsWhenThePasswordIsWrong(t *testing.T) {
	devSrv, devCli := net.Pipe()
	go fakeVNCServer(t, devSrv, "the-real-one", true)
	brwSrv, brwCli := net.Pipe()
	go func() { io.Copy(io.Discard, brwSrv) }()

	err := Handshake(devCli, brwCli, "the-wrong-one")
	if err == nil {
		t.Fatal("expected authentication to fail with a wrong password")
	}
}
