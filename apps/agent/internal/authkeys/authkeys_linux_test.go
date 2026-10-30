package authkeys

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/visionify/edgepilot/packages/proto/gen/edgepilot/v1"
)

// The Linux path: the key file, sshd -t, then a reload of whichever of
// ssh/sshd exists — and never dseditgroup or launchctl.
func TestApplyLinuxReloadsTheUnitThatExists(t *testing.T) {
	dir := t.TempDir()
	sshDir := filepath.Join(dir, "ssh")
	if err := os.MkdirAll(filepath.Join(sshDir, "sshd_config.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(sshDir, "sshd_config")
	// No Include: includedDir only honours /etc/ssh/ paths, so a temp tree
	// exercises the append-to-sshd_config branch.
	if err := os.WriteFile(config, []byte("Port 22\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	r := New(Options{Dir: filepath.Join(dir, "keys"), SSHDConfig: config})
	r.goos = "linux"
	r.lookupUser = func(name string) (*user.User, error) { return &user.User{Username: name}, nil }
	r.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		if call == "systemctl try-reload-or-restart ssh.service" {
			// A RHEL box: there is no ssh.service, only sshd.service.
			return []byte("Unit ssh.service not found."), errors.New("exit status 5")
		}
		return nil, nil
	}

	keys := []*v1.AuthorizedKey{{KeyId: "k1", PublicKey: "ssh-ed25519 AAAA test", TargetUsername: "ubuntu"}}
	if err := r.Apply(context.Background(), keys); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "keys", "ubuntu"))
	if err != nil || !strings.Contains(string(got), "ssh-ed25519 AAAA test # edgepilot:k1") {
		t.Fatalf("key file = %q, %v", got, err)
	}

	for _, c := range calls {
		if strings.HasPrefix(c, "dseditgroup") || strings.HasPrefix(c, "launchctl") {
			t.Errorf("macOS command run on Linux: %s", c)
		}
	}
	want := []string{"/usr/sbin/sshd -t", "systemctl try-reload-or-restart ssh.service", "systemctl try-reload-or-restart sshd.service"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// Revoking the last key must clear it from disk even when the agent has just
// restarted and has never applied anything in this process.
func TestApplyEmptySetAfterRestartPrunes(t *testing.T) {
	dir := t.TempDir()
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(keysDir, "ubuntu")
	if err := os.WriteFile(stale, []byte("ssh-ed25519 AAAA revoked # edgepilot:k1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := New(Options{Dir: keysDir, SSHDConfig: filepath.Join(dir, "absent")})
	r.goos = "linux"
	r.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }

	if err := r.Apply(context.Background(), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(stale)
	if strings.Contains(string(got), "AAAA revoked") {
		t.Fatalf("the revoked key survived a restart: %q", got)
	}
}
