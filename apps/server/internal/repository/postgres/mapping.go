package postgres

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Conversions between pgx's wire types and plain Go.
//
// These exist so that pgtype does not escape this package. A domain entity
// carrying a pgtype.Timestamptz would force every service, every handler, and
// every test to know which database driver the repository happens to use — and
// would make the domain package untestable without one.

// --- UUID ------------------------------------------------------------------

func toPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil}
}

func toPgUUIDPtr(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return toPgUUID(*id)
}

func fromPgUUID(v pgtype.UUID) uuid.UUID {
	if !v.Valid {
		return uuid.Nil
	}
	return uuid.UUID(v.Bytes)
}

func fromPgUUIDPtr(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := uuid.UUID(v.Bytes)
	return &id
}

// --- time ------------------------------------------------------------------

func toPgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

func fromPgTime(v pgtype.Timestamptz) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time.UTC()
}

func fromPgTimePtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

// --- scalars ---------------------------------------------------------------

// derefString flattens a nullable column to "".
//
// Safe only where the distinction does not matter: a hostname that is NULL and
// one that is empty are equally "not set" to every consumer. Where the
// difference *is* meaningful — a threshold, a count, a timestamp — the pointer
// is kept.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrInt16(v *int) *int16 {
	if v == nil {
		return nil
	}
	n := int16(*v)
	return &n
}

func intFromInt16(v *int16) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// --- inet and macaddr ------------------------------------------------------

// toNetip parses a textual address for an inet column.
//
// An unparseable address yields NULL rather than an error: these come from
// agent-reported inventory, and one malformed field must not fail an entire
// enrollment. The value is cosmetic — it is displayed, never routed on.
func toNetip(s string) *netip.Addr {
	if s == "" {
		return nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	return &addr
}

func fromNetip(a *netip.Addr) string {
	if a == nil || !a.IsValid() {
		return ""
	}
	return a.String()
}

// --- jsonb -----------------------------------------------------------------

// labelsToJSON renders a label map for a jsonb column.
//
// nil becomes `{}` rather than SQL NULL, because the containment operator used
// for label selectors (`labels @> '{...}'`) is false against NULL — a device
// with no labels would silently vanish from an unfiltered listing.
func labelsToJSON(labels map[string]string) ([]byte, error) {
	if labels == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(labels)
}

// labelsFromJSON parses a jsonb label column.
//
// A malformed payload returns an empty map rather than an error: labels are
// operator metadata, and one bad row should not make a device unreadable.
func labelsFromJSON(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]string{}
	}
	return out
}

// settingsToJSON and settingsFromJSON handle organization settings, where a
// parse failure IS worth surfacing: settings carry policy, and silently
// defaulting a security-relevant flag would be worse than an error.
func settingsToJSON(v any) ([]byte, error) {
	if v == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(v)
}

func settingsFromJSON(raw []byte, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// macString renders a macaddr column. Empty rather than an error when unset:
// the value is displayed, never matched on.
func macString(a net.HardwareAddr) string {
	if len(a) == 0 {
		return ""
	}
	return a.String()
}

// --- numeric ---------------------------------------------------------------

// X.509 serial numbers are up to 20 bytes — far beyond int64 — so the column is
// numeric(40,0) and the domain carries the decimal string. These convert
// between the two without going through a lossy integer type.

func numericFromDecimalString(s string) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if s == "" {
		return n, nil
	}
	if err := n.Scan(s); err != nil {
		return n, fmt.Errorf("certificate serial %q is not a decimal integer: %w", s, err)
	}
	return n, nil
}

func numericToDecimalString(n pgtype.Numeric) string {
	if !n.Valid {
		return ""
	}
	v, err := n.Value()
	if err != nil || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
