// Package enroll obtains the device's first certificate.
//
// This is the only user-plane endpoint an agent ever calls. Everything else goes
// over the mTLS device plane — which an unenrolled agent cannot reach, because
// it has no client certificate yet. That chicken-and-egg is the whole reason
// this package exists.
//
// # What is being trusted, and when
//
// At this moment the agent trusts two things and nothing else: the enrollment
// token it was installed with, and the TLS identity of the server it was told to
// call. After this exchange it trusts a CA bundle issued to it, and the token is
// never used again.
//
// So the server's TLS certificate matters more here than anywhere else in the
// system. A self-hosted deployment with a private CA supplies that CA at install
// time (`EP_SERVER_CA_FILE`); one with a publicly-trusted certificate uses the
// system roots. What the agent will not do is skip verification — there is no
// insecure mode, because the one moment worth attacking is this one.
package enroll

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors that decide whether retrying is worth anything.
var (
	// ErrTokenRejected means the token is missing, expired, revoked, or
	// exhausted. Terminal: retrying with the same token cannot succeed, and an
	// agent that hammers the endpoint turns one bad install into a load problem.
	ErrTokenRejected = errors.New("enroll: the enrollment token was rejected")

	// ErrAlreadyEnrolled means this serial number belongs to a live device.
	// Terminal for the same reason, and it usually means a re-image that needs
	// the old device decommissioned first.
	ErrAlreadyEnrolled = errors.New("enroll: this serial number is already enrolled")

	// ErrRejected means the server refused the request itself — a malformed
	// CSR, a serial number that fails validation. Terminal.
	ErrRejected = errors.New("enroll: the server rejected the enrollment request")

	// ErrUnavailable means the server could not answer right now. Retryable.
	ErrUnavailable = errors.New("enroll: the server is unavailable")
)

// Request is what the agent sends.
type Request struct {
	DeviceID uuid.UUID
	CSRDer   []byte

	SerialNumber string
	HardwareUUID string
	Hostname     string

	ModelIdentifier string
	Chip            string
	Arch            string
	CPUCores        int
	MemoryBytes     int64
	DiskTotalBytes  int64
	OSVersion       string
	OSBuild         string
	AgentVersion    string
	LocalIP         string
}

// Result is what the server returns.
type Result struct {
	DeviceID uuid.UUID
	OrgID    uuid.UUID

	CertificatePEM []byte
	CABundlePEM    []byte
	NotAfter       time.Time

	Approved            bool
	DevicePlaneEndpoint string
	SigningPublicKeys   [][]byte
}

// Client calls the enrollment endpoint.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	log     *slog.Logger
}

// Options configures a Client.
type Options struct {
	// BaseURL is the server root, with or without the /api/v1 suffix.
	BaseURL string
	// Token is the plaintext enrollment token.
	Token string
	// ServerCAPEM pins the CA that signed the server's TLS certificate. Empty
	// means the system roots, which is right for a publicly-trusted certificate
	// and wrong for a private one.
	ServerCAPEM []byte
	// Timeout bounds one attempt. Zero means 30 seconds.
	Timeout time.Duration
	// Logger receives attempt-level detail.
	Logger *slog.Logger
	// HTTPClient overrides the transport. Used by tests to reach a local
	// server; production leaves it nil so the pinning above applies.
	HTTPClient *http.Client
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("enroll: BaseURL is required")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("enroll: an enrollment token is required")
	}
	base, err := normalizeBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		// Cloned from the default so the connection pooling, proxy handling,
		// and timeouts stay standard; only the TLS configuration is ours. The
		// assertion is checked because a future stdlib change to
		// DefaultTransport's type would otherwise panic at startup on every
		// device at once.
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("enroll: unexpected default transport %T",
				http.DefaultTransport)
		}
		transport := base.Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

		if len(opts.ServerCAPEM) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(opts.ServerCAPEM) {
				return nil, errors.New("enroll: ServerCAPEM contains no usable certificate")
			}
			// Replaces the system roots rather than adding to them. A
			// deployment that pins its own CA does not also want to accept a
			// certificate from any public CA for the same hostname.
			transport.TLSClientConfig.RootCAs = pool
		}
		httpClient = &http.Client{Transport: transport}
	}
	httpClient.Timeout = opts.Timeout

	return &Client{
		baseURL: base,
		token:   strings.TrimSpace(opts.Token),
		http:    httpClient,
		log:     opts.Logger,
	}, nil
}

// wireRequest mirrors the request schema in openapi.yaml. Written out rather
// than reusing a shared struct: the agent and the server are separate modules on
// purpose, and this is the contract between them.
type wireRequest struct {
	DeviceID        string `json:"device_id"`
	CSR             string `json:"csr"`
	SerialNumber    string `json:"serial_number"`
	HardwareUUID    string `json:"hardware_uuid,omitempty"`
	Hostname        string `json:"hostname"`
	OSVersion       string `json:"os_version,omitempty"`
	OSBuild         string `json:"os_build,omitempty"`
	ModelIdentifier string `json:"model_identifier,omitempty"`
	Chip            string `json:"chip,omitempty"`
	Arch            string `json:"arch,omitempty"`
	CPUCores        *int   `json:"cpu_cores,omitempty"`
	MemoryBytes     *int64 `json:"memory_bytes,omitempty"`
	DiskTotalBytes  *int64 `json:"disk_total_bytes,omitempty"`
	AgentVersion    string `json:"agent_version,omitempty"`
	LocalIP         string `json:"local_ip,omitempty"`
}

type wireResponse struct {
	DeviceID            string   `json:"device_id"`
	OrgID               string   `json:"org_id"`
	CertificatePEM      string   `json:"certificate_pem"`
	CABundlePEM         string   `json:"ca_bundle_pem"`
	NotAfter            string   `json:"not_after"`
	Approved            bool     `json:"approved"`
	DevicePlaneEndpoint string   `json:"device_plane_endpoint"`
	SigningPublicKeys   []string `json:"signing_public_keys"`
}

// problem is RFC 9457, which is what the server returns on every error.
type problem struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

// Enroll performs one attempt.
//
// Retrying is the caller's decision, because only the caller knows whether it is
// worth waiting: the errors above distinguish "try again in a minute" from "this
// will never work".
func (c *Client) Enroll(ctx context.Context, req Request) (*Result, error) {
	if req.DeviceID == uuid.Nil {
		return nil, errors.New("enroll: a device id is required")
	}
	if len(req.CSRDer) == 0 {
		return nil, errors.New("enroll: a CSR is required")
	}
	if strings.TrimSpace(req.SerialNumber) == "" {
		return nil, errors.New("enroll: a serial number is required")
	}

	body, err := json.Marshal(toWire(req))
	if err != nil {
		return nil, fmt.Errorf("enroll: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("enroll: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("User-Agent", "edgepilot-agent/"+req.AgentVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure is the network, not a decision: retryable.
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() {
		// Drained before closing so the connection can be reused, and capped so
		// a hostile or broken server cannot make the agent read forever.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	// 1 MiB: a certificate and a chain are a few kilobytes. Anything larger is
	// not an enrollment response.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response: %w", ErrUnavailable, err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, classify(resp.StatusCode, raw)
	}

	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("enroll: parse response: %w", err)
	}
	return fromWire(wire)
}

// Retryable reports whether waiting and trying again could help.
//
// A device that keeps retrying a rejected token is a device that never enrolls
// and generates load doing it, so the distinction has to be explicit.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrTokenRejected),
		errors.Is(err, ErrAlreadyEnrolled),
		errors.Is(err, ErrRejected):
		return false
	default:
		return true
	}
}

// ReadTokenFile reads the token an installer left behind.
func ReadTokenFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("enroll: read token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("enroll: token file %s is empty", path)
	}
	return token, nil
}

// RemoveTokenFile deletes the token once it has been used.
//
// A single-use token sitting on disk after enrollment is a live credential on a
// machine that may be resold, imaged, or backed up. The certificate has replaced
// it by this point, so there is nothing to lose by removing it — and a failure
// to remove is worth surfacing rather than swallowing.
func RemoveTokenFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("enroll: remove the used token file %s: %w", path, err)
	}
	return nil
}

func toWire(req Request) wireRequest {
	out := wireRequest{
		DeviceID:        req.DeviceID.String(),
		CSR:             base64.StdEncoding.EncodeToString(req.CSRDer),
		SerialNumber:    req.SerialNumber,
		HardwareUUID:    req.HardwareUUID,
		Hostname:        req.Hostname,
		OSVersion:       req.OSVersion,
		OSBuild:         req.OSBuild,
		ModelIdentifier: req.ModelIdentifier,
		Chip:            req.Chip,
		Arch:            req.Arch,
		AgentVersion:    req.AgentVersion,
		LocalIP:         req.LocalIP,
	}
	// Omitted rather than sent as zero: the schema constrains cpu_cores to at
	// least 1, and a zero would fail validation on a machine that merely did not
	// report its core count.
	if req.CPUCores > 0 {
		cores := req.CPUCores
		out.CPUCores = &cores
	}
	if req.MemoryBytes > 0 {
		mem := req.MemoryBytes
		out.MemoryBytes = &mem
	}
	if req.DiskTotalBytes > 0 {
		disk := req.DiskTotalBytes
		out.DiskTotalBytes = &disk
	}
	return out
}

func fromWire(wire wireResponse) (*Result, error) {
	deviceID, err := uuid.Parse(wire.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("enroll: response device_id %q: %w", wire.DeviceID, err)
	}
	var orgID uuid.UUID
	if wire.OrgID != "" {
		if orgID, err = uuid.Parse(wire.OrgID); err != nil {
			return nil, fmt.Errorf("enroll: response org_id %q: %w", wire.OrgID, err)
		}
	}
	if strings.TrimSpace(wire.CertificatePEM) == "" {
		return nil, errors.New("enroll: the server returned no certificate")
	}
	if strings.TrimSpace(wire.CABundlePEM) == "" {
		return nil, errors.New("enroll: the server returned no CA bundle")
	}

	var notAfter time.Time
	if wire.NotAfter != "" {
		if notAfter, err = time.Parse(time.RFC3339, wire.NotAfter); err != nil {
			return nil, fmt.Errorf("enroll: response not_after %q: %w", wire.NotAfter, err)
		}
	}

	keys := make([][]byte, 0, len(wire.SigningPublicKeys))
	for i, encoded := range wire.SigningPublicKeys {
		key, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("enroll: response signing_public_keys[%d]: %w", i, decodeErr)
		}
		keys = append(keys, key)
	}

	return &Result{
		DeviceID:            deviceID,
		OrgID:               orgID,
		CertificatePEM:      []byte(wire.CertificatePEM),
		CABundlePEM:         []byte(wire.CABundlePEM),
		NotAfter:            notAfter,
		Approved:            wire.Approved,
		DevicePlaneEndpoint: wire.DevicePlaneEndpoint,
		SigningPublicKeys:   keys,
	}, nil
}

// classify turns a status code into an error the caller can branch on.
func classify(status int, body []byte) error {
	detail := describeProblem(body)

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrTokenRejected, detail)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrAlreadyEnrolled, detail)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s", ErrRejected, detail)
	case http.StatusTooManyRequests:
		// The server is shedding load, which is exactly the case for backing
		// off and trying again.
		return fmt.Errorf("%w: rate limited: %s", ErrUnavailable, detail)
	default:
		if status >= 500 {
			return fmt.Errorf("%w: server returned %d: %s", ErrUnavailable, status, detail)
		}
		return fmt.Errorf("%w: unexpected status %d: %s", ErrRejected, status, detail)
	}
}

// describeProblem extracts something human-readable from an RFC 9457 body,
// falling back to the raw text for a server that returned something else.
func describeProblem(body []byte) string {
	var p problem
	if err := json.Unmarshal(body, &p); err == nil {
		switch {
		case p.Detail != "":
			return p.Detail
		case p.Title != "":
			return p.Title
		}
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "no detail"
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// normalizeBaseURL accepts a server root with or without the API prefix.
//
// Operators type the server's address, not its API path, and an agent that
// silently produces /agent/enroll against the wrong root fails with a 404 that
// looks like a server problem.
func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("enroll: BaseURL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("enroll: BaseURL %q must be http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("enroll: BaseURL %q has no host", raw)
	}

	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/api/v1") {
		path += "/api/v1"
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
