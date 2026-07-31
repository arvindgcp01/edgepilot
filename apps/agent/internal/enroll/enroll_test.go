package enroll_test

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/agent/internal/enroll"
	"github.com/visionify/edgepilot/apps/agent/internal/identity"
)

// The endpoint is stood up here rather than mocked at the client boundary: what
// is worth testing is that the agent speaks the contract in openapi.yaml — the
// field names, the base64 CSR, the Bearer header — and a stub that returned
// canned values regardless of the request would test none of that.
//
// The server side of this contract is tested against openapi.yaml on its own, in
// the server module. The two meet in the end-to-end test that runs once both
// binaries exist.

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// captured is what the fake endpoint saw.
type captured struct {
	authorization string
	contentType   string
	userAgent     string
	path          string
	body          map[string]any
}

// enrollServer implements POST /api/v1/agent/enroll.
type enrollServer struct {
	t      *testing.T
	status int
	// respond overrides the success body.
	respond func(req map[string]any) any
	// problem is returned instead when status is an error.
	problem any

	seen []captured
}

func (s *enrollServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.seen = append(s.seen, captured{
		authorization: r.Header.Get("Authorization"),
		contentType:   r.Header.Get("Content-Type"),
		userAgent:     r.Header.Get("User-Agent"),
		path:          r.URL.Path,
		body:          body,
	})

	status := s.status
	if status == 0 {
		status = http.StatusCreated
	}
	if status >= 400 {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		payload := s.problem
		if payload == nil {
			payload = map[string]any{
				"type": "about:blank", "title": "Enrollment failed",
				"status": status, "detail": "the token was rejected",
			}
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}

	var payload any
	if s.respond != nil {
		payload = s.respond(body)
	} else {
		payload = successBody(body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func successBody(req map[string]any) map[string]any {
	deviceID, _ := req["device_id"].(string)
	return map[string]any{
		"device_id":             deviceID,
		"org_id":                "0f3b7c1e-1c1a-4a3d-9f8e-2b6d4c5a7e91",
		"certificate_pem":       "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		"ca_bundle_pem":         "-----BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----\n",
		"not_after":             "2026-10-29T12:00:00Z",
		"approved":              true,
		"device_plane_endpoint": "edgepilot.example.com:9443",
		"signing_public_keys": []string{
			base64.StdEncoding.EncodeToString([]byte("signing-key")),
		},
	}
}

func newClient(t *testing.T, srv *enrollServer, baseURL, token string) *enroll.Client {
	t.Helper()
	c, err := enroll.New(enroll.Options{
		BaseURL: baseURL, Token: token, Logger: quiet(),
		HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// realRequest builds a request with a genuine CSR, so the encoding is exercised
// against bytes a CA would actually have to parse.
func realRequest(t *testing.T) (enroll.Request, uuid.UUID) {
	t.Helper()

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := identity.NewDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.CreateCSR(key, deviceID)
	if err != nil {
		t.Fatal(err)
	}

	return enroll.Request{
		DeviceID: deviceID, CSRDer: csr,
		SerialNumber: "H2WK9J4YQ1", HardwareUUID: "F1B2C3D4-5E6F-4A7B-8C9D-0E1F2A3B4C5D",
		Hostname: "mac-mini-01", ModelIdentifier: "Mac14,12", Chip: "Apple M2 Pro",
		Arch: "arm64", CPUCores: 12, MemoryBytes: 32 << 30, DiskTotalBytes: 500 << 30,
		OSVersion: "14.5", OSBuild: "23F79", AgentVersion: "1.0.0", LocalIP: "10.0.0.42",
	}, deviceID
}

// --- the happy path --------------------------------------------------------

func TestEnrollSpeaksTheContract(t *testing.T) {
	t.Parallel()

	handler := &enrollServer{t: t}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newClient(t, handler, srv.URL, "ep_enroll_secret")
	req, deviceID := realRequest(t)

	result, err := client.Enroll(t.Context(), req)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	if len(handler.seen) != 1 {
		t.Fatalf("the endpoint was called %d times, want 1", len(handler.seen))
	}
	got := handler.seen[0]

	if got.path != "/api/v1/agent/enroll" {
		t.Errorf("path = %q, want /api/v1/agent/enroll", got.path)
	}
	if got.authorization != "Bearer ep_enroll_secret" {
		t.Errorf("authorization = %q, want a Bearer token", got.authorization)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.contentType)
	}
	if !strings.HasPrefix(got.userAgent, "edgepilot-agent/") {
		t.Errorf("user-agent = %q, want an edgepilot-agent identifier", got.userAgent)
	}

	// The CSR must arrive as base64 DER that a CA can parse — this is the field
	// most likely to be mangled and the one that fails most obscurely.
	encoded, ok := got.body["csr"].(string)
	if !ok {
		t.Fatalf("csr = %#v, want a base64 string", got.body["csr"])
	}
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("csr is not base64: %v", err)
	}
	parsed, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatalf("the server could not parse the CSR: %v", err)
	}
	if err := parsed.CheckSignature(); err != nil {
		t.Errorf("the CSR arrived with a broken signature: %v", err)
	}
	if parsed.Subject.CommonName != deviceID.String() {
		t.Errorf("CSR CN = %q, want the device id", parsed.Subject.CommonName)
	}

	for field, want := range map[string]any{
		"device_id": deviceID.String(), "serial_number": "H2WK9J4YQ1",
		"hostname": "mac-mini-01", "arch": "arm64", "chip": "Apple M2 Pro",
		"model_identifier": "Mac14,12", "os_version": "14.5", "os_build": "23F79",
		"cpu_cores": float64(12), "memory_bytes": float64(32 << 30),
	} {
		if got.body[field] != want {
			t.Errorf("%s = %#v, want %#v", field, got.body[field], want)
		}
	}

	if result.DeviceID != deviceID {
		t.Errorf("result device id = %s, want %s", result.DeviceID, deviceID)
	}
	if !result.Approved {
		t.Error("approved was not carried through")
	}
	if result.DevicePlaneEndpoint != "edgepilot.example.com:9443" {
		t.Errorf("endpoint = %q", result.DevicePlaneEndpoint)
	}
	if want := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC); !result.NotAfter.Equal(want) {
		t.Errorf("not_after = %s, want %s", result.NotAfter, want)
	}
	if len(result.SigningPublicKeys) != 1 || string(result.SigningPublicKeys[0]) != "signing-key" {
		t.Errorf("signing keys did not decode: %q", result.SigningPublicKeys)
	}
}

// A Mac that did not report its core count must not send zero: the schema
// requires at least 1, so a zero would fail validation for a device whose only
// problem is a missing optional fact.
func TestUnknownFactsAreOmittedRatherThanSentAsZero(t *testing.T) {
	t.Parallel()

	handler := &enrollServer{t: t}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newClient(t, handler, srv.URL, "token")
	req, _ := realRequest(t)
	req.CPUCores, req.MemoryBytes, req.DiskTotalBytes = 0, 0, 0
	req.Chip, req.OSBuild = "", ""

	if _, err := client.Enroll(t.Context(), req); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	body := handler.seen[0].body
	for _, field := range []string{"cpu_cores", "memory_bytes", "disk_total_bytes", "chip", "os_build"} {
		if _, present := body[field]; present {
			t.Errorf("%s was sent as %#v; unknown facts must be omitted", field, body[field])
		}
	}
	// The required ones are still there.
	for _, field := range []string{"device_id", "csr", "serial_number", "hostname"} {
		if _, present := body[field]; !present {
			t.Errorf("%s is required and was not sent", field)
		}
	}
}

// --- what to do when it fails ----------------------------------------------

// The distinction that matters operationally: a rejected token will never
// succeed, and an agent that retries it forever turns one bad install into a
// load problem on the server.
func TestErrorsSayWhetherRetryingCouldHelp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status    int
		want      error
		retryable bool
	}{
		{http.StatusUnauthorized, enroll.ErrTokenRejected, false},
		{http.StatusForbidden, enroll.ErrTokenRejected, false},
		{http.StatusConflict, enroll.ErrAlreadyEnrolled, false},
		{http.StatusUnprocessableEntity, enroll.ErrRejected, false},
		{http.StatusBadRequest, enroll.ErrRejected, false},
		{http.StatusTooManyRequests, enroll.ErrUnavailable, true},
		{http.StatusInternalServerError, enroll.ErrUnavailable, true},
		{http.StatusBadGateway, enroll.ErrUnavailable, true},
		{http.StatusServiceUnavailable, enroll.ErrUnavailable, true},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()

			handler := &enrollServer{t: t, status: tc.status}
			srv := httptest.NewServer(handler)
			defer srv.Close()

			client := newClient(t, handler, srv.URL, "token")
			req, _ := realRequest(t)

			_, err := client.Enroll(t.Context(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := enroll.Retryable(err); got != tc.retryable {
				t.Errorf("Retryable = %v, want %v", got, tc.retryable)
			}
		})
	}
}

// The operator reading agent logs at 3 a.m. needs the server's explanation, not
// just a status code.
func TestTheServersExplanationReachesTheLog(t *testing.T) {
	t.Parallel()

	handler := &enrollServer{
		t: t, status: http.StatusUnauthorized,
		problem: map[string]any{
			"title": "Unauthorized", "status": 401,
			"detail": "this enrollment token expired on 2026-06-01",
		},
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newClient(t, handler, srv.URL, "token")
	req, _ := realRequest(t)

	_, err := client.Enroll(t.Context(), req)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "expired on 2026-06-01") {
		t.Errorf("the server's detail is missing from: %v", err)
	}
}

// An unreachable server is the normal condition at a site whose uplink is down,
// and it must be retryable rather than terminal.
func TestAnUnreachableServerIsRetryable(t *testing.T) {
	t.Parallel()

	handler := &enrollServer{t: t}
	srv := httptest.NewServer(handler)
	srv.Close() // nothing is listening now

	client := newClient(t, handler, srv.URL, "token")
	req, _ := realRequest(t)

	_, err := client.Enroll(t.Context(), req)
	if !errors.Is(err, enroll.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if !enroll.Retryable(err) {
		t.Error("an unreachable server must be retryable")
	}
}

// A response missing the certificate is not a successful enrollment, whatever
// status code it carried: saving it would leave a device that believes it is
// enrolled and can never connect.
func TestAResponseWithoutACertificateIsRejected(t *testing.T) {
	t.Parallel()

	cases := map[string]func(map[string]any) any{
		"no certificate": func(req map[string]any) any {
			body := successBody(req)
			delete(body, "certificate_pem")
			return body
		},
		"no CA bundle": func(req map[string]any) any {
			body := successBody(req)
			delete(body, "ca_bundle_pem")
			return body
		},
		"an unparseable device id": func(req map[string]any) any {
			body := successBody(req)
			body["device_id"] = "not-a-uuid"
			return body
		},
		"an unparseable expiry": func(req map[string]any) any {
			body := successBody(req)
			body["not_after"] = "next Tuesday"
			return body
		},
		"signing keys that are not base64": func(req map[string]any) any {
			body := successBody(req)
			body["signing_public_keys"] = []string{"!!!not base64!!!"}
			return body
		},
	}

	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			handler := &enrollServer{t: t, respond: respond}
			srv := httptest.NewServer(handler)
			defer srv.Close()

			client := newClient(t, handler, srv.URL, "token")
			req, _ := realRequest(t)

			if _, err := client.Enroll(t.Context(), req); err == nil {
				t.Error("a response that cannot be used must be rejected")
			}
		})
	}
}

func TestCancellationStopsTheAttempt(t *testing.T) {
	t.Parallel()

	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)

	client, err := enroll.New(enroll.Options{
		BaseURL: srv.URL, Token: "token", Logger: quiet(), HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	req, _ := realRequest(t)
	start := time.Now()
	if _, err := client.Enroll(ctx, req); err == nil {
		t.Fatal("expected a failure")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the attempt ran for %s after its context was cancelled", elapsed)
	}
}

// --- configuration ---------------------------------------------------------

// Operators type the server's address, not its API path. Both have to work, or
// the failure is a 404 that looks like a server problem.
func TestTheBaseURLAcceptsWhatAnOperatorWouldType(t *testing.T) {
	t.Parallel()

	handler := &enrollServer{t: t}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, base := range []string{srv.URL, srv.URL + "/", srv.URL + "/api/v1", srv.URL + "/api/v1/"} {
		client := newClient(t, handler, base, "token")
		req, _ := realRequest(t)
		if _, err := client.Enroll(t.Context(), req); err != nil {
			t.Fatalf("base URL %q: %v", base, err)
		}
	}

	for _, seen := range handler.seen {
		if seen.path != "/api/v1/agent/enroll" {
			t.Errorf("path = %q, want /api/v1/agent/enroll", seen.path)
		}
	}
}

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	cases := map[string]enroll.Options{
		"no base URL":      {Token: "t"},
		"no token":         {BaseURL: "https://example.com"},
		"a blank token":    {BaseURL: "https://example.com", Token: "   "},
		"no scheme":        {BaseURL: "example.com", Token: "t"},
		"the wrong scheme": {BaseURL: "ftp://example.com", Token: "t"},
		"no host":          {BaseURL: "https://", Token: "t"},
		"a bad CA":         {BaseURL: "https://example.com", Token: "t", ServerCAPEM: []byte("not a certificate")},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts.Logger = quiet()
			if _, err := enroll.New(opts); err == nil {
				t.Error("this configuration should have been refused")
			}
		})
	}
}

func TestEnrollValidatesItsOwnRequest(t *testing.T) {
	t.Parallel()

	handler := &enrollServer{t: t}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := newClient(t, handler, srv.URL, "token")

	valid, _ := realRequest(t)

	noDevice := valid
	noDevice.DeviceID = uuid.Nil
	noCSR := valid
	noCSR.CSRDer = nil
	noSerial := valid
	noSerial.SerialNumber = "  "

	for name, req := range map[string]enroll.Request{
		"no device id": noDevice, "no CSR": noCSR, "no serial number": noSerial,
	} {
		if _, err := client.Enroll(t.Context(), req); err == nil {
			t.Errorf("%s: the request should have been refused before it was sent", name)
		}
	}
	if len(handler.seen) != 0 {
		t.Errorf("an invalid request reached the server %d times", len(handler.seen))
	}
}

// --- the token file --------------------------------------------------------

func TestReadTokenFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "enroll")
	// Installers write the token with a trailing newline; the header would be
	// malformed if it survived.
	if err := os.WriteFile(path, []byte("  ep_enroll_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := enroll.ReadTokenFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "ep_enroll_secret" {
		t.Errorf("token = %q, want it trimmed", got)
	}

	if _, err := enroll.ReadTokenFile(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing token file must be an error")
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := enroll.ReadTokenFile(empty); err == nil {
		t.Error("an empty token file must be an error rather than an empty token")
	}
}

// A single-use token left on disk after enrollment is a live credential on a
// machine that may be resold, imaged, or backed up.
func TestTheUsedTokenFileIsRemovable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "enroll")
	if err := os.WriteFile(path, []byte("ep_enroll_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enroll.RemoveTokenFile(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the token file is still there")
	}
	// Removing again is what happens when the agent restarts after a successful
	// enrollment, and must not be an error.
	if err := enroll.RemoveTokenFile(path); err != nil {
		t.Errorf("removing an absent token file should be a no-op, got: %v", err)
	}
}
