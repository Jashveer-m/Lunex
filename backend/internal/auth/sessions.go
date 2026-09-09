package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrSessionNotFound means no live session matched the presented refresh token.
var ErrSessionNotFound = errors.New("session not found")

// Session mirrors a row of the sessions table.
type Session struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	RefreshTokenHash string
	ExpiresAt        time.Time
	DeviceInfo       *string
	CreatedAt        time.Time
}

// SessionRepository is the Postgres store for refresh sessions.
type SessionRepository struct {
	db *sql.DB
}

func NewSessionRepository(db *sql.DB) *SessionRepository { return &SessionRepository{db: db} }

func (r *SessionRepository) Create(ctx context.Context, userID uuid.UUID, hash string, expiresAt time.Time, deviceInfo *string) (Session, error) {
	var s Session
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO sessions (user_id, refresh_token_hash, expires_at, device_info)
		VALUES ($1, $2, $3, $4)
		RETURNING id, user_id, refresh_token_hash, expires_at, device_info, created_at`,
		userID, hash, expiresAt, deviceInfo,
	).Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.ExpiresAt, &s.DeviceInfo, &s.CreatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	return s, nil
}

// Rotate atomically swaps a live session's refresh token hash for a new one.
// Doing it in a single UPDATE ... RETURNING means two concurrent uses of the
// same token cannot both succeed: the loser matches zero rows.
func (r *SessionRepository) Rotate(ctx context.Context, oldHash, newHash string, expiresAt time.Time) (Session, error) {
	var s Session
	err := r.db.QueryRowContext(ctx, `
		UPDATE sessions
		SET refresh_token_hash = $1, expires_at = $2
		WHERE refresh_token_hash = $3 AND expires_at > now()
		RETURNING id, user_id, refresh_token_hash, expires_at, device_info, created_at`,
		newHash, expiresAt, oldHash,
	).Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.ExpiresAt, &s.DeviceInfo, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("rotate session: %w", err)
	}
	return s, nil
}

// DeleteByTokenHash invalidates a single session (logout).
func (r *SessionRepository) DeleteByTokenHash(ctx context.Context, hash string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE refresh_token_hash = $1`, hash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// DeleteExpired sweeps rows whose refresh token can no longer be used.
func (r *SessionRepository) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}
