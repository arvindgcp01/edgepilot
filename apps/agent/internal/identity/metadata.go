package identity

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// parsedMetadata is metadata with its string fields resolved, so the caller
// never handles a half-parsed record.
type parsedMetadata struct {
	metadata
	deviceID    uuid.UUID
	orgID       uuid.UUID
	notAfter    time.Time
	signingKeys [][]byte
}

func encodeMetadata(id *Identity) ([]byte, error) {
	keys := make([]string, 0, len(id.SigningPublicKeys))
	for _, k := range id.SigningPublicKeys {
		keys = append(keys, base64.StdEncoding.EncodeToString(k))
	}

	meta := metadata{
		DeviceID:            id.DeviceID.String(),
		OrgID:               id.OrgID.String(),
		NotAfter:            id.NotAfter.UTC().Format(time.RFC3339),
		Approved:            id.Approved,
		DevicePlaneEndpoint: id.DevicePlaneEndpoint,
		SigningPublicKeys:   keys,
		EnrolledAt:          time.Now().UTC().Format(time.RFC3339),
	}

	// Indented because a human reads this file during an incident, and a
	// one-line JSON blob in a terminal is not something anyone should have to
	// parse by eye.
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("identity: encode metadata: %w", err)
	}
	return append(out, '\n'), nil
}

func decodeMetadata(raw []byte) (*parsedMetadata, error) {
	var meta metadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("identity: parse metadata: %w", err)
	}

	deviceID, err := uuid.Parse(meta.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("identity: metadata device_id %q: %w", meta.DeviceID, err)
	}
	// org_id is informational — the certificate's SPIFFE SAN is what the server
	// authenticates against — so a missing one is tolerated rather than fatal.
	var orgID uuid.UUID
	if meta.OrgID != "" {
		if orgID, err = uuid.Parse(meta.OrgID); err != nil {
			return nil, fmt.Errorf("identity: metadata org_id %q: %w", meta.OrgID, err)
		}
	}

	var notAfter time.Time
	if meta.NotAfter != "" {
		if notAfter, err = time.Parse(time.RFC3339, meta.NotAfter); err != nil {
			return nil, fmt.Errorf("identity: metadata not_after %q: %w", meta.NotAfter, err)
		}
	}

	keys := make([][]byte, 0, len(meta.SigningPublicKeys))
	for i, encoded := range meta.SigningPublicKeys {
		key, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("identity: metadata signing_public_keys[%d]: %w", i, decodeErr)
		}
		keys = append(keys, key)
	}

	return &parsedMetadata{
		metadata: meta, deviceID: deviceID, orgID: orgID,
		notAfter: notAfter, signingKeys: keys,
	}, nil
}
