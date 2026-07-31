// Package problem renders domain errors as RFC 9457 Problem Details.
//
// This is the only place in the server that decides an HTTP status code.
// Handlers return errors; services and repositories return domain.Kind. That
// separation is what keeps a not-found from becoming a 500 in one handler and a
// 404 in another.
//
// It is also a security boundary. Two rules hold without exception:
//
//   - An internal error's cause is logged and never rendered. A stack trace, a
//     SQL fragment, or a connection string in a response body is a gift to an
//     attacker and useless to a legitimate client.
//
//   - A resource in another organization renders exactly as one that does not
//     exist. Anything else turns the API into an oracle for enumerating another
//     tenant's fleet.
package problem

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
)

// TypeBaseURI prefixes every problem type. The full URI is stable and part of
// the API contract, so clients may branch on it.
const TypeBaseURI = "https://edgepilot.visionify.ai/problems/"

// ContentType is the media type required by RFC 9457.
const ContentType = "application/problem+json"

// Problem is the response body. Field names match the schema in
// packages/shared/openapi/openapi.yaml; CI contract-tests the two against each
// other.
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Errors    []FieldProblem `json:"errors,omitempty"`
}

// FieldProblem locates one validation failure.
type FieldProblem struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

// statusByKind is the single mapping table. Every Kind appears; the compiler
// does not enforce that, so TestEveryKindMaps does.
var statusByKind = map[domain.Kind]int{
	domain.KindNotFound:        http.StatusNotFound,
	domain.KindConflict:        http.StatusConflict,
	domain.KindValidation:      http.StatusUnprocessableEntity,
	domain.KindMalformed:       http.StatusBadRequest,
	domain.KindUnauthenticated: http.StatusUnauthorized,
	domain.KindForbidden:       http.StatusForbidden,
	domain.KindLocked:          http.StatusLocked,
	domain.KindMFARequired:     http.StatusPreconditionRequired,
	domain.KindTooLarge:        http.StatusRequestEntityTooLarge,
	domain.KindRateLimited:     http.StatusTooManyRequests,
	domain.KindUnavailable:     http.StatusServiceUnavailable,
	domain.KindInternal:        http.StatusInternalServerError,
}

var titleByKind = map[domain.Kind]string{
	domain.KindNotFound:        "Not found",
	domain.KindConflict:        "Conflict",
	domain.KindValidation:      "Validation failed",
	domain.KindMalformed:       "Malformed request",
	domain.KindUnauthenticated: "Unauthenticated",
	domain.KindForbidden:       "Insufficient permission",
	domain.KindLocked:          "Account locked",
	domain.KindMFARequired:     "Multi-factor authentication required",
	domain.KindTooLarge:        "Payload too large",
	domain.KindRateLimited:     "Too many requests",
	domain.KindUnavailable:     "Temporarily unavailable",
	domain.KindInternal:        "Internal error",
}

// StatusFor returns the HTTP status for an error. Anything unclassified is a
// 500: an error nobody categorised is a bug, and a bug is a server fault.
func StatusFor(err error) int {
	if status, ok := statusByKind[domain.KindOf(err)]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// From converts an error into a Problem and its status code.
//
// instance is the request path. requestID ties the response to the server logs
// and the audit entry.
func From(err error, instance, requestID string) (Problem, int) {
	kind := domain.KindOf(err)
	status := StatusFor(err)

	p := Problem{
		Type:      TypeBaseURI + domain.CodeOf(err),
		Title:     titleFor(kind),
		Status:    status,
		Instance:  instance,
		RequestID: requestID,
	}

	domainErr, ok := domain.AsError(err)
	if !ok {
		// Not one of ours: an unwrapped driver or library error. It gets the
		// generic 500 body, and its text is not disclosed.
		p.Detail = "An internal error occurred."
		return p, status
	}

	// Internal errors carry a wrapped cause that must not reach the client. The
	// caller-facing Message is used, never Error(), which would splice the
	// cause into the body.
	p.Detail = domainErr.Message
	if kind == domain.KindInternal {
		p.Detail = "An internal error occurred."
	}

	for _, f := range domainErr.Fields {
		p.Errors = append(p.Errors, FieldProblem{Pointer: f.Pointer, Message: f.Message})
	}

	return p, status
}

// Write renders err to w and logs it at a level matching its severity.
//
// Client mistakes are not server problems: a 404 or a 403 logs at debug, so the
// log stays readable and a genuine 500 remains visible.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	requestID := logging.RequestIDFromContext(r.Context())
	p, status := From(err, r.URL.Path, requestID)

	logProblem(r, err, status)

	if retry := retryAfter(err); retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	// An unauthenticated request needs to be told how to authenticate.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="edgepilot"`)
	}

	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	// A failure to encode is unrecoverable here — the status is already sent —
	// so it is logged rather than returned.
	if encodeErr := json.NewEncoder(w).Encode(p); encodeErr != nil {
		logging.FromContext(r.Context()).ErrorContext(r.Context(),
			"failed to encode problem response", slog.Any("error", encodeErr))
	}
}

func logProblem(r *http.Request, err error, status int) {
	log := logging.FromContext(r.Context())

	attrs := []any{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.String("code", domain.CodeOf(err)),
	}
	if d, ok := domain.AsError(err); ok && d.Op != "" {
		attrs = append(attrs, slog.String("op", d.Op))
	}
	// The full error, including the wrapped cause the client never sees. The
	// logger's redactor handles anything sensitive that reached the message.
	attrs = append(attrs, slog.Any("error", err))

	switch {
	case status >= 500:
		log.ErrorContext(r.Context(), "request failed", attrs...)
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		// Denied access is a security event, not noise: it is what an intrusion
		// attempt looks like in aggregate.
		log.WarnContext(r.Context(), "request denied", attrs...)
	default:
		log.DebugContext(r.Context(), "request rejected", attrs...)
	}
}

func retryAfter(err error) int {
	if d, ok := domain.AsError(err); ok {
		return d.RetryAfterSeconds
	}
	return 0
}

func titleFor(k domain.Kind) string {
	if t, ok := titleByKind[k]; ok {
		return t
	}
	return "Internal error"
}
