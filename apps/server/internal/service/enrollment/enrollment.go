// Package enrollment registers devices and issues their mTLS identity.
//
// This is the one path an unauthenticated agent can reach, so it is worth being
// precise about what an attacker holding a valid enrollment token can and
// cannot do.
//
// They CAN: register a device into the token's organization, and obtain a
// client certificate for it.
//
// They CANNOT: choose which organization (the token decides), impersonate an
// existing device in another tenant (the serial is scoped to the org), obtain a
// certificate that authorises anything (a pending device receives no commands),
// or use the token more times than it permits (redemption is atomic).
//
// That is the containment boundary: a leaked token costs you one unwanted
// device sitting in the approval queue, not access to a fleet.
package enrollment

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
	"github.com/visionify/edgepilot/apps/server/internal/platform/token"
)

// Service enrolls devices and renews their certificates.
type Service struct {
	store domain.Store
	ca    *ca.Authority
	log   *slog.Logger

	// devicePlaneEndpoint is where an enrolled agent is told to connect.
	devicePlaneEndpoint string
	// signingPublicKeys are the Ed25519 keys the agent pins for artifact and
	// script signatures. Delivered at enrollment so the agent has them before
	// it can be asked to install anything.
	signingPublicKeys [][]byte

	renewBefore time.Duration
	now         func() time.Time
}

// Options configures the Service.
type Options struct {
	Store               domain.Store
	CA                  *ca.Authority
	Logger              *slog.Logger
	DevicePlaneEndpoint string
	SigningPublicKeys   [][]byte
	RenewBefore         time.Duration
	Now                 func() time.Time
}

// New builds the Service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("enrollment: Store is required")
	case opts.CA == nil:
		return nil, errors.New("enrollment: CA is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RenewBefore <= 0 {
		opts.RenewBefore = 30 * 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		store:               opts.Store,
		ca:                  opts.CA,
		log:                 opts.Logger,
		devicePlaneEndpoint: opts.DevicePlaneEndpoint,
		signingPublicKeys:   opts.SigningPublicKeys,
		renewBefore:         opts.RenewBefore,
		now:                 opts.Now,
	}, nil
}

// Request is what an agent supplies on first boot.
type Request struct {
	// Token is the plaintext enrollment token.
	Token string
	// DeviceID is the UUIDv7 the agent generated and keeps for its lifetime.
	DeviceID uuid.UUID
	// CSRDer is a DER-encoded PKCS#10 request. Only its public key and
	// signature are used; see the ca package.
	CSRDer []byte

	SerialNumber string
	HardwareUUID string
	Hostname     string

	ModelIdentifier string
	Chip            string
	Arch            string
	CPUCores        *int
	MemoryBytes     *int64
	DiskTotalBytes  *int64
	OSVersion       string
	OSBuild         string
	AgentVersion    string
	LocalIP         string
	PublicIP        string
}

// Result is what the agent receives.
type Result struct {
	DeviceID uuid.UUID
	OrgID    uuid.UUID

	CertificatePEM []byte
	CABundlePEM    []byte
	NotAfter       time.Time

	// Approved is false when the token did not carry auto-approve. The agent
	// connects and reports inventory but is dispatched no commands.
	Approved bool

	DevicePlaneEndpoint string
	SigningPublicKeys   [][]byte
}

// Enroll registers a device and issues its first certificate.
//
// Everything after token redemption happens in one transaction. A device row
// without a certificate can never connect, and a certificate without a device
// row is unreachable; either orphan would need manual cleanup before a retry
// could succeed, and the agent retrying is the normal case on a flaky link.
func (s *Service) Enroll(ctx context.Context, req Request) (*Result, error) {
	// Uniform failure for anything token-related. An agent holding a bad token
	// learns only that it is bad — not whether it was unknown, expired,
	// revoked, or exhausted — so a probe cannot enumerate valid tokens.
	tokenFail := domain.Unauthenticated("invalid_enrollment_token")

	if req.Token == "" || req.DeviceID == uuid.Nil {
		return nil, tokenFail
	}
	// Validate the CSR before spending a token redemption on it. Redemption
	// increments a use count, and a malformed request should not consume one of
	// a single-use token's uses.
	if _, err := ca.ParseAndVerifyCSR(req.CSRDer); err != nil {
		return nil, domain.Invalid("/csr", "%s", csrMessage(err))
	}

	enrollToken, err := s.store.EnrollmentTokens().Redeem(ctx, token.HashEnrollmentToken(req.Token), s.now())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, tokenFail
		}
		return nil, err
	}

	// The token decides the tenant. Nothing the agent sent influences this —
	// which is what stops a device enrolling itself into someone else's fleet.
	orgID := enrollToken.OrgID
	scoped := domain.WithOrg(ctx, orgID)

	in := domain.EnrollDeviceInput{
		DeviceID:        req.DeviceID,
		OrgID:           orgID,
		SerialNumber:    req.SerialNumber,
		HardwareUUID:    req.HardwareUUID,
		Hostname:        req.Hostname,
		ModelIdentifier: req.ModelIdentifier,
		Chip:            req.Chip,
		Arch:            req.Arch,
		CPUCores:        req.CPUCores,
		MemoryBytes:     req.MemoryBytes,
		DiskTotalBytes:  req.DiskTotalBytes,
		OSVersion:       req.OSVersion,
		OSBuild:         req.OSBuild,
		AgentVersion:    req.AgentVersion,
		LocalIP:         req.LocalIP,
		PublicIP:        req.PublicIP,
		Labels:          enrollToken.Labels,
		EnrolledByToken: &enrollToken.ID,
		AutoApprove:     enrollToken.AutoApprove,
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}

	var (
		device *domain.Device
		issued *ca.Issued
	)
	err = s.store.InTx(scoped, func(tx domain.Store) error {
		device, err = tx.Devices().Enroll(scoped, in)
		if err != nil {
			return err
		}

		// The device id comes from the record just written, not from the
		// request — so even if enrollment reclaimed an existing row for this
		// serial, the certificate names the device we actually have.
		issued, err = s.ca.Sign(req.CSRDer, device.ID, device.OrgID)
		if err != nil {
			return domain.Internal(err)
		}

		// Re-enrollment supersedes the previous identity. Leaving the old
		// certificate live would mean a Mac that was re-imaged — or whose key
		// was copied off it before a rebuild — could still connect.
		if _, err := tx.DeviceCertificates().RevokeAllForDevice(scoped, device.ID, "superseded by re-enrollment"); err != nil {
			return err
		}

		_, err = tx.DeviceCertificates().Issue(scoped, &domain.DeviceCertificate{
			DeviceID:          device.ID,
			Serial:            issued.SerialDecimal,
			FingerprintSHA256: issued.FingerprintSHA256,
			PublicKeyDER:      issued.PublicKeyDER,
			CertificatePEM:    string(issued.CertificatePEM),
			NotBefore:         issued.NotBefore,
			NotAfter:          issued.NotAfter,
			IssuedVia:         "enrollment",
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "device enrolled",
		slog.String("device_id", device.ID.String()),
		slog.String("org_id", orgID.String()),
		slog.String("serial_number", device.SerialNumber),
		slog.Bool("approved", device.Approved()),
		slog.String("token_prefix", enrollToken.TokenPrefix))

	return &Result{
		DeviceID:            device.ID,
		OrgID:               orgID,
		CertificatePEM:      issued.CertificatePEM,
		CABundlePEM:         s.ca.ChainPEM(),
		NotAfter:            issued.NotAfter,
		Approved:            device.Approved(),
		DevicePlaneEndpoint: s.devicePlaneEndpoint,
		SigningPublicKeys:   s.signingPublicKeys,
	}, nil
}

// RenewRequest asks for a fresh certificate for an already-authenticated device.
type RenewRequest struct {
	// DeviceID and OrgID come from the peer certificate on the live stream,
	// never from the message body — the whole point of renewing over an
	// authenticated connection is that identity is already established.
	DeviceID uuid.UUID
	OrgID    uuid.UUID
	CSRDer   []byte
}

// Renew issues a replacement certificate.
//
// No enrollment token and no human involvement: the device proves who it is by
// the certificate it is already using. That is what makes a 90-day lifetime
// practical — a longer one would only exist to avoid an operational burden that
// this removes.
//
// The previous certificate is NOT revoked. The agent has to receive the new one
// and swap it in; killing the old one first would disconnect the device mid-
// renewal and leave it unable to retry. Both are briefly valid, and the old one
// expires on its own.
func (s *Service) Renew(ctx context.Context, req RenewRequest) (*Result, error) {
	if req.DeviceID == uuid.Nil || req.OrgID == uuid.Nil {
		return nil, domain.Internal(errors.New("enrollment: renew requires an authenticated device identity"))
	}
	if _, err := ca.ParseAndVerifyCSR(req.CSRDer); err != nil {
		return nil, domain.Invalid("/csr", "%s", csrMessage(err))
	}

	scoped := domain.WithOrg(ctx, req.OrgID)

	device, err := s.store.Devices().Get(scoped, req.DeviceID)
	if err != nil {
		return nil, err
	}
	// A revoked or rejected device holds a certificate that is about to stop
	// working. Renewing it would undo the revocation.
	if device.EnrollmentState == domain.EnrollmentRevoked ||
		device.EnrollmentState == domain.EnrollmentRejected {
		return nil, domain.Forbidden("device:enroll")
	}

	issued, err := s.ca.Sign(req.CSRDer, device.ID, device.OrgID)
	if err != nil {
		return nil, domain.Internal(err)
	}

	if _, err := s.store.DeviceCertificates().Issue(scoped, &domain.DeviceCertificate{
		DeviceID:          device.ID,
		Serial:            issued.SerialDecimal,
		FingerprintSHA256: issued.FingerprintSHA256,
		PublicKeyDER:      issued.PublicKeyDER,
		CertificatePEM:    string(issued.CertificatePEM),
		NotBefore:         issued.NotBefore,
		NotAfter:          issued.NotAfter,
		IssuedVia:         "renewal",
	}); err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "device certificate renewed",
		slog.String("device_id", device.ID.String()),
		slog.Time("not_after", issued.NotAfter))

	return &Result{
		DeviceID:            device.ID,
		OrgID:               device.OrgID,
		CertificatePEM:      issued.CertificatePEM,
		CABundlePEM:         s.ca.ChainPEM(),
		NotAfter:            issued.NotAfter,
		Approved:            device.Approved(),
		DevicePlaneEndpoint: s.devicePlaneEndpoint,
		SigningPublicKeys:   s.signingPublicKeys,
	}, nil
}

// Approve moves a pending device to approved or rejected.
func (s *Service) Approve(ctx context.Context, deviceID uuid.UUID, approve bool) (*domain.Device, error) {
	principal, ok := domain.PrincipalFromContext(ctx)
	if !ok {
		return nil, domain.Unauthenticated("not_authenticated")
	}
	if err := principal.Authorize(domain.PermDeviceApprove); err != nil {
		return nil, err
	}

	device, err := s.store.Devices().Get(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if device.EnrollmentState != domain.EnrollmentPending {
		return nil, domain.Conflict("device_not_pending",
			"This device is %s, not awaiting approval.", device.EnrollmentState)
	}

	state := domain.EnrollmentApproved
	if !approve {
		state = domain.EnrollmentRejected
	}

	updated, err := s.store.Devices().SetEnrollmentState(ctx, deviceID, state)
	if err != nil {
		return nil, err
	}

	// A rejected device must not keep a usable identity. Leaving its
	// certificate live would let it reconnect and keep reporting, which is
	// precisely what rejecting it was meant to stop.
	if !approve {
		if _, err := s.store.DeviceCertificates().RevokeAllForDevice(ctx, deviceID, "enrollment rejected"); err != nil {
			return nil, err
		}
	}

	s.log.InfoContext(ctx, "device enrollment decision",
		slog.String("device_id", deviceID.String()),
		slog.String("decision", string(state)),
		slog.String("decided_by", principal.UserID.String()))

	return updated, nil
}

// NeedsRenewal reports whether a device should be told to renew.
func (s *Service) NeedsRenewal(cert *domain.DeviceCertificate) bool {
	return cert.NeedsRenewal(s.now(), s.renewBefore)
}

// csrMessage turns a CSR failure into something an agent author can act on,
// without echoing parser internals.
func csrMessage(err error) string {
	switch {
	case errors.Is(err, ca.ErrCSRSignature):
		return "the signature does not match the enclosed public key"
	case errors.Is(err, ca.ErrWeakKey):
		return err.Error()
	default:
		return "is not a valid DER-encoded PKCS#10 certificate request"
	}
}
