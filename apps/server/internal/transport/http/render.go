package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// decode reads a JSON request body into v.
//
// Unknown fields are rejected. A client that sends `allow_custom_script` when
// the field is `allow_custom_scripts` should be told, not silently ignored and
// left believing it set something — this is the difference between a typo caught
// in development and a permission nobody granted.
func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, _ := strings.Cut(ct, ";")
		if strings.TrimSpace(mediaType) != "application/json" {
			return domain.Validation(domain.FieldError{
				Pointer: "/", Message: "the request body must be application/json",
			})
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return decodeError(err)
	}
	// Exactly one JSON value. A body with trailing content is either a bug or
	// an attempt at request smuggling, and accepting the first value silently
	// is how the second becomes possible.
	if dec.More() {
		return domain.Validation(domain.FieldError{
			Pointer: "/", Message: "the request body must hold exactly one JSON object",
		})
	}
	return nil
}

// decodeError turns a JSON failure into a validation problem a client can act
// on, without echoing the parser's internal wording.
func decodeError(err error) error {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return domain.Validation(domain.FieldError{
			Pointer: "/",
			Message: fmt.Sprintf("the request body is larger than the %d byte limit", maxBytes.Limit),
		})
	}

	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return domain.Validation(domain.FieldError{
			Pointer: "/",
			Message: fmt.Sprintf("the request body is not valid JSON (at byte %d)", syntax.Offset),
		})
	}

	var unmarshalType *json.UnmarshalTypeError
	if errors.As(err, &unmarshalType) {
		pointer := "/"
		if unmarshalType.Field != "" {
			pointer += strings.ReplaceAll(unmarshalType.Field, ".", "/")
		}
		return domain.Validation(domain.FieldError{
			Pointer: pointer,
			Message: fmt.Sprintf("expected %s", unmarshalType.Type.String()),
		})
	}

	if strings.Contains(err.Error(), "unknown field") {
		// The decoder's message names the field, which is the useful part.
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return domain.Validation(domain.FieldError{
			Pointer: "/" + field, Message: "unknown field",
		})
	}

	if errors.Is(err, io.EOF) {
		return domain.Validation(domain.FieldError{
			Pointer: "/", Message: "the request body is empty",
		})
	}
	return domain.Validation(domain.FieldError{
		Pointer: "/", Message: "the request body could not be read",
	})
}

// respond writes a JSON response.
func respond(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if body == nil {
		return nil
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so there is nothing useful to tell
		// the client. Returning the error puts it in the access log, which is
		// where it can actually be seen.
		return fmt.Errorf("http: encode response: %w", err)
	}
	return nil
}

// noContent writes a 204.
func noContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// pathUUID reads a UUID path variable.
//
// A malformed id is a 400 rather than a 404: the client sent something that
// cannot identify anything, which is a different problem from asking for
// something that does not exist.
func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.Validation(domain.FieldError{
			Pointer: "/" + name, Message: "must be a UUID",
		})
	}
	return id, nil
}

// pageRequest reads cursor pagination from the query string.
func pageRequest(r *http.Request) (domain.PageRequest, error) {
	page := domain.PageRequest{}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return page, domain.Validation(domain.FieldError{
				Pointer: "/limit", Message: "must be a number",
			})
		}
		page.Limit = limit
	}

	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err := domain.DecodeCursor(raw)
		if err != nil {
			// A cursor is opaque, so a client cannot have constructed a valid
			// one by hand — a malformed one is either tampering or a cursor
			// from a different sort order, and both deserve the same answer.
			return page, domain.Validation(domain.FieldError{
				Pointer: "/cursor", Message: "is not a valid cursor",
			})
		}
		page.Cursor = cursor
	}

	return page.Normalize(), nil
}

// queryList reads a repeated or comma-separated query parameter.
func queryList(r *http.Request, name string) []string {
	values := r.URL.Query()[name]
	var out []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}
