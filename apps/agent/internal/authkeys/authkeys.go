// Package authkeys installs the operator keys the server sends.
//
// The point is that an operator registers a public key with EdgePilot once and
// can then reach every Mac in the organization. Without this, "add my key to the
// fleet" means visiting every machine, which does not scale past the first site
// and is exactly the chore the product exists to remove.
//
// # What it touches
//
// Two things, both on purpose and both reversible. It does NOT create accounts:
// keys are installed for a local account that already exists — normally the
// person who uses the Mac — because adding login accounts to a customer's
// machines is not something a fleet agent should do unasked.
//
//   - `/etc/edgepilot/authorized_keys.d/<user>`, written only by this package.
//     Keeping EdgePilot's keys in their own file means reconciliation can delete
//     anything it does not recognise without ever touching a key a human added
//     to `~/.ssh/authorized_keys`.
//   - One `AuthorizedKeysFile` line in `/etc/ssh/sshd_config`, added once, so
//     sshd consults that file at all.
//
// # Why reconcile rather than append
//
// Because revocation has to work. The server sends the complete set of keys
// that should be present; whatever is in the file and not in that set is
// removed. An append-only scheme would leave a revoked key working on every Mac
// that happened to be offline when it was revoked.
package authkeys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// Defaults for a macOS install.
const (
	// DefaultUsername is the account a key targets when it does not name one.
	// It must already exist on the device; nothing here creates it.
	DefaultUsername = "edgepilot"

	// KeysDir holds one file per target account, owned by root.
	KeysDir = "/etc/edgepilot/authorized_keys.d"

	sshdConfigPath = "/etc/ssh/sshd_config"

	// managedMarker identifies the line this package adds to sshd_config, so a
	// second run recognises its own work instead of adding it again.
	managedMarker = "# Added by EdgePilot. Operator keys are managed in " + KeysDir
)

// Reconciler installs and removes operator keys.
type Reconciler struct {
	log *slog.Logger

	// dir and config are overridable so tests do not write to /etc.
	dir        string
	sshdConfig string

	// run executes a privileged command. Replaced in tests.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)

	// lookupUser reports whether an account exists.
	lookupUser func(name string) (*user.User, error)

	// lastApplied is the digest of the key set most recently written, so an
	// unchanged policy — which arrives on every reconnect — does not rewrite
	// files or reload sshd.
	lastApplied string
}

// Options configures a Reconciler.
type Options struct {
	Log        *slog.Logger
	Dir        string
	SSHDConfig string
}

// New builds a Reconciler.
func New(opts Options) *Reconciler {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Dir == "" {
		opts.Dir = KeysDir
	}
	if opts.SSHDConfig == "" {
		opts.SSHDConfig = sshdConfigPath
	}
	return &Reconciler{
		log:        opts.Log,
		dir:        opts.Dir,
		sshdConfig: opts.SSHDConfig,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		lookupUser: user.Lookup,
	}
}

// Apply makes the machine match the key set the server sent.
func (r *Reconciler) Apply(ctx context.Context, keys []*v1.AuthorizedKey) error {
	// Group by account: one file per target user, and each file is authoritative
	// for that account.
	byUser := make(map[string][]*v1.AuthorizedKey)
	for _, k := range keys {
		if strings.TrimSpace(k.GetPublicKey()) == "" {
			continue
		}
		username := k.GetTargetUsername()
		if username == "" {
			username = DefaultUsername
		}
		if !validUsername(username) {
			// The server should never send this; refusing is cheaper than
			// reasoning about what a shell would do with it.
			r.log.Warn("ignoring an authorized key with an implausible username",
				slog.String("username", username))
			continue
		}
		byUser[username] = append(byUser[username], k)
	}

	digest := digestOf(byUser)
	if digest == r.lastApplied {
		return nil
	}

	var failures []error
	for username, userKeys := range byUser {
		if err := r.applyUser(ctx, username, userKeys); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", username, err))
		}
	}

	// An account that had keys and now has none: the file is emptied rather
	// than deleted, so the account stops accepting EdgePilot keys immediately
	// and the absence is visible to anyone looking.
	if err := r.pruneUnlisted(byUser); err != nil {
		failures = append(failures, err)
	}

	if len(failures) > 0 {
		return errors.Join(failures...)
	}

	// Only after everything succeeded. A partial apply must be retried on the
	// next policy, not skipped because the digest matched.
	r.lastApplied = digest
	r.log.Info("reconciled the operator key set",
		slog.Int("accounts", len(byUser)), slog.Int("keys", len(keys)))
	return nil
}

// applyUser writes one account's keys.
func (r *Reconciler) applyUser(ctx context.Context, username string, keys []*v1.AuthorizedKey) error {
	if err := r.requireUser(ctx, username); err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", r.dir, err)
	}

	var buf bytes.Buffer
	buf.WriteString("# Managed by EdgePilot. Edits are overwritten.\n")
	buf.WriteString("# Keys are registered against the organization, not this machine.\n")

	// Sorted, so an unchanged set produces an unchanged file and the digest
	// check above stays meaningful.
	sort.Slice(keys, func(i, j int) bool { return keys[i].GetKeyId() < keys[j].GetKeyId() })

	for _, k := range keys {
		options := append([]string(nil), k.GetOptions()...)
		if k.GetExpiresAt() != nil {
			// sshd enforces this itself, so an expired key stops working even on
			// a Mac that never hears the revocation.
			options = append(options, fmt.Sprintf(`expiry-time="%s"`,
				k.GetExpiresAt().AsTime().UTC().Format("20060102150405")))
		}

		if len(options) > 0 {
			buf.WriteString(strings.Join(options, ","))
			buf.WriteByte(' ')
		}
		buf.WriteString(strings.TrimSpace(k.GetPublicKey()))
		buf.WriteString(" # edgepilot:")
		buf.WriteString(k.GetKeyId())
		buf.WriteByte('\n')
	}

	path := filepath.Join(r.dir, username)
	if err := writeFileAtomic(path, buf.Bytes(), 0o644); err != nil {
		return err
	}

	if err := r.ensureSSHDConfig(ctx); err != nil {
		return fmt.Errorf("configuring sshd: %w", err)
	}
	return nil
}

// pruneUnlisted empties files for accounts the server no longer sends keys for.
func (r *Reconciler) pruneUnlisted(current map[string][]*v1.AuthorizedKey) error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", r.dir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, still := current[e.Name()]; still {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		if err := writeFileAtomic(path,
			[]byte("# Managed by EdgePilot. No keys are currently authorized.\n"), 0o644); err != nil {
			return err
		}
		r.log.Info("removed the operator keys for an account no longer authorized",
			slog.String("username", e.Name()))
	}
	return nil
}

// requireUser checks that the target account already exists.
//
// EdgePilot never creates an account. These are the customer's machines, and a
// fleet agent that adds login accounts to them is doing something the operator
// did not ask for and may not notice. Keys are installed for accounts that are
// already there — the person who normally uses the Mac — and a key naming an
// account that does not exist is reported rather than acted upon.
func (r *Reconciler) requireUser(ctx context.Context, username string) error {
	if _, err := r.lookupUser(username); err != nil {
		return fmt.Errorf("no local account %q on this device; "+
			"register the key against an account that exists here", username)
	}

	// Remote Login access is re-checked on every reconcile, not once: an
	// operator can switch it from "All users" to "Only these users" long after
	// the key was installed, and that would silently lock EdgePilot out of a Mac
	// it had been reaching perfectly well.
	return r.allowRemoteLogin(ctx, username)
}

// allowRemoteLogin grants the account access to sshd on macOS.
//
// Remote Login has an access list of its own, held in the group
// `com.apple.access_ssh`. When it is set to "Only these users" — which is what
// enabling it for one account produces — sshd refuses everyone outside that
// group *before* it considers their key. The failure is an ordinary "Permission
// denied (publickey)", which looks exactly like a key that was not installed,
// so it is worth handling explicitly rather than leaving to be rediscovered.
//
// When the group does not exist, Remote Login is set to "All users" and there is
// nothing to join.
func (r *Reconciler) allowRemoteLogin(ctx context.Context, username string) error {
	const group = "com.apple.access_ssh"

	if _, err := r.run(ctx, "dseditgroup", "-o", "read", group); err != nil {
		r.log.Debug("Remote Login is open to all users; no access group to join")
		return nil
	}

	if out, err := r.run(ctx, "dseditgroup", "-o", "edit",
		"-a", username, "-t", "user", group); err != nil {
		return fmt.Errorf("granting %s access to Remote Login: %w: %s",
			username, err, bytes.TrimSpace(out))
	}

	r.log.Info("granted the EdgePilot account access to Remote Login",
		slog.String("username", username), slog.String("group", group))
	return nil
}

// ensureSSHDConfig points sshd at the managed key directory.
//
// Added once and then left alone. The user's own ~/.ssh/authorized_keys stays
// first in the list, so this only ever adds a place for sshd to look — it never
// takes one away.
func (r *Reconciler) ensureSSHDConfig(ctx context.Context) error {
	existing, err := os.ReadFile(r.sshdConfig)
	if err != nil {
		if os.IsNotExist(err) {
			// No sshd on this machine. Keys are written and will apply if one is
			// ever enabled; failing here would be reporting someone else's
			// problem as ours.
			return nil
		}
		return fmt.Errorf("reading %s: %w", r.sshdConfig, err)
	}

	directive := managedMarker + "\n" +
		"AuthorizedKeysFile\t.ssh/authorized_keys " + r.dir + "/%u\n"

	// Prefer a drop-in. sshd uses the *first* value it finds for a keyword, and
	// macOS ships an `Include /etc/ssh/sshd_config.d/*` at the top of the main
	// file — so a line appended to the bottom can be silently overridden by
	// anything included above it, while a drop-in is read early and wins.
	if dropInDir, ok := includedDir(existing); ok {
		path := filepath.Join(dropInDir, "100-edgepilot.conf")
		if current, err := os.ReadFile(path); err == nil &&
			bytes.Contains(current, []byte(r.dir)) {
			return nil
		}
		if err := os.MkdirAll(dropInDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dropInDir, err)
		}
		if err := writeFileAtomic(path, []byte(directive), 0o644); err != nil {
			return err
		}
		r.log.Info("told sshd to read EdgePilot's managed keys",
			slog.String("path", path))
		return r.validateAndReload(ctx, path, nil)
	}

	if bytes.Contains(existing, []byte(managedMarker)) {
		return nil
	}

	updated := append([]byte(nil), existing...)
	if len(updated) > 0 && !bytes.HasSuffix(updated, []byte("\n")) {
		updated = append(updated, '\n')
	}
	updated = append(updated, []byte("\n"+directive)...)

	if err := writeFileAtomic(r.sshdConfig, updated, 0o644); err != nil {
		return err
	}
	r.log.Info("told sshd to read EdgePilot's managed keys",
		slog.String("path", r.sshdConfig))
	return r.validateAndReload(ctx, r.sshdConfig, existing)
}

// includedDir finds the drop-in directory the main config includes, if any.
func includedDir(config []byte) (string, bool) {
	for _, line := range strings.Split(string(config), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || !strings.EqualFold(fields[0], "Include") {
			continue
		}
		// "Include /etc/ssh/sshd_config.d/*" — the directory is what matters.
		dir := filepath.Dir(strings.Trim(fields[1], `"`))
		if strings.HasPrefix(dir, "/etc/ssh/") {
			return dir, true
		}
	}
	return "", false
}

// validateAndReload checks the configuration and restarts sshd.
//
// restore is the previous contents of path, or nil when path is a new file this
// package created — in which case rolling back means removing it.
func (r *Reconciler) validateAndReload(ctx context.Context, path string, restore []byte) error {

	// A configuration sshd will not parse is worse than no change at all: the
	// next restart takes Remote Login down entirely, and the machine is
	// unreachable by the very path this was meant to open.
	if out, err := r.run(ctx, "/usr/sbin/sshd", "-t"); err != nil {
		r.log.Error("the sshd configuration is invalid; rolling back",
			slog.String("path", path),
			slog.String("output", string(bytes.TrimSpace(out))))

		var restoreErr error
		if restore == nil {
			restoreErr = os.Remove(path)
		} else {
			restoreErr = writeFileAtomic(path, restore, 0o644)
		}
		if restoreErr != nil {
			return fmt.Errorf("sshd config invalid and could not be restored: %w", restoreErr)
		}
		return fmt.Errorf("sshd rejected the configuration: %w", err)
	}

	// Reload so it takes effect without waiting for a reboot. Existing sessions
	// are unaffected.
	if out, err := r.run(ctx, "launchctl", "kickstart", "-k", "system/com.openssh.sshd"); err != nil {
		// Not fatal: the keys are on disk and sshd picks them up when it next
		// starts. Saying so is more useful than failing the whole reconcile.
		r.log.Warn("could not reload sshd; the keys apply at its next restart",
			slog.Any("error", err), slog.String("output", string(bytes.TrimSpace(out))))
	}
	return nil
}

// writeFileAtomic replaces a file without ever leaving it half-written.
//
// A truncated authorized_keys file is a machine nobody can log into, and a
// truncated sshd_config is worse.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".edgepilot-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// digestOf summarises a key set, so an unchanged policy is a no-op.
func digestOf(byUser map[string][]*v1.AuthorizedKey) string {
	usernames := make([]string, 0, len(byUser))
	for u := range byUser {
		usernames = append(usernames, u)
	}
	sort.Strings(usernames)

	var b strings.Builder
	for _, u := range usernames {
		b.WriteString(u)
		b.WriteByte('\n')

		lines := make([]string, 0, len(byUser[u]))
		for _, k := range byUser[u] {
			expiry := ""
			if k.GetExpiresAt() != nil {
				expiry = k.GetExpiresAt().AsTime().Format(time.RFC3339)
			}
			lines = append(lines, k.GetKeyId()+"|"+strings.TrimSpace(k.GetPublicKey())+
				"|"+expiry+"|"+strings.Join(k.GetOptions(), ","))
		}
		sort.Strings(lines)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// validUsername guards what reaches dscl and a file path.
//
// The rule is [a-z_] first, then [a-z0-9_.-], matching the CHECK on
// ssh_keys.target_username that the server enforces. The dot matters: macOS
// derives a short name from the full name at account creation and routinely
// produces one containing one — "City Pallets Inc." becomes "citypalletsinc."
// — so the POSIX useradd rule this used to apply rejected entirely ordinary
// customer accounts. Every device in this fleet is a Mac. The Linux rule was
// never the right one, and it failed silently: the key was dropped with a
// warning in a log nobody reads, and the operator saw only a refused login.
//
// Requiring the first character to be [a-z_] is what keeps the dot safe. The
// value becomes a path element under /etc/edgepilot/authorized_keys.d, and no
// string beginning with [a-z_] can be "." or ".." or contain a separator, so
// it cannot escape that directory.
func validUsername(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9', c == '-', c == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
