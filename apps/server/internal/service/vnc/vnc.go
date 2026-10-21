// Package vnc terminates VNC authentication so the browser never needs the
// password.
//
// # The problem
//
// noVNC in the browser speaks the RFB protocol and would do its own
// authentication — which means it would need the device's VNC password, the one
// thing an operator must never see. So the relay stands in the middle. It
// completes the real authentication handshake with the device using the sealed
// password, then presents the browser a handshake that says "no authentication
// required" and bridges the two. From the first framebuffer byte onward it is a
// dumb pipe; only the handshake is inspected.
//
// This is a deliberate man-in-the-middle, and it is sound here for the same
// reason the browser terminal is: the server is already the trusted relay
// between an authenticated operator and a device it manages. The compensating
// control is the audit record the caller writes around this.
//
// # References
//
// RFB 3.8, RFC 6143. The VNC authentication DES quirk (§7.2.2) — the password
// is used as a DES key with the bit order of each byte reversed — is the one
// piece that is not obvious from the RFC and is preserved from the original
// AT&T implementation.
package vnc

import (
	"crypto/des" //nolint:gosec // VNC authentication is DES by protocol definition; not our choice.
	"encoding/binary"
	"fmt"
	"io"
)

// Security types (RFB §7.1.2).
const (
	secInvalid = 0
	secNone    = 1
	secVNCAuth = 2
)

// version is the RFB version the relay speaks to both sides. 3.8 is universal
// and is what macOS Screen Sharing offers.
var version = []byte("RFB 003.008\n")

// Handshake authenticates to the device and presents the browser a no-auth
// handshake, leaving both connections positioned at the start of the
// framebuffer stream. After it returns nil, the caller bridges device and
// browser as raw bytes.
//
// device is the tunnel to the Mac's VNC server; browser is the operator's
// WebSocket, adapted to a byte stream. Both are read and written in lockstep
// here, so neither should be being copied by anything else yet.
func Handshake(device io.ReadWriter, browser io.ReadWriter, password string) error {
	if err := authenticateToDevice(device, password); err != nil {
		return err
	}
	if err := presentNoAuthToBrowser(browser); err != nil {
		return fmt.Errorf("vnc: browser handshake: %w", err)
	}
	if err := bridgeInit(device, browser); err != nil {
		return fmt.Errorf("vnc: init exchange: %w", err)
	}
	return nil
}

// authenticateToDevice performs the client half of the RFB handshake against
// the device: version, security negotiation, VNC authentication, result.
func authenticateToDevice(device io.ReadWriter, password string) error {
	// 1. ProtocolVersion. The server speaks first.
	serverVersion := make([]byte, 12)
	if _, err := io.ReadFull(device, serverVersion); err != nil {
		return fmt.Errorf("vnc: read device version: %w", err)
	}
	if _, err := device.Write(version); err != nil {
		return fmt.Errorf("vnc: write version to device: %w", err)
	}

	// 2. Security types. In 3.7+, the server lists them and the client picks.
	count := make([]byte, 1)
	if _, err := io.ReadFull(device, count); err != nil {
		return fmt.Errorf("vnc: read security count: %w", err)
	}
	if count[0] == 0 {
		// A zero count is followed by a reason string: the device refused us.
		return fmt.Errorf("vnc: device refused the connection: %s", readReason(device))
	}
	types := make([]byte, count[0])
	if _, err := io.ReadFull(device, types); err != nil {
		return fmt.Errorf("vnc: read security types: %w", err)
	}

	switch {
	case containsByte(types, secVNCAuth):
		if _, err := device.Write([]byte{secVNCAuth}); err != nil {
			return fmt.Errorf("vnc: select VNC auth: %w", err)
		}
		if err := vncAuth(device, password); err != nil {
			return err
		}
	case containsByte(types, secNone):
		// The device wants no password. Unusual for a configured Mac, but valid.
		if _, err := device.Write([]byte{secNone}); err != nil {
			return fmt.Errorf("vnc: select no auth: %w", err)
		}
	default:
		return fmt.Errorf("vnc: device offers no supported security type (offered %v)", types)
	}

	// 3. SecurityResult. 0 = OK. Non-zero is followed by a reason in 3.8.
	return readSecurityResult(device)
}

// vncAuth answers the 16-byte DES challenge.
func vncAuth(device io.ReadWriter, password string) error {
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(device, challenge); err != nil {
		return fmt.Errorf("vnc: read challenge: %w", err)
	}
	response, err := encryptChallenge(challenge, password)
	if err != nil {
		return err
	}
	if _, err := device.Write(response); err != nil {
		return fmt.Errorf("vnc: write challenge response: %w", err)
	}
	return nil
}

// readSecurityResult reads the u32 result and turns a failure into the device's
// own reason where one is offered.
func readSecurityResult(device io.Reader) error {
	var result uint32
	if err := binary.Read(device, binary.BigEndian, &result); err != nil {
		return fmt.Errorf("vnc: read security result: %w", err)
	}
	if result == 0 {
		return nil
	}
	// 3.8 appends a reason; 3.3 does not. Either way, the password is wrong or
	// access is denied — which for us means the sealed password and the one the
	// agent set have diverged.
	return fmt.Errorf("vnc: device rejected authentication: %s", readReason(device))
}

// presentNoAuthToBrowser plays the server half of the handshake toward the
// browser, offering exactly one security type: None.
func presentNoAuthToBrowser(browser io.ReadWriter) error {
	// 1. ProtocolVersion — we speak first to the browser.
	if _, err := browser.Write(version); err != nil {
		return err
	}
	clientVersion := make([]byte, 12)
	if _, err := io.ReadFull(browser, clientVersion); err != nil {
		return err
	}

	// 2. Offer only None, and let the browser select it.
	if _, err := browser.Write([]byte{1, secNone}); err != nil {
		return err
	}
	chosen := make([]byte, 1)
	if _, err := io.ReadFull(browser, chosen); err != nil {
		return err
	}
	if chosen[0] != secNone {
		return fmt.Errorf("browser selected security type %d, only None was offered", chosen[0])
	}

	// 3. SecurityResult OK. Required in 3.8 even for None.
	return binary.Write(browser, binary.BigEndian, uint32(0))
}

// bridgeInit forwards ClientInit (browser → device) and ServerInit
// (device → browser), after which the streams carry framebuffer traffic and
// the caller takes over with a plain copy.
func bridgeInit(device io.ReadWriter, browser io.ReadWriter) error {
	// ClientInit: a single shared-flag byte.
	shared := make([]byte, 1)
	if _, err := io.ReadFull(browser, shared); err != nil {
		return fmt.Errorf("read ClientInit: %w", err)
	}
	if _, err := device.Write(shared); err != nil {
		return fmt.Errorf("write ClientInit: %w", err)
	}

	// ServerInit: width(2) height(2) pixel-format(16) name-length(4) name(n).
	header := make([]byte, 24)
	if _, err := io.ReadFull(device, header); err != nil {
		return fmt.Errorf("read ServerInit header: %w", err)
	}
	nameLen := binary.BigEndian.Uint32(header[20:24])
	if nameLen > 1<<16 {
		return fmt.Errorf("implausible ServerInit name length %d", nameLen)
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(device, name); err != nil {
		return fmt.Errorf("read ServerInit name: %w", err)
	}
	if _, err := browser.Write(header); err != nil {
		return fmt.Errorf("write ServerInit header: %w", err)
	}
	if _, err := browser.Write(name); err != nil {
		return fmt.Errorf("write ServerInit name: %w", err)
	}
	return nil
}

// encryptChallenge encrypts the challenge with the password as a DES key, using
// the VNC bit-reversal quirk. The password is truncated or zero-padded to eight
// bytes, which is why screen.NewPassword generates exactly eight.
func encryptChallenge(challenge []byte, password string) ([]byte, error) {
	if len(challenge) != 16 {
		return nil, fmt.Errorf("vnc: challenge is %d bytes, want 16", len(challenge))
	}
	key := make([]byte, 8)
	copy(key, password)
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	block, err := des.NewCipher(key) //nolint:gosec // protocol-mandated.
	if err != nil {
		return nil, fmt.Errorf("vnc: DES key: %w", err)
	}
	out := make([]byte, 16)
	block.Encrypt(out[0:8], challenge[0:8])
	block.Encrypt(out[8:16], challenge[8:16])
	return out, nil
}

func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = (r << 1) | (b & 1)
		b >>= 1
	}
	return r
}

func containsByte(haystack []byte, needle byte) bool {
	for _, b := range haystack {
		if b == needle {
			return true
		}
	}
	return false
}

// readReason reads a length-prefixed reason string, best-effort. Used only on
// an error path, so it never itself returns an error — a missing reason becomes
// a generic message rather than masking the failure it is explaining.
func readReason(r io.Reader) string {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil || length == 0 || length > 4096 {
		return "no reason given"
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "no reason given"
	}
	return string(buf)
}

// Copy bridges two streams until either closes, returning the byte counts. The
// caller runs it once per direction. A context cancel closes the streams from
// the caller's side, which unblocks the reads here.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
