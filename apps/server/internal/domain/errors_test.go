package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorsIsMatchesByKind(t *testing.T) {
	t.Parallel()
	err := NotFound("device", "abc")

	if !errors.Is(err, ErrNotFound) {
		t.Error("a not-found error should match ErrNotFound regardless of which site produced it")
	}
	if errors.Is(err, ErrConflict) {
		t.Error("a not-found error must not match ErrConflict")
	}

	// Wrapping must not break matching, which is the whole point of a taxonomy.
	wrapped := fmt.Errorf("loading device: %w", err)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("wrapping broke errors.Is")
	}
	if KindOf(wrapped) != KindNotFound {
		t.Error("wrapping broke KindOf")
	}
}

// A specific sentinel should match only its own code, so a caller can branch on
// "device offline" without catching every unavailable dependency.
func TestErrorsIsCanMatchASpecificCode(t *testing.T) {
	t.Parallel()
	offline := DeviceOffline("abc")
	storageDown := Unavailable("storage_unavailable", "blob storage is unreachable")

	target := &Error{Kind: KindUnavailable, Code: "device_offline"}

	if !errors.Is(offline, target) {
		t.Error("device offline should match its specific sentinel")
	}
	if errors.Is(storageDown, target) {
		t.Error("an unrelated unavailable error must not match the device-offline sentinel")
	}
	// Both are still KindUnavailable.
	if !errors.Is(storageDown, ErrUnavailable) {
		t.Error("storage failure should still match the Kind sentinel")
	}
}

// The zero Kind is KindInternal on purpose: an error nobody classified is a
// bug, and 500 is the honest answer.
func TestZeroKindIsInternal(t *testing.T) {
	t.Parallel()
	var e Error
	if e.Kind != KindInternal {
		t.Errorf("zero Kind = %q, want KindInternal", e.Kind)
	}
	if KindOf(nil) != KindInternal {
		t.Error("KindOf(nil) should be KindInternal, not a panic or a success")
	}
}

// Decoration must not mutate the shared sentinels — that would corrupt every
// later comparison in the process.
func TestDecorationDoesNotMutateSentinels(t *testing.T) {
	t.Parallel()
	before := fmt.Sprintf("%+v", *ErrNotFound)

	decorated := ErrNotFound.WithOp("device.Get").WithMessage("gone").WithField("/id", "bad")
	if decorated == ErrNotFound {
		t.Fatal("decoration must return a copy, not the sentinel itself")
	}
	if after := fmt.Sprintf("%+v", *ErrNotFound); after != before {
		t.Errorf("sentinel was mutated: %s, want %s", after, before)
	}
	if decorated.Op != "device.Get" || decorated.Message != "gone" || len(decorated.Fields) != 1 {
		t.Errorf("decoration did not apply: %+v", decorated)
	}
}

func TestErrorStringIncludesCauseForLogs(t *testing.T) {
	t.Parallel()
	// Error() is the log representation and SHOULD carry the cause. It is the
	// rendering path that must not — the two are deliberately different.
	err := Internal(errors.New("underlying failure")).WithOp("device.List")
	msg := err.Error()
	for _, want := range []string{"device.List", "internal_error", "underlying failure"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() omits %q: %s", want, msg)
		}
	}
}

func TestUnwrapReachesTheCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("root cause")
	err := Conflict("dup", "duplicate").Wrap(sentinel)
	if !errors.Is(err, sentinel) {
		t.Error("Unwrap should expose the cause to errors.Is")
	}
}
