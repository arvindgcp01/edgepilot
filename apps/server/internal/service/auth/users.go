package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
)

// CreateUser adds an operator account and returns its one-time password.
//
// There is no invitation email on this server, so the account is created
// usable: a random 18-byte password is generated here, hashed, and returned
// exactly once to the administrator who asked — the same contract as an
// enrollment token. It is never stored, never logged, and the new user is
// expected to change it, which ChangePassword lets them do.
func (s *Service) CreateUser(ctx context.Context, in domain.CreateUserInput) (*domain.User, string, error) {
	if err := in.Validate(); err != nil {
		return nil, "", err
	}

	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", domain.Internal(fmt.Errorf("auth: generate a password: %w", err))
	}
	temporary := base64.RawURLEncoding.EncodeToString(raw)

	hash, err := s.hasher.Hash(temporary)
	if err != nil {
		return nil, "", domain.Internal(fmt.Errorf("auth: hash the password: %w", err))
	}

	user, err := s.store.Users().Create(ctx, in, hash)
	if err != nil {
		return nil, "", err
	}
	return user, temporary, nil
}
