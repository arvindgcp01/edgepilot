package main

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	t.Parallel()

	// The output must satisfy the CHECK constraint on organizations.slug:
	// ^[a-z0-9][a-z0-9-]{1,62}$
	for input, want := range map[string]string{
		"Visionify Operations":     "visionify-operations",
		"ACME Corp.":               "acme-corp",
		"Foo   Bar":                "foo-bar",
		"  leading and trailing  ": "leading-and-trailing",
		"Already-Hyphenated":       "already-hyphenated",
		"Numbers 123":              "numbers-123",
		"--Leading Hyphens":        "leading-hyphens",
		"Ünïcödé Näme":             "n-c-d-n-me",
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got := slugify(input)
			if got != want {
				t.Errorf("slugify(%q) = %q, want %q", input, got, want)
			}
			assertValidSlug(t, got)
		})
	}
}

func TestSlugifyAlwaysProducesAValidSlug(t *testing.T) {
	t.Parallel()

	// Inputs that reduce to nothing usable must still yield something the
	// database will accept — failing a CHECK constraint is a confusing way for
	// an operator to learn their organization name was unusable.
	for _, input := range []string{"", "!!!", "---", "?", "  ", "日本語"} {
		got := slugify(input)
		assertValidSlug(t, got)
	}

	// And an over-long name is truncated rather than rejected.
	long := slugify(strings.Repeat("verylongname ", 20))
	if len(long) > 63 {
		t.Errorf("slug is %d characters, exceeding the 63-character limit: %q", len(long), long)
	}
	assertValidSlug(t, long)
}

func assertValidSlug(t *testing.T, slug string) {
	t.Helper()
	if len(slug) < 2 || len(slug) > 63 {
		t.Errorf("slug %q has invalid length %d", slug, len(slug))
		return
	}
	first := rune(slug[0])
	isLower := first >= 'a' && first <= 'z'
	isDigit := first >= '0' && first <= '9'
	if !isLower && !isDigit {
		t.Errorf("slug %q must start with a letter or digit", slug)
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			t.Errorf("slug %q contains an invalid character %q", slug, r)
			return
		}
	}
}

func TestGeneratedPasswordIsStrongAndUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]struct{}{}
	for range 200 {
		pw, err := generatePassword()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if _, dup := seen[pw]; dup {
			t.Fatal("a generated password repeated; the entropy source is broken")
		}
		seen[pw] = struct{}{}

		// Must clear the hasher's own minimum, or bootstrap would fail after
		// generating a password it then refuses.
		if len(pw) < 12 {
			t.Errorf("generated password is only %d characters: %q", len(pw), pw)
		}
	}
}
