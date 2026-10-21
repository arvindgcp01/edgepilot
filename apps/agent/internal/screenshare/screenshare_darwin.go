//go:build darwin && !simulator

package screenshare

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// commandTimeout bounds each invocation. kickstart restarts a service and can
// take a couple of seconds; twenty is generous and still bounds a hang.
const commandTimeout = 20 * time.Second

// runCommand executes a privileged command with a bound and a minimal
// environment, matching how the authkeys reconciler shells out.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return nil, fmt.Errorf("%s: %w: %s", name, err, stderr)
			}
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
