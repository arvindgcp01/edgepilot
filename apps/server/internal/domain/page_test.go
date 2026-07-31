package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	when := time.Date(2026, 7, 31, 12, 34, 56, 789_000_000, time.UTC)

	for _, tc := range []struct {
		name string
		in   Cursor
	}{
		{"time key", Cursor{Time: when, ID: id}},
		{"text key", Cursor{Text: "acme-mac-01", ID: id}},
		{"both keys", Cursor{Time: when, Text: "acme-mac-01", ID: id}},
		// Hostnames can contain the separator, which is why the text component
		// is encoded rather than concatenated raw.
		{"text containing the separator", Cursor{Text: "mac.with.dots", ID: id}},
		{"empty text", Cursor{Time: when, Text: "", ID: id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			decoded, err := DecodeCursor(tc.in.Encode())
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !decoded.Time.Equal(tc.in.Time) {
				t.Errorf("time = %v, want %v", decoded.Time, tc.in.Time)
			}
			if decoded.Text != tc.in.Text {
				t.Errorf("text = %q, want %q", decoded.Text, tc.in.Text)
			}
			if decoded.ID != tc.in.ID {
				t.Errorf("id = %v, want %v", decoded.ID, tc.in.ID)
			}
		})
	}
}

func TestZeroCursorEncodesToEmpty(t *testing.T) {
	t.Parallel()

	if got := (Cursor{}).Encode(); got != "" {
		t.Errorf("zero cursor encoded to %q, want empty", got)
	}
	c, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if !c.IsZero() {
		t.Error("empty token should decode to the zero cursor")
	}
}

// A malformed cursor must be reported, not silently treated as "start again".
// Restarting would make a client loop over page one forever with no indication
// why.
func TestMalformedCursorIsRejected(t *testing.T) {
	t.Parallel()

	for _, token := range []string{
		"not-base64!!",
		"YWJj",                     // valid base64, wrong shape
		"MTIzLg",                   // too few parts
		"bm90LWEtbnVtYmVyLmFiYy54", // non-numeric time
	} {
		if _, err := DecodeCursor(token); err == nil {
			t.Errorf("token %q should be rejected", token)
		} else if !errors.Is(err, ErrValidation) {
			t.Errorf("token %q should produce a validation error, got %v", token, err)
		}
	}
}

func TestPageRequestNormalize(t *testing.T) {
	t.Parallel()

	if got := (PageRequest{}).Normalize().Limit; got != DefaultPageSize {
		t.Errorf("zero limit = %d, want the default %d", got, DefaultPageSize)
	}
	if got := (PageRequest{Limit: -5}).Normalize().Limit; got != DefaultPageSize {
		t.Errorf("negative limit = %d, want the default", got)
	}
	// An oversized request is clamped rather than rejected: it is answerable,
	// just not as asked, and failing it would be pedantic.
	if got := (PageRequest{Limit: 10_000}).Normalize().Limit; got != MaxPageSize {
		t.Errorf("oversized limit = %d, want the maximum %d", got, MaxPageSize)
	}
	if got := (PageRequest{Limit: 25}).Normalize().Limit; got != 25 {
		t.Errorf("in-range limit was changed to %d", got)
	}
}

func TestNewPageTrimsTheProbeRow(t *testing.T) {
	t.Parallel()

	cursorOf := func(n int) Cursor { return Cursor{ID: uuid.New(), Text: string(rune('a' + n))} }

	// Fetching limit+1 is how HasMore is answered without a COUNT.
	full := NewPage([]int{1, 2, 3, 4}, 3, cursorOf)
	if len(full.Items) != 3 {
		t.Errorf("items = %d, want 3 (the probe row must be trimmed)", len(full.Items))
	}
	if !full.HasMore {
		t.Error("HasMore should be true when an extra row came back")
	}
	if full.NextCursor == "" {
		t.Error("a page with more should carry a cursor")
	}

	exact := NewPage([]int{1, 2, 3}, 3, cursorOf)
	if exact.HasMore {
		t.Error("HasMore should be false when exactly the limit came back")
	}
	if exact.NextCursor != "" {
		t.Error("a final page should carry no cursor")
	}
}

// An empty result must serialise as [] rather than null, so a client can range
// over it without a nil check.
func TestEmptyPageHasNonNilItems(t *testing.T) {
	t.Parallel()

	page := NewPage[int](nil, 10, func(int) Cursor { return Cursor{} })
	if page.Items == nil {
		t.Error("Items must be non-nil so it encodes as [] rather than null")
	}
	if len(page.Items) != 0 {
		t.Errorf("len = %d, want 0", len(page.Items))
	}
	if page.HasMore {
		t.Error("an empty page has no more")
	}
}
