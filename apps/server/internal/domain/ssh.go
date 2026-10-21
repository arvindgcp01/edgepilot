package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SSHKey is an operator's public key, authorised to reach the bastion.
//
// The key authenticates the operator *to EdgePilot*. It does not authenticate
// them to the Mac: the bastion relays an encrypted session, and the Mac's own
// sshd decides who may log in. Both checks have to pass, which is the intended
// arrangement — revoking here severs access to the whole fleet at once, without
// EdgePilot ever holding a credential that grants a shell by itself.
type SSHKey struct {
	ID    uuid.UUID
	OrgID uuid.UUID

	// OwnerUserID is nil for an org-wide service key.
	OwnerUserID *uuid.UUID

	Name      string
	PublicKey string // authorized_keys line
	KeyType   string

	// FingerprintSHA256 is the OpenSSH form, "SHA256:base64…". It is what the
	// bastion looks a key up by, and what an operator sees in `ssh-keygen -lf`.
	FingerprintSHA256 string

	Scope          string // 'org' | 'device'
	TargetUsername string

	ExpiresAt     *time.Time
	LastUsedAt    *time.Time
	RevokedAt     *time.Time
	RevokedReason string

	CreatedBy *uuid.UUID
	CreatedAt time.Time
}

// Active reports whether the key may be used now.
func (k *SSHKey) Active(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return false
	}
	return true
}

// CreateSSHKeyInput registers a public key.
type CreateSSHKeyInput struct {
	OrgID             uuid.UUID
	OwnerUserID       *uuid.UUID
	Name              string
	PublicKey         string
	KeyType           string
	FingerprintSHA256 string
	Scope             string
	TargetUsername    string
	ExpiresAt         *time.Time
	CreatedBy         *uuid.UUID
}

// SSHSession is the audit record for one tunnelled connection.
//
// Metadata only, and deliberately so: the bastion relays bytes it cannot read.
// What is recorded is who connected, to which device, from where, for how long,
// and how much data crossed — everything except the one thing a compromised
// control plane must not be able to hand over.
type SSHSession struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	DeviceID uuid.UUID
	UserID   *uuid.UUID
	SSHKeyID *uuid.UUID

	BastionReplica string
	ClientIP       string
	TargetUsername string

	Status      string // 'active' | 'closed' | 'denied' | 'failed'
	BytesIn     int64
	BytesOut    int64
	CloseReason string

	StartedAt time.Time
	EndedAt   *time.Time
}

// OpenSSHSessionInput starts an audit record.
type OpenSSHSessionInput struct {
	OrgID          uuid.UUID
	DeviceID       uuid.UUID
	UserID         *uuid.UUID
	SSHKeyID       *uuid.UUID
	BastionReplica string
	ClientIP       string
	TargetUsername string
	Status         string
}

// CloseSSHSessionInput completes an audit record.
type CloseSSHSessionInput struct {
	ID          uuid.UUID
	Status      string
	BytesIn     int64
	BytesOut    int64
	CloseReason string
}

// SSHKeyRepository stores operator public keys.
type SSHKeyRepository interface {
	Create(ctx context.Context, in CreateSSHKeyInput) (*SSHKey, error)

	// FindByFingerprint looks a key up across every organization.
	//
	// Unscoped on purpose: the bastion has to identify the caller *before* it
	// knows which organization they belong to, exactly as certificate
	// revocation is checked before any application identity exists. The
	// organization it returns is what scopes everything the session does
	// afterwards.
	FindByFingerprint(ctx context.Context, fingerprint string) (*SSHKey, error)

	// ListForOrg returns the organization's keys, newest first.
	ListForOrg(ctx context.Context) ([]*SSHKey, error)

	// TouchLastUsed records that a key authenticated. Best-effort: a failure
	// here must not deny a session that is otherwise authorised.
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error

	Revoke(ctx context.Context, id uuid.UUID, reason string, at time.Time) error
}

// SSHSessionRepository stores session audit records.
type SSHSessionRepository interface {
	Open(ctx context.Context, in OpenSSHSessionInput) (*SSHSession, error)
	Close(ctx context.Context, in CloseSSHSessionInput) error

	// ListForDevice returns a device's sessions, most recent first.
	ListForDevice(ctx context.Context, deviceID uuid.UUID, limit int) ([]*SSHSession, error)

	// ListActive returns the organization's sessions that are still open.
	ListActive(ctx context.Context) ([]*SSHSession, error)

	// Get returns one session.
	Get(ctx context.Context, id uuid.UUID) (*SSHSession, error)

	// CloseOrphaned ends sessions this replica was holding when it stopped.
	//
	// A relay cannot survive the process that held it, so any row still marked
	// active for this replica at startup is a session that ended without anyone
	// recording it. Left alone they accumulate, and "who is connected right now"
	// stops meaning anything — which is the one question the fleet grid uses
	// this table to answer.
	CloseOrphaned(ctx context.Context, replica string) (int, error)

	// Retarget corrects the account a session logged in as.
	//
	// The browser terminal opens its audit row before it knows which of the
	// registered accounts the device will accept, so the row is written with the
	// first candidate and corrected once one answers.
	Retarget(ctx context.Context, id uuid.UUID, username string) error
}
