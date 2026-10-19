package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("auth record not found")

type Repository interface {
	FindUserByUsername(context.Context, string) (User, error)
	CreateSession(context.Context, Session) error
	FindActorByTokenHash(context.Context, [32]byte, time.Time) (Actor, error)
}

type MySQLRepository struct {
	db *sql.DB
}

func NewMySQLRepository(db *sql.DB) *MySQLRepository {
	return &MySQLRepository{db: db}
}

func (r *MySQLRepository) FindUserByUsername(ctx context.Context, username string) (User, error) {
	var user User
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, role FROM users WHERE username = ?`, username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user: %w", err)
	}
	return user, nil
}

func (r *MySQLRepository) CreateSession(ctx context.Context, session Session) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO auth_sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		session.TokenHash[:], session.UserID, session.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *MySQLRepository) FindActorByTokenHash(ctx context.Context, tokenHash [32]byte, now time.Time) (Actor, error) {
	var actor Actor
	err := r.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.role
		FROM auth_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash[:], now,
	).Scan(&actor.UserID, &actor.Username, &actor.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{}, ErrNotFound
	}
	if err != nil {
		return Actor{}, fmt.Errorf("find session: %w", err)
	}
	return actor, nil
}
