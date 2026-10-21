// Package screenshare turns macOS Screen Sharing on and off, and sets its VNC
// password, so an operator can reach the device's desktop through the same
// tunnel SSH uses.
//
// Why Apple's own service rather than a bundled capture: Screen Sharing (and
// Remote Management, which configures it) is a system LaunchDaemon. It runs at
// the login window, survives reboots with no user logged in, and is exempt from
// the screen-recording consent prompt that macOS forces on every third-party
// capture — a prompt no one can click on a headless Mac. Reusing it is the
// difference between "works on a rack of mac minis" and "works on the one Mac a
// human is sitting at".
//
// # What it touches
//
// One thing, reversibly: it invokes `kickstart`, the Remote Management control
// tool, to enable the service with a legacy-VNC password (so a plain VNC client
// — here, the relay — can authenticate on port 5900) or to disable it. It
// creates no accounts and changes nothing else. When policy says off, it turns
// the service off; the two states are driven entirely by the server.
package screenshare

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Reconciler applies the desired screen-sharing state to the machine.
//
// Like the authkeys reconciler, it is idempotent and remembers the last state
// it applied, so an unchanged policy — which arrives on every reconnect — does
// not re-run kickstart. The password is compared too: a rotated password must
// re-run even when the on/off state did not change.
type Reconciler struct {
	log *slog.Logger

	// run executes a privileged command. Replaced in tests.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)

	mu           sync.Mutex
	lastEnabled  *bool
	lastPassword string
}

// Options configures a Reconciler.
type Options struct {
	Log *slog.Logger
}

// New builds a Reconciler.
func New(opts Options) *Reconciler {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &Reconciler{log: opts.Log, run: runCommand}
}

// Apply makes the machine match the desired state.
//
// Called from the config path on its own goroutine (kickstart restarts a
// service and is not instant), so it must not be on the goroutine that reads
// the stream. A failure is returned for logging and retried on the next config
// rather than escalated: a Mac whose Screen Sharing would not toggle is still a
// managed Mac, and dropping its stream over that would remove the telemetry
// that says so.
func (r *Reconciler) Apply(ctx context.Context, enabled bool, password string) error {
	r.mu.Lock()
	unchanged := r.lastEnabled != nil && *r.lastEnabled == enabled &&
		(!enabled || r.lastPassword == password)
	r.mu.Unlock()
	if unchanged {
		return nil
	}

	name, args := screenCommand(enabled, password)
	if _, err := r.run(ctx, name, args...); err != nil {
		if enabled {
			return fmt.Errorf("screenshare: enable: %w", err)
		}
		return fmt.Errorf("screenshare: disable: %w", err)
	}

	r.mu.Lock()
	e := enabled
	r.lastEnabled = &e
	r.lastPassword = password
	r.mu.Unlock()

	r.log.Info("applied screen-sharing policy", slog.Bool("enabled", enabled))
	return nil
}

// kickstart is Remote Management's control tool — the supported way to enable
// Screen Sharing with a VNC password from the command line, present on every
// macOS version this fleet runs.
const kickstart = "/System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart"

// screenCommand builds the privileged invocation for the desired state. It is
// the same on every platform because it is only data; whether it actually runs
// is decided by runCommand, which is a real exec on macOS and a no-op
// elsewhere. Keeping the command here rather than behind a build tag is what
// lets the reconciler's decisions be tested on any OS.
//
// Enable, in order: activate and configure the service; grant all users access
// with full privileges; set the legacy VNC password so a plain VNC client can
// authenticate on 5900; restart the agent so it takes effect at once. The
// password is an argument, briefly visible in the process list — acceptable for
// a short-lived per-device secret, and better than leaving it in a temp file.
func screenCommand(enabled bool, password string) (string, []string) {
	if !enabled {
		return kickstart, []string{"-deactivate", "-configure", "-access", "-off"}
	}
	return kickstart, []string{
		"-activate",
		"-configure",
		"-access", "-on",
		"-allowAccessFor", "-allUsers",
		"-privs", "-all",
		"-clientopts", "-setvnclegacy", "-vnclegacy", "yes",
		"-setvncpw", "-vncpw", password,
		"-restart", "-agent",
	}
}
