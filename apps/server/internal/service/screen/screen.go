// Package screen holds the small amount of shared logic behind remote screen
// access: generating a device's VNC password, sealing it, and unsealing it.
//
// It deliberately has no store and no transport. Three call sites need these
// three functions — the device update handler (to enable), the gRPC config
// builder (to send the password to the agent), and the relay (to authenticate)
// — and each already holds the pieces it needs. Putting the crypto here rather
// than in any one of them keeps the seal/unseal symmetric by construction.
package screen

import (
	"crypto/rand"
	"fmt"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/platform/secret"
)

// passwordLen is 8 because the VNC authentication scheme (RFB 003.x, security
// type 2) uses the password as a DES key and silently truncates it to eight
// bytes. Generating exactly eight avoids the trap of a longer password that
// authenticates by its first eight characters and confuses everyone.
const passwordLen = 8

// alphabet excludes look-alike characters. The password is never typed by a
// human — the relay uses it — but a support engineer reading it out of a log
// during an incident should not have to guess l versus 1.
const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// NewPassword returns a fresh random VNC password.
func NewPassword() (string, error) {
	buf := make([]byte, passwordLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("screen: generate a password: %w", err)
	}
	out := make([]byte, passwordLen)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// Seal encrypts a password for storage. The device id is the associated data,
// so a sealed password copied onto another device's row fails to open — it is
// bound to exactly the machine it was minted for.
func Seal(sealer *secret.Sealer, deviceID uuid.UUID, password string) ([]byte, error) {
	sealed, err := sealer.Seal([]byte(password), deviceID[:])
	if err != nil {
		return nil, fmt.Errorf("screen: seal the password: %w", err)
	}
	return sealed, nil
}

// Open recovers a device's plaintext VNC password. Returns ("", nil) when the
// device has none — screen sharing was never enabled — so callers can treat
// absence as "no password to send" rather than an error.
func Open(sealer *secret.Sealer, deviceID uuid.UUID, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	plaintext, err := sealer.Open(sealed, deviceID[:])
	if err != nil {
		return "", fmt.Errorf("screen: open the password: %w", err)
	}
	return string(plaintext), nil
}
