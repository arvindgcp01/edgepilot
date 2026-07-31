package main

import (
	"slices"
	"testing"
)

// partitionArgs exists because Go's flag package stops parsing at the first
// positional argument. That default silently dropped -yes from
// `migrate down 1 -yes` — on the one command where that flag suppresses a
// confirmation prompt before a destructive operation.
func TestPartitionArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		wantFlags      []string
		wantPositional []string
	}{
		{
			name:           "flags before positionals",
			args:           []string{"-yes", "down", "1"},
			wantFlags:      []string{"-yes"},
			wantPositional: []string{"down", "1"},
		},
		{
			name: "flag after a positional is still a flag",
			// The regression this function exists for.
			args:           []string{"down", "1", "-yes"},
			wantFlags:      []string{"-yes"},
			wantPositional: []string{"down", "1"},
		},
		{
			name:           "value flag with a separate value",
			args:           []string{"up", "-database-url", "postgres://h/db"},
			wantFlags:      []string{"-database-url", "postgres://h/db"},
			wantPositional: []string{"up"},
		},
		{
			name:           "value flag with an inline value",
			args:           []string{"-database-url=postgres://h/db", "up"},
			wantFlags:      []string{"-database-url=postgres://h/db"},
			wantPositional: []string{"up"},
		},
		{
			name: "boolean flag does not swallow the next argument",
			// If -yes consumed "3", the command would reverse one migration
			// instead of three.
			args:           []string{"-yes", "down", "3"},
			wantFlags:      []string{"-yes"},
			wantPositional: []string{"down", "3"},
		},
		{
			name:           "double dash ends flag parsing",
			args:           []string{"-json", "status", "--", "-not-a-flag"},
			wantFlags:      []string{"-json"},
			wantPositional: []string{"status", "-not-a-flag"},
		},
		{
			name:           "double-dash long form",
			args:           []string{"--json", "status"},
			wantFlags:      []string{"--json"},
			wantPositional: []string{"status"},
		},
		{
			name:           "mixed",
			args:           []string{"force", "-yes", "7", "-log-level", "debug"},
			wantFlags:      []string{"-yes", "-log-level", "debug"},
			wantPositional: []string{"force", "7"},
		},
		{
			name:           "no arguments",
			args:           nil,
			wantFlags:      nil,
			wantPositional: nil,
		},
		{
			name:           "only a subcommand",
			args:           []string{"status"},
			wantFlags:      nil,
			wantPositional: []string{"status"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flags, positional := partitionArgs(tc.args, migrateBoolFlags)
			if !slices.Equal(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			if !slices.Equal(positional, tc.wantPositional) {
				t.Errorf("positional = %q, want %q", positional, tc.wantPositional)
			}
		})
	}
}

func TestRequireIntArg(t *testing.T) {
	t.Parallel()

	if n, err := requireIntArg([]string{"3"}, "down <n>"); err != nil || n != 3 {
		t.Errorf("got (%d, %v), want (3, nil)", n, err)
	}
	if _, err := requireIntArg(nil, "down <n>"); err == nil {
		t.Error("a missing argument must be reported, not defaulted")
	}
	if _, err := requireIntArg([]string{"lots"}, "down <n>"); err == nil {
		t.Error("a non-numeric argument must be reported")
	}
}

func TestJoinUints(t *testing.T) {
	t.Parallel()

	if got := joinUints([]uint{1, 2, 8}); got != "1, 2, 8" {
		t.Errorf("joinUints = %q", got)
	}
	if got := joinUints(nil); got != "" {
		t.Errorf("joinUints(nil) = %q, want empty", got)
	}
}

// No t.Parallel here: t.Setenv mutates process-wide state, and the testing
// package rejects the combination outright.
func TestEnvOr(t *testing.T) {
	t.Setenv("EP_TEST_ENVOR", "set-value")
	if got := envOr("EP_TEST_ENVOR", "fallback"); got != "set-value" {
		t.Errorf("envOr = %q, want the environment value", got)
	}
	if got := envOr("EP_TEST_ENVOR_ABSENT", "fallback"); got != "fallback" {
		t.Errorf("envOr = %q, want the fallback", got)
	}
}
