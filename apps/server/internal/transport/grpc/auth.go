// Package grpc is EdgePilot's device plane: the mTLS gRPC listener every agent
// holds a stream to.
//
// Device authentication happens at the TLS layer, not in application code. By
// the time a handler runs, the peer has already proved possession of a private
// key matching a certificate this CA issued, and the identity has been read out
// of that certificate's SPIFFE SAN. There is no device token to validate, and
// no code path that can forget to.
package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/visionify/edgepilot/apps/server/internal/platform/ca"
)

// RevocationChecker reports whether a certificate has been revoked.
//
// Declared here, in the package that consumes it, and satisfied by
// service/revocation. It must be fast: this runs synchronously inside the TLS
// handshake.
type RevocationChecker interface {
	IsRevoked(fingerprint []byte) bool
}

// deviceIdentityKey carries the authenticated device through the request
// context.
type deviceIdentityKey struct{}

// WithDeviceIdentity attaches an authenticated device identity.
func WithDeviceIdentity(ctx context.Context, id *ca.Identity) context.Context {
	return context.WithValue(ctx, deviceIdentityKey{}, id)
}

// DeviceIdentityFromContext returns the authenticated device.
//
// The boolean is not decoration: a handler reaching for an identity that is not
// there has been reached by something other than an authenticated agent, and
// must refuse rather than proceed with a zero-valued device.
func DeviceIdentityFromContext(ctx context.Context) (*ca.Identity, bool) {
	id, ok := ctx.Value(deviceIdentityKey{}).(*ca.Identity)
	return id, ok && id != nil
}

// TLSConfig builds the device plane's TLS configuration.
//
// Three things it does that a default configuration would not:
//
//   - RequireAndVerifyClientCert. Not VerifyClientCertIfGiven — an agent
//     without a certificate must fail the handshake, not reach application code
//     as an anonymous peer that something downstream has to remember to reject.
//
//   - TLS 1.3 minimum. Every agent is software we ship, so there is no legacy
//     client to accommodate, and 1.3 removes renegotiation and the cipher
//     negotiation weaknesses along with it.
//
//   - Revocation checked during the handshake. A revoked device is refused at
//     the transport layer and never reaches a stream, so revocation does not
//     depend on every handler remembering to check.
func TLSConfig(authority *ca.Authority, serverCert tls.Certificate, revoked RevocationChecker, log *slog.Logger) *tls.Config {
	if log == nil {
		log = slog.Default()
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    authority.Pool(),
		MinVersion:   tls.VersionTLS13,

		// Runs after the standard chain verification has already succeeded, so
		// this only has to add what x509 does not know about: our revocation
		// state and our identity encoding.
		VerifyPeerCertificate: func(_ [][]byte, verifiedChains [][]*x509.Certificate) error {
			if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
				return errors.New("no verified certificate chain")
			}
			leaf := verifiedChains[0][0]

			identity, err := authority.IdentityFromChain(verifiedChains[0])
			if err != nil {
				// A certificate this CA issued always carries a SPIFFE SAN, so
				// reaching here means either a certificate from elsewhere that
				// somehow chained, or one issued before the SAN existed.
				log.Warn("rejecting peer certificate with no device identity",
					slog.String("subject", leaf.Subject.CommonName),
					slog.Any("error", err))
				return fmt.Errorf("device identity: %w", err)
			}

			if revoked != nil && revoked.IsRevoked(identity.FingerprintSHA256) {
				log.Warn("rejecting revoked device certificate",
					slog.String("device_id", identity.DeviceID.String()),
					slog.String("org_id", identity.OrgID.String()))
				return errors.New("certificate has been revoked")
			}
			return nil
		},
	}
}

// Credentials wraps the TLS config for the gRPC server.
func Credentials(cfg *tls.Config) credentials.TransportCredentials {
	return credentials.NewTLS(cfg)
}

// AuthInterceptor extracts the device identity from the peer certificate and
// puts it in the context.
//
// The identity is re-derived here rather than smuggled out of the TLS callback
// through shared state: a callback has no per-connection place to put it, and
// anything global would be a race between concurrent handshakes.
type AuthInterceptor struct {
	authority *ca.Authority
	log       *slog.Logger
}

// NewAuthInterceptor builds the interceptor.
func NewAuthInterceptor(authority *ca.Authority, log *slog.Logger) *AuthInterceptor {
	if log == nil {
		log = slog.Default()
	}
	return &AuthInterceptor{authority: authority, log: log}
}

// Stream is the gRPC stream interceptor.
func (a *AuthInterceptor) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		identity, err := a.identityFrom(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &identityStream{ServerStream: ss, ctx: WithDeviceIdentity(ss.Context(), identity)})
	}
}

// Unary is the gRPC unary interceptor.
func (a *AuthInterceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		identity, err := a.identityFrom(ctx)
		if err != nil {
			return nil, err
		}
		return handler(WithDeviceIdentity(ctx, identity), req)
	}
}

func (a *AuthInterceptor) identityFrom(ctx context.Context) (*ca.Identity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer information")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		// A plaintext connection reached the device plane. That is a
		// misconfiguration rather than an attack, but it must not be served.
		return nil, status.Error(codes.Unauthenticated, "connection is not mutually authenticated")
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no verified client certificate")
	}

	identity, err := a.authority.IdentityFromChain(tlsInfo.State.VerifiedChains[0])
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "client certificate carries no device identity")
	}
	return identity, nil
}

// identityStream carries the decorated context through to the handler.
type identityStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *identityStream) Context() context.Context { return s.ctx }

// DeviceOrOrg is a small helper for logging and scoping.
func DeviceOrOrg(id *ca.Identity) (device, org uuid.UUID) {
	if id == nil {
		return uuid.Nil, uuid.Nil
	}
	return id.DeviceID, id.OrgID
}
