package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type fakeRepository struct {
	user       User
	actor      Actor
	session    Session
	findErr    error
	sessionErr error
}

func (r *fakeRepository) FindUserByUsername(context.Context, string) (User, error) {
	if r.findErr != nil {
		return User{}, r.findErr
	}
	return r.user, nil
}

func (r *fakeRepository) CreateSession(_ context.Context, session Session) error {
	r.session = session
	return r.sessionErr
}

func (r *fakeRepository) FindActorByTokenHash(context.Context, [32]byte, time.Time) (Actor, error) {
	if r.findErr != nil {
		return Actor{}, r.findErr
	}
	return r.actor, nil
}

func TestLoginCreatesHashedSession(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("demo-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepository{user: User{ID: 7, Username: "alice", PasswordHash: string(hash), Role: RoleCustomer}}
	service := NewService(repo, time.Hour)
	fixedNow := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	token, expiresAt, err := service.Login(context.Background(), "alice", "demo-password")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if token == "" {
		t.Fatal("Login() returned empty token")
	}
	if repo.session.TokenHash == [32]byte{} {
		t.Fatal("stored token hash is empty")
	}
	if !expiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("expiresAt = %v", expiresAt)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.MinCost)
	repo := &fakeRepository{user: User{ID: 1, Username: "alice", PasswordHash: string(hash)}}
	service := NewService(repo, time.Hour)

	_, _, err := service.Login(context.Background(), "alice", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthenticateHidesMissingSession(t *testing.T) {
	service := NewService(&fakeRepository{findErr: ErrNotFound}, time.Hour)
	_, err := service.Authenticate(context.Background(), "missing")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}
