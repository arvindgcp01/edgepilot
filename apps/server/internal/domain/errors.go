// Package domain holds EdgePilot's entities, invariants, and the interfaces the
// service layer depends on. It imports nothing but the standard library — no
// Gin, no GORM, no gRPC, no Redis. That constraint is what keeps the business
// rules testable without infrastructure, and it is enforced in CI by
// scripts/lint-arch.sh.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a failure by what the caller should do about it.
//
// Nothing below the transport layer knows an HTTP status code. Services and
// repositories return a Kind; exactly one place — internal/transport/http —
// turns that into a status. Handlers that construct status codes themselves are
// how a 404 for one caller becomes a 500 for another.
type Kind string

const (
	// KindInternal is the zero value on purpose: an error that nobody
	// classified is a bug, and 500 is the honest answer for a bug.
	KindInternal Kind = ""

	KindNotFound        Kind = "not_found"
	KindConflict        Kind = "conflict"
	KindValidation      Kind = "validation"
	KindMalformed       Kind = "malformed"
	KindUnauthenticated Kind = "unauthenticated"
	KindForbidden       Kind = "forbidden"
	KindLocked          Kind = "locked"
	KindMFARequired     Kind = "mfa_required"
	KindTooLarge        Kind = "too_large"
	KindRateLimited     Kind = "rate_limited"
	KindUnavailable     Kind = "unavailable"
)

// FieldError locates a validation failure within the request body.
type FieldError struct {
	// Pointer is an RFC 6901 JSON Pointer into the request, such as
	// "/params/delay_seconds".
	Pointer string
	Message string
}

// Error is EdgePilot's error type.
//
// It carries three things a bare error cannot: a Kind the transport layer maps
// to a status, a stable Code clients can branch on without parsing prose, and a
// Message that is safe to show a caller. Anything unsafe stays in the wrapped
// error, which is logged and never rendered.
type Error struct {
	Kind Kind

	// Code is a stable, machine-readable slug — "device_offline",
	// "serial_already_enrolled". It becomes the last segment of the Problem
	// Details type URI, so it is part of the API contract and must not change
	// once released.
	Code string

	// Message is shown to the caller. It must never contain a secret, an
	// internal hostname, a SQL fragment, or anything else that describes the
	// server's internals.
	Message string

	// Fields carries per-field detail for KindValidation.
	Fields []FieldError

	// Op is the logical operation, such as "device.Approve". Logged, never
	// rendered.
	Op string

	// RetryAfterSeconds populates the Retry-After header on 429 and 503.
	RetryAfterSeconds int

	// err is the underlying cause. Logged, never rendered.
	err error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.Code != "" {
		b.WriteString(e.Code)
		b.WriteString(": ")
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	} else {
		b.WriteString(string(e.Kind))
	}
	if e.err != nil {
		b.WriteString(": ")
		b.WriteString(e.err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.err }

// Is makes every error of a given Kind match that Kind's sentinel, so callers
// can write errors.Is(err, domain.ErrNotFound) without caring which of the
// dozens of not-found sites produced it.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	// A sentinel carries only a Kind; a concrete error additionally carries a
	// Code. Comparing Codes when the target has one allows the more specific
	// errors.Is(err, ErrDeviceOffline) to work too.
	if t.Code != "" && t.Code != e.Code {
		return false
	}
	return t.Kind == e.Kind
}

// Sentinels for errors.Is. They are matched by Kind, so any error of that Kind
// satisfies them.
var (
	ErrNotFound        = &Error{Kind: KindNotFound}
	ErrConflict        = &Error{Kind: KindConflict}
	ErrValidation      = &Error{Kind: KindValidation}
	ErrMalformed       = &Error{Kind: KindMalformed}
	ErrUnauthenticated = &Error{Kind: KindUnauthenticated}
	ErrForbidden       = &Error{Kind: KindForbidden}
	ErrLocked          = &Error{Kind: KindLocked}
	ErrMFARequired     = &Error{Kind: KindMFARequired}
	ErrTooLarge        = &Error{Kind: KindTooLarge}
	ErrRateLimited     = &Error{Kind: KindRateLimited}
	ErrUnavailable     = &Error{Kind: KindUnavailable}
	ErrInternal        = &Error{Kind: KindInternal}
)

// --- constructors ----------------------------------------------------------

// NotFound reports a missing resource.
//
// It is also the correct answer for a resource that exists in another
// organization. Returning 403 there would confirm the resource exists, turning
// the API into an oracle for enumerating another tenant's fleet. Row-level
// security makes this the natural outcome — a cross-tenant query returns no
// rows — but it is stated here because it is a decision, not an accident.
func NotFound(resource, id string) *Error {
	return &Error{
		Kind:    KindNotFound,
		Code:    resource + "_not_found",
		Message: fmt.Sprintf("No %s with id %q.", resource, id),
	}
}

// Conflict reports a state conflict: a duplicate, or an invalid transition.
func Conflict(code, format string, args ...any) *Error {
	return &Error{Kind: KindConflict, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Validation reports one or more field-level problems.
func Validation(fields ...FieldError) *Error {
	return &Error{
		Kind:    KindValidation,
		Code:    "validation_failed",
		Message: "The request body failed validation.",
		Fields:  fields,
	}
}

// Invalid reports a single field-level problem.
func Invalid(pointer, format string, args ...any) *Error {
	return Validation(FieldError{Pointer: pointer, Message: fmt.Sprintf(format, args...)})
}

// Malformed reports a request that could not be parsed at all.
func Malformed(format string, args ...any) *Error {
	return &Error{Kind: KindMalformed, Code: "malformed_request", Message: fmt.Sprintf(format, args...)}
}

// Unauthenticated reports absent or invalid credentials.
//
// The message is deliberately uniform. An unknown email, a wrong password, and
// a disabled account must be indistinguishable, or the endpoint becomes a user
// enumeration oracle.
func Unauthenticated(code string) *Error {
	return &Error{
		Kind:    KindUnauthenticated,
		Code:    code,
		Message: "Authentication failed.",
	}
}

// Forbidden reports a caller who is authenticated but lacks a permission.
func Forbidden(permission string) *Error {
	return &Error{
		Kind:    KindForbidden,
		Code:    "insufficient_permission",
		Message: fmt.Sprintf("This action requires %s.", permission),
	}
}

// Locked reports an account locked after repeated failures.
func Locked(retryAfterSeconds int) *Error {
	return &Error{
		Kind:              KindLocked,
		Code:              "account_locked",
		Message:           "This account is temporarily locked after repeated failed sign-in attempts.",
		RetryAfterSeconds: retryAfterSeconds,
	}
}

// MFARequired reports that a second factor is enrolled and was not supplied.
func MFARequired() *Error {
	return &Error{
		Kind:    KindMFARequired,
		Code:    "mfa_required",
		Message: "A multi-factor authentication code is required.",
	}
}

// TooLarge reports a payload above the accepted limit.
func TooLarge(limitBytes int64) *Error {
	return &Error{
		Kind:    KindTooLarge,
		Code:    "payload_too_large",
		Message: fmt.Sprintf("The payload exceeds the %d byte limit.", limitBytes),
	}
}

// RateLimited reports a caller who exceeded their quota.
func RateLimited(retryAfterSeconds int) *Error {
	return &Error{
		Kind:              KindRateLimited,
		Code:              "rate_limited",
		Message:           "Too many requests.",
		RetryAfterSeconds: retryAfterSeconds,
	}
}

// Unavailable reports a dependency that is temporarily unusable.
func Unavailable(code, format string, args ...any) *Error {
	return &Error{Kind: KindUnavailable, Code: code, Message: fmt.Sprintf(format, args...)}
}

// DeviceOffline is common enough across files, commands, and logs to deserve
// its own constructor.
func DeviceOffline(deviceID string) *Error {
	return &Error{
		Kind:              KindUnavailable,
		Code:              "device_offline",
		Message:           "The device is not currently connected.",
		RetryAfterSeconds: 30,
		Op:                "device.Reach",
	}
}

// Internal reports a server-side fault.
//
// The wrapped cause is logged; the caller is told nothing about it. A stack
// trace, a SQL fragment, or a connection string in an error body is a gift to
// an attacker and useless to a legitimate client.
func Internal(err error) *Error {
	return &Error{
		Kind:    KindInternal,
		Code:    "internal_error",
		Message: "An internal error occurred.",
		err:     err,
	}
}

// --- decoration ------------------------------------------------------------

// Wrap attaches an underlying cause. The cause is logged and never rendered.
func (e *Error) Wrap(err error) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.err = err
	return &clone
}

// WithOp records the logical operation, for logs and traces.
func (e *Error) WithOp(op string) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Op = op
	return &clone
}

// WithMessage replaces the caller-facing message. The replacement must be safe
// to show a client.
func (e *Error) WithMessage(format string, args ...any) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Message = fmt.Sprintf(format, args...)
	return &clone
}

// WithField appends a field-level detail.
func (e *Error) WithField(pointer, format string, args ...any) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Fields = append(append([]FieldError(nil), e.Fields...),
		FieldError{Pointer: pointer, Message: fmt.Sprintf(format, args...)})
	return &clone
}

// --- inspection ------------------------------------------------------------

// KindOf reports the Kind of any error.
//
// An unclassified error — a driver failure, a context deadline, anything that
// escaped without being wrapped — reports KindInternal. That is the safe
// default: it produces a 500 and discloses nothing.
func KindOf(err error) Kind {
	if err == nil {
		return KindInternal
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// CodeOf returns the stable machine-readable code, or "internal_error".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Code != "" {
		return e.Code
	}
	return "internal_error"
}

// AsError extracts the *Error from a chain, if there is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsKind reports whether err has the given Kind.
func IsKind(err error, k Kind) bool { return KindOf(err) == k }
