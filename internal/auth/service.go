package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

type Service struct {
	repo Repository
	now  func() time.Time
	ttl  time.Duration
}

func NewService(repo Repository, ttl time.Duration) *Service {
	return &Service{repo: repo, now: time.Now, ttl: ttl}
}

func (s *Service) Login(ctx context.Context, username, password string) (string, time.Time, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", time.Time{}, ErrInvalidCredentials
	}
	user, err := s.repo.FindUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", time.Time{}, ErrInvalidCredentials
		}
		return "", time.Time{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	expiresAt := s.now().Add(s.ttl)
	if err := s.repo.CreateSession(ctx, Session{TokenHash: hash, UserID: user.ID, ExpiresAt: expiresAt}); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Actor, error) {
	if token == "" {
		return Actor{}, ErrInvalidCredentials
	}
	hash := sha256.Sum256([]byte(token))
	actor, err := s.repo.FindActorByTokenHash(ctx, hash, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Actor{}, ErrInvalidCredentials
		}
		return Actor{}, err
	}
	return actor, nil
}
