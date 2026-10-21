//go:build !darwin || simulator

package screenshare

import "context"

// Off a real Mac there is no Screen Sharing to toggle, so the command is never
// executed — the reconciler still runs its decision logic (and tests still
// exercise it by substituting r.run), but nothing happens on the host.
func runCommand(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, nil
}
