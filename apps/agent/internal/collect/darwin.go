//go:build darwin && !simulator

package collect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// darwinCollector reads facts from a real Mac.
//
// Everything here shells out, because the alternative is cgo bindings to
// IOKit — and a static, cgo-free binary is what makes the agent a single file
// that runs on every macOS version in the fleet.
type darwinCollector struct{}

func newCollector() Collector { return &darwinCollector{} }

func (c *darwinCollector) Collect(ctx context.Context) (*Facts, error) {
	hardware, err := run(ctx, "system_profiler", "-json", "SPHardwareDataType")
	if err != nil {
		return nil, err
	}
	facts, err := ParseHardware(hardware)
	if err != nil {
		return nil, err
	}
	if facts.SerialNumber == "" {
		return nil, ErrNoSerialNumber
	}

	// Everything below is best-effort. A Mac that will not report its build
	// number is still a Mac worth managing, and failing the whole collection
	// over one field would leave it unmanaged instead.
	if out, swErr := run(ctx, "sw_vers"); swErr == nil {
		facts.OSName, facts.OSVersion, facts.OSBuild = ParseSWVers(out)
	}
	if facts.OSName == "" {
		facts.OSName = "macOS"
	}

	if out, btErr := run(ctx, "sysctl", "-n", "kern.boottime"); btErr == nil {
		if booted, parseErr := ParseBootTime(out); parseErr == nil {
			facts.BootedAt = booted
		}
	}

	if name, hostErr := os.Hostname(); hostErr == nil {
		facts.Hostname = shortHostname(name)
	}
	facts.LocalIP = primaryLocalIP(ctx)
	facts.DiskTotalBytes = diskTotalBytes("/")

	return facts, nil
}

// commandTimeout bounds each external command.
//
// system_profiler can take seconds on a busy machine and has been known to hang
// outright; an agent that blocks forever on it never reports at all, which is
// worse than reporting without a model name.
const commandTimeout = 20 * time.Second

// run executes a command with a bound and returns its stdout.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// A minimal environment: these tools need none of the agent's, and
	// inheriting one is how a stray LANG turns parseable output into localised
	// output.
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}

	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if stderr != "" {
			return nil, fmt.Errorf("collect: %s: %w: %s", name, err, stderr)
		}
		return nil, fmt.Errorf("collect: %s: %w", name, err)
	}
	return out, nil
}
