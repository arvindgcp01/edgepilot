package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// SSHKeys returns the operator public-key repository.
func (s *Store) SSHKeys() domain.SSHKeyRepository { return &sshKeyRepo{s} }

// SSHSessions returns the session audit repository.
func (s *Store) SSHSessions() domain.SSHSessionRepository { return &sshSessionRepo{s} }

// These two repositories are hand-written rather than generated, because they
// arrived with the bastion and regenerating the whole sqlc surface for two
// tables would touch far more than it needs to. The transaction and role
// discipline is identical — see withTx — and that is the part that matters.

type sshKeyRepo struct{ s *Store }

const sshKeyColumns = `
	id, org_id, owner_user_id, name, public_key, key_type, fingerprint_sha256,
	scope, target_username, expires_at, last_used_at, revoked_at,
	COALESCE(revoked_reason, ''), created_by, created_at`

func scanSSHKey(row pgx.Row) (*domain.SSHKey, error) {
	var k domain.SSHKey
	err := row.Scan(&k.ID, &k.OrgID, &k.OwnerUserID, &k.Name, &k.PublicKey,
		&k.KeyType, &k.FingerprintSHA256, &k.Scope, &k.TargetUsername,
		&k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt, &k.RevokedReason,
		&k.CreatedBy, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (r *sshKeyRepo) Create(ctx context.Context, in domain.CreateSSHKeyInput) (*domain.SSHKey, error) {
	if in.Scope == "" {
		in.Scope = "device"
	}
	if in.TargetUsername == "" {
		in.TargetUsername = "edgepilot"
	}

	var key *domain.SSHKey
	err := r.s.rawScoped(ctx, in.OrgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO ssh_keys (org_id, owner_user_id, name, public_key, key_type,
			                      fingerprint_sha256, scope, target_username,
			                      expires_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+sshKeyColumns,
			in.OrgID, in.OwnerUserID, in.Name, in.PublicKey, in.KeyType,
			in.FingerprintSHA256, in.Scope, in.TargetUsername,
			in.ExpiresAt, in.CreatedBy)

		k, err := scanSSHKey(row)
		if err != nil {
			return translate(err, "ssh key")
		}
		key = k
		return nil
	})
	return key, err
}

func (r *sshKeyRepo) FindByFingerprint(ctx context.Context, fingerprint string) (*domain.SSHKey, error) {
	var key *domain.SSHKey
	// Unscoped: the bastion must identify the caller before it knows their
	// organization. See the interface comment.
	err := r.s.rawUnscoped(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+sshKeyColumns+` FROM ssh_keys WHERE fingerprint_sha256 = $1`,
			fingerprint)

		k, err := scanSSHKey(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("ssh key", fingerprint)
		}
		if err != nil {
			return translate(err, "ssh key")
		}
		key = k
		return nil
	})
	return key, err
}

func (r *sshKeyRepo) ListForOrg(ctx context.Context) ([]*domain.SSHKey, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before listing ssh keys"))
	}

	var keys []*domain.SSHKey
	err := r.s.rawScoped(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+sshKeyColumns+` FROM ssh_keys ORDER BY created_at DESC`)
		if err != nil {
			return translate(err, "ssh key")
		}
		defer rows.Close()

		for rows.Next() {
			k, scanErr := scanSSHKey(rows)
			if scanErr != nil {
				return translate(scanErr, "ssh key")
			}
			keys = append(keys, k)
		}
		return translate(rows.Err(), "ssh key")
	})
	return keys, err
}

func (r *sshKeyRepo) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return r.s.rawUnscoped(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE ssh_keys SET last_used_at = $2 WHERE id = $1`, id, at)
		return translate(err, "ssh key")
	})
}

func (r *sshKeyRepo) Revoke(ctx context.Context, id uuid.UUID, reason string, at time.Time) error {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before revoking an ssh key"))
	}

	return r.s.rawScoped(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE ssh_keys
			   SET revoked_at = $2, revoked_reason = $3
			 WHERE id = $1 AND revoked_at IS NULL`, id, at, reason)
		if err != nil {
			return translate(err, "ssh key")
		}
		if tag.RowsAffected() == 0 {
			return domain.NotFound("ssh key", id.String())
		}
		return nil
	})
}

type sshSessionRepo struct{ s *Store }

const sshSessionColumns = `
	id, org_id, device_id, user_id, ssh_key_id, bastion_replica,
	COALESCE(host(client_ip), ''), target_username, status, bytes_in, bytes_out,
	COALESCE(close_reason, ''), started_at, ended_at`

func scanSSHSession(row pgx.Row) (*domain.SSHSession, error) {
	var s domain.SSHSession
	err := row.Scan(&s.ID, &s.OrgID, &s.DeviceID, &s.UserID, &s.SSHKeyID,
		&s.BastionReplica, &s.ClientIP, &s.TargetUsername, &s.Status,
		&s.BytesIn, &s.BytesOut, &s.CloseReason, &s.StartedAt, &s.EndedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *sshSessionRepo) Open(ctx context.Context, in domain.OpenSSHSessionInput) (*domain.SSHSession, error) {
	if in.Status == "" {
		in.Status = "active"
	}

	// A denied session is recorded too, so an audit that shows only successful
	// access is not mistaken for the whole picture.
	var clientIP *string
	if in.ClientIP != "" {
		clientIP = &in.ClientIP
	}

	var session *domain.SSHSession
	err := r.s.rawScoped(ctx, in.OrgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO ssh_sessions (org_id, device_id, user_id, ssh_key_id,
			                          bastion_replica, client_ip, target_username, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+sshSessionColumns,
			in.OrgID, in.DeviceID, in.UserID, in.SSHKeyID,
			in.BastionReplica, clientIP, in.TargetUsername, in.Status)

		s, err := scanSSHSession(row)
		if err != nil {
			return translate(err, "ssh session")
		}
		session = s
		return nil
	})
	return session, err
}

func (r *sshSessionRepo) Close(ctx context.Context, in domain.CloseSSHSessionInput) error {
	if in.Status == "" {
		in.Status = "closed"
	}
	// Unscoped: a session is closed from the bastion's teardown path, which runs
	// after the operator's context is gone.
	return r.s.rawUnscoped(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE ssh_sessions
			   SET status = $2, bytes_in = $3, bytes_out = $4,
			       close_reason = $5, ended_at = now()
			 WHERE id = $1 AND ended_at IS NULL`,
			in.ID, in.Status, in.BytesIn, in.BytesOut, in.CloseReason)
		return translate(err, "ssh session")
	})
}

func (r *sshSessionRepo) ListForDevice(ctx context.Context, deviceID uuid.UUID, limit int) ([]*domain.SSHSession, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before listing ssh sessions"))
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var sessions []*domain.SSHSession
	err := r.s.rawScoped(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+sshSessionColumns+`
			   FROM ssh_sessions WHERE device_id = $1
			  ORDER BY started_at DESC LIMIT $2`, deviceID, limit)
		if err != nil {
			return translate(err, "ssh session")
		}
		defer rows.Close()

		for rows.Next() {
			s, scanErr := scanSSHSession(rows)
			if scanErr != nil {
				return translate(scanErr, "ssh session")
			}
			sessions = append(sessions, s)
		}
		return translate(rows.Err(), "ssh session")
	})
	return sessions, err
}

func (r *sshSessionRepo) ListActive(ctx context.Context) ([]*domain.SSHSession, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before listing ssh sessions"))
	}

	var sessions []*domain.SSHSession
	err := r.s.rawScoped(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+sshSessionColumns+`
			   FROM ssh_sessions WHERE status = 'active'
			  ORDER BY started_at DESC`)
		if err != nil {
			return translate(err, "ssh session")
		}
		defer rows.Close()

		for rows.Next() {
			s, scanErr := scanSSHSession(rows)
			if scanErr != nil {
				return translate(scanErr, "ssh session")
			}
			sessions = append(sessions, s)
		}
		return translate(rows.Err(), "ssh session")
	})
	return sessions, err
}

func (r *sshSessionRepo) Get(ctx context.Context, id uuid.UUID) (*domain.SSHSession, error) {
	orgID, ok := domain.OrgFromContext(ctx)
	if !ok {
		return nil, domain.Internal(errors.New(
			"tenant scope missing from context: call domain.WithOrg before reading an ssh session"))
	}

	var session *domain.SSHSession
	err := r.s.rawScoped(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+sshSessionColumns+` FROM ssh_sessions WHERE id = $1`, id)
		s, err := scanSSHSession(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("ssh session", id.String())
		}
		if err != nil {
			return translate(err, "ssh session")
		}
		session = s
		return nil
	})
	return session, err
}

func (r *sshSessionRepo) CloseOrphaned(ctx context.Context, replica string) (int, error) {
	var closed int
	err := r.s.rawUnscoped(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE ssh_sessions
			   SET status = 'closed', ended_at = now(),
			       close_reason = 'the server restarted while this session was open'
			 WHERE status = 'active' AND bastion_replica = $1`, replica)
		if err != nil {
			return translate(err, "ssh session")
		}
		closed = int(tag.RowsAffected())
		return nil
	})
	return closed, err
}

func (r *sshSessionRepo) Retarget(ctx context.Context, id uuid.UUID, username string) error {
	// Unscoped, like Close: this runs from the terminal's own path after the
	// login has been decided, and only ever narrows a row it already created.
	return r.s.rawUnscoped(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE ssh_sessions SET target_username = $2 WHERE id = $1`, id, username)
		return translate(err, "ssh session")
	})
}

// rawScoped runs fn in a tenant-scoped transaction with the restricted role.
//
// The sqlc equivalents take a *sqlcgen.Queries; these two repositories need the
// transaction itself because their statements are hand-written. The role and
// scope handling is the same, and must stay that way — see setOrgScope for why
// assuming the role inside the transaction is what makes RLS load-bearing.
func (s *Store) rawScoped(ctx context.Context, orgID uuid.UUID, fn func(pgx.Tx) error) error {
	return s.rawTx(ctx, orgID, fn)
}

// rawUnscoped runs fn as the admin role, for the operations that legitimately
// cross organizations.
func (s *Store) rawUnscoped(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.rawTx(ctx, uuid.Nil, fn)
}

func (s *Store) rawTx(ctx context.Context, orgID uuid.UUID, fn func(pgx.Tx) error) error {
	if s.tx != nil {
		return fn(s.tx)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Internal(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if orgID != uuid.Nil {
		if err := setOrgScope(ctx, tx, orgID); err != nil {
			return err
		}
	} else if err := setAdminRole(ctx, tx); err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Internal(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}
