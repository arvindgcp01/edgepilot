package domain

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultPageSize and MaxPageSize bound every listing.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Cursor marks a position in a result set.
//
// Every list in this API is backed by a table taking concurrent inserts, and
// OFFSET against such a table silently skips and duplicates rows between pages:
// insert one row before page 2 is fetched and a row from page 1 reappears.
// A cursor names the last row seen instead, so the boundary is stable no matter
// what is inserted meanwhile.
//
// The ID is carried alongside the timestamp because timestamps collide — two
// devices enrolled in the same microsecond would make the order partial, and a
// partial order loses or repeats rows at the page boundary.
type Cursor struct {
	// Time is the sort key for time-ordered listings.
	Time time.Time
	// Text is the sort key for text-ordered listings, such as devices by
	// hostname. Exactly one of Time and Text is meaningful, decided by the
	// ordering the page was fetched with — which is why changing the sort
	// mid-pagination invalidates the cursor rather than being silently honoured.
	Text string
	// ID breaks ties. Without it two rows sharing a sort key make the ordering
	// partial, and a partial order loses or repeats rows at the page boundary.
	ID uuid.UUID
}

// IsZero reports whether the cursor addresses the start of the result set.
func (c Cursor) IsZero() bool { return c.ID == uuid.Nil }

// Encode renders the cursor as an opaque token.
//
// Opaque, not structured: a client that parses a cursor will eventually
// construct one, and then the encoding cannot change. Base64 signals "do not
// read this" without pretending to be a security boundary — the contents are
// not secret, they are simply not the client's business.
func (c Cursor) Encode() string {
	if c.IsZero() {
		return ""
	}
	// An absent time is encoded as an empty field, not as UnixNano() of the
	// zero value: time.Time{} is year 1, which overflows int64 nanoseconds and
	// round-trips into a garbage date in 1754. A text-ordered cursor legitimately
	// carries no time, so this is the common case rather than an edge one.
	//
	// The empty string is the sentinel rather than "0", which is a real instant
	// (1970-01-01) and would be ambiguous.
	var timePart string
	if !c.Time.IsZero() {
		timePart = strconv.FormatInt(c.Time.UTC().UnixNano(), 10)
	}

	// The text key is base64-encoded within the payload so that a hostname
	// containing the field separator cannot corrupt the cursor.
	raw := timePart + "." +
		c.ID.String() + "." +
		base64.RawURLEncoding.EncodeToString([]byte(c.Text))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token produced by Encode.
//
// A malformed cursor is a 400, not an empty first page: silently restarting
// pagination would make a client loop over page one forever without ever
// reporting why.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, Invalid("/cursor", "is not a valid cursor")
	}
	parts := strings.SplitN(string(raw), ".", 3)
	if len(parts) != 3 {
		return Cursor{}, Invalid("/cursor", "is not a valid cursor")
	}
	var when time.Time
	if parts[0] != "" {
		n, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return Cursor{}, Invalid("/cursor", "is not a valid cursor")
		}
		when = time.Unix(0, n).UTC()
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return Cursor{}, Invalid("/cursor", "is not a valid cursor")
	}
	text, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Cursor{}, Invalid("/cursor", "is not a valid cursor")
	}
	return Cursor{Time: when, Text: string(text), ID: id}, nil
}

// PageRequest is the input to a listing.
type PageRequest struct {
	Cursor Cursor
	Limit  int
}

// Normalize clamps the limit into range. A caller asking for 10,000 rows gets
// the maximum rather than an error: the request is answerable, just not as
// asked, and failing it would be pedantic.
func (p PageRequest) Normalize() PageRequest {
	switch {
	case p.Limit <= 0:
		p.Limit = DefaultPageSize
	case p.Limit > MaxPageSize:
		p.Limit = MaxPageSize
	}
	return p
}

// Page is one page of results.
type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

// NewPage builds a page from one more row than requested.
//
// Fetching limit+1 and discarding the extra is how HasMore is answered without
// a second COUNT query — which on a filtered, RLS-scoped table would cost about
// as much as the page itself.
func NewPage[T any](rows []T, limit int, cursorOf func(T) Cursor) Page[T] {
	page := Page[T]{Items: rows}
	if limit > 0 && len(rows) > limit {
		page.Items = rows[:limit]
		page.HasMore = true
	}
	if page.Items == nil {
		// An empty result must encode as [] rather than null: a client that
		// ranges over the field should not have to nil-check it first.
		page.Items = []T{}
	}
	if page.HasMore && cursorOf != nil {
		page.NextCursor = cursorOf(page.Items[len(page.Items)-1]).Encode()
	}
	return page
}
