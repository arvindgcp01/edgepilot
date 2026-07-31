package ca

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// SPIFFE identity encoding: spiffe://edgepilot/<org-uuid>/<device-uuid>
//
// A URI SAN rather than the Common Name. The CN is a legacy free-text field
// with no structure, no uniqueness guarantee, and inconsistent handling across
// TLS stacks; RFC 6125 has deprecated relying on it for over a decade. The URI
// SAN is what policy engines and service meshes read, and encoding the tenant
// alongside the device means a connection can be scoped before any database
// lookup — which the gRPC interceptor needs, since scoping is what makes the
// lookup safe.

func spiffeURI(orgID, deviceID uuid.UUID) *url.URL {
	return &url.URL{
		Scheme: "spiffe",
		Host:   TrustDomain,
		Path:   "/" + orgID.String() + "/" + deviceID.String(),
	}
}

// parseSPIFFE extracts the organization and device from a certificate's SANs.
//
// Only URI SANs are considered. Falling back to the Common Name would reopen
// exactly the ambiguity the SAN exists to remove — and the CN on these
// certificates is set by us anyway, so a fallback would only ever help a
// certificate we did not issue.
func parseSPIFFE(cert *x509.Certificate) (deviceID, orgID uuid.UUID, err error) {
	for _, u := range cert.URIs {
		if u == nil || u.Scheme != "spiffe" || u.Host != TrustDomain {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 2 {
			continue
		}
		org, orgErr := uuid.Parse(parts[0])
		device, deviceErr := uuid.Parse(parts[1])
		if orgErr != nil || deviceErr != nil {
			continue
		}
		return device, org, nil
	}
	return uuid.Nil, uuid.Nil, fmt.Errorf("%w: no spiffe://%s/<org>/<device> URI SAN",
		ErrNoDeviceIdentity, TrustDomain)
}
