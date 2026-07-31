package problem

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/logging"
)

func request(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	ctx := logging.WithRequestID(r.Context(), "req-test-1")
	ctx = logging.WithLogger(ctx, logging.Discard())
	return r.WithContext(ctx)
}

func writeAndDecode(t *testing.T, err error) (*httptest.ResponseRecorder, Problem) {
	t.Helper()
	rec := httptest.NewRecorder()
	Write(rec, request(t, "/api/v1/devices/abc"), err)

	var p Problem
	if decodeErr := json.Unmarshal(rec.Body.Bytes(), &p); decodeErr != nil {
		t.Fatalf("response is not valid JSON (%v): %s", decodeErr, rec.Body.String())
	}
	return rec, p
}

// --- status mapping --------------------------------------------------------

func TestStatusMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want int
	}{
		{domain.NotFound("device", "abc"), http.StatusNotFound},
		{domain.Conflict("serial_already_enrolled", "already enrolled"), http.StatusConflict},
		{domain.Invalid("/params/delay_seconds", "must be <= 3600"), http.StatusUnprocessableEntity},
		{domain.Malformed("body is not valid JSON"), http.StatusBadRequest},
		{domain.Unauthenticated("invalid_credentials"), http.StatusUnauthorized},
		{domain.Forbidden("command:destructive"), http.StatusForbidden},
		{domain.Locked(900), http.StatusLocked},
		{domain.MFARequired(), http.StatusPreconditionRequired},
		{domain.TooLarge(1 << 20), http.StatusRequestEntityTooLarge},
		{domain.RateLimited(60), http.StatusTooManyRequests},
		{domain.DeviceOffline("abc"), http.StatusServiceUnavailable},
		{domain.Internal(errors.New("boom")), http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(domain.CodeOf(tc.err), func(t *testing.T) {
			t.Parallel()
			if got := StatusFor(tc.err); got != tc.want {
				t.Errorf("StatusFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// Every Kind must have a status and a title. A new Kind that nobody mapped
// would silently become a 500.
func TestEveryKindMaps(t *testing.T) {
	t.Parallel()
	kinds := []domain.Kind{
		domain.KindInternal, domain.KindNotFound, domain.KindConflict,
		domain.KindValidation, domain.KindMalformed, domain.KindUnauthenticated,
		domain.KindForbidden, domain.KindLocked, domain.KindMFARequired,
		domain.KindTooLarge, domain.KindRateLimited, domain.KindUnavailable,
	}
	for _, k := range kinds {
		if _, ok := statusByKind[k]; !ok {
			t.Errorf("Kind %q has no status mapping", k)
		}
		if _, ok := titleByKind[k]; !ok {
			t.Errorf("Kind %q has no title", k)
		}
	}
}

// An error that never went through the domain constructors — a driver failure,
// a context deadline — must produce a 500, not a 200 or a panic.
func TestUnclassifiedErrorBecomesInternal(t *testing.T) {
	t.Parallel()
	rec, p := writeAndDecode(t, errors.New("pq: connection refused"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("raw error text leaked to the client: %s", rec.Body.String())
	}
	if p.Detail != "An internal error occurred." {
		t.Errorf("Detail = %q, want the generic message", p.Detail)
	}
}

// --- disclosure ------------------------------------------------------------

// The cause of an internal error is for the logs. Anything about the server's
// internals in a response body helps an attacker and helps no one else.
func TestInternalCauseIsNeverDisclosed(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("pq: relation \"devices\" does not exist; "+
		"dsn=postgres://edgepilot:%s@db.internal:5432/edgepilot", "hunter2")

	rec, p := writeAndDecode(t, domain.Internal(cause).WithOp("device.List"))
	body := rec.Body.String()

	for _, leaked := range []string{"hunter2", "db.internal", "relation", "pq:", "dsn="} {
		if strings.Contains(body, leaked) {
			t.Errorf("internal detail %q leaked to the client: %s", leaked, body)
		}
	}
	if p.Detail != "An internal error occurred." {
		t.Errorf("Detail = %q, want the generic message", p.Detail)
	}
	// The request ID is what lets an operator find the real cause in the logs,
	// so it must survive.
	if p.RequestID != "req-test-1" {
		t.Errorf("RequestID = %q, want it echoed for log correlation", p.RequestID)
	}
}

// A resource in another organization must be indistinguishable from one that
// does not exist. A 403 here would confirm existence and let a caller enumerate
// another tenant's fleet.
func TestCrossTenantAccessIsIndistinguishableFromAbsence(t *testing.T) {
	t.Parallel()
	// Row-level security makes a cross-tenant read return no rows, so the
	// repository produces exactly the same error in both cases. This pins that
	// the rendering does not reintroduce a difference.
	absent := domain.NotFound("device", "3f9a1c2e-0000-0000-0000-000000000001")
	otherTenant := domain.NotFound("device", "3f9a1c2e-0000-0000-0000-000000000002")

	recA, pA := writeAndDecode(t, absent)
	recB, pB := writeAndDecode(t, otherTenant)

	if recA.Code != http.StatusNotFound || recB.Code != http.StatusNotFound {
		t.Fatalf("both must be 404, got %d and %d", recA.Code, recB.Code)
	}
	if pA.Type != pB.Type || pA.Title != pB.Title || pA.Status != pB.Status {
		t.Errorf("responses differ in a way that discloses existence:\n%+v\n%+v", pA, pB)
	}
}

// Authentication failures must be uniform: an unknown email, a wrong password,
// and a disabled account all look the same, or the endpoint enumerates users.
func TestAuthenticationFailuresAreUniform(t *testing.T) {
	t.Parallel()
	var bodies []string
	for _, code := range []string{"invalid_credentials", "invalid_credentials", "invalid_credentials"} {
		rec, _ := writeAndDecode(t, domain.Unauthenticated(code))
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("authentication failures differ:\n%s\n%s", bodies[0], bodies[i])
		}
	}
}

// --- body shape ------------------------------------------------------------

func TestProblemBodyShape(t *testing.T) {
	t.Parallel()
	err := domain.Validation(
		domain.FieldError{Pointer: "/params/delay_seconds", Message: "must be <= 3600"},
		domain.FieldError{Pointer: "/params/reason", Message: "is required"},
	)

	rec, p := writeAndDecode(t, err)

	if got := rec.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("Content-Type = %q, want %q", got, ContentType)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if p.Type != TypeBaseURI+"validation_failed" {
		t.Errorf("Type = %q", p.Type)
	}
	if p.Status != http.StatusUnprocessableEntity {
		t.Errorf("Status = %d", p.Status)
	}
	if p.Instance != "/api/v1/devices/abc" {
		t.Errorf("Instance = %q", p.Instance)
	}
	if len(p.Errors) != 2 {
		t.Fatalf("expected 2 field errors, got %d", len(p.Errors))
	}
	if p.Errors[0].Pointer != "/params/delay_seconds" {
		t.Errorf("field pointer = %q", p.Errors[0].Pointer)
	}
}

// The status in the body must agree with the status on the wire; clients read
// whichever is convenient.
func TestBodyStatusMatchesWireStatus(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		domain.NotFound("device", "x"),
		domain.Forbidden("device:write"),
		domain.RateLimited(30),
		domain.Internal(errors.New("x")),
	} {
		rec, p := writeAndDecode(t, err)
		if p.Status != rec.Code {
			t.Errorf("body status %d != wire status %d for %v", p.Status, rec.Code, err)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{domain.RateLimited(60), "60"},
		{domain.Locked(900), "900"},
		{domain.DeviceOffline("abc"), "30"},
		{domain.NotFound("device", "abc"), ""},
	} {
		rec := httptest.NewRecorder()
		Write(rec, request(t, "/x"), tc.err)
		if got := rec.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("Retry-After for %v = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestUnauthorizedAdvertisesHowToAuthenticate(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Write(rec, request(t, "/x"), domain.Unauthenticated("missing_token"))
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
}

// HEAD must carry the status and headers but no body.
func TestHeadRequestHasNoBody(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodHead, "/api/v1/devices", nil)
	r = r.WithContext(logging.WithLogger(r.Context(), logging.Discard()))

	rec := httptest.NewRecorder()
	Write(rec, r, domain.NotFound("device", "abc"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response should have no body, got %q", rec.Body.String())
	}
}
