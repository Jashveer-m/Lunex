package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Repository is the Postgres-backed store for users and their profiles.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// Create inserts a user and its profile in one transaction: a user without a
// profile row is not a state the rest of the system should ever have to handle.
func (r *Repository) Create(ctx context.Context, email, passwordHash, name, timezone string) (User, Profile, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, Profile{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the tx is committed

	var u User
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING id, email, password_hash, created_at, updated_at`,
		strings.ToLower(email), passwordHash,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, Profile{}, ErrEmailTaken
		}
		return User{}, Profile{}, fmt.Errorf("insert user: %w", err)
	}

	var p Profile
	err = tx.QueryRowContext(ctx, `
		INSERT INTO user_profiles (user_id, name, timezone)
		VALUES ($1, $2, $3)
		RETURNING user_id, name, timezone, created_at, updated_at`,
		u.ID, name, timezone,
	).Scan(&p.UserID, &p.Name, &p.Timezone, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return User{}, Profile{}, fmt.Errorf("insert profile: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return User{}, Profile{}, fmt.Errorf("commit: %w", err)
	}
	return u, p, nil
}

func (r *Repository) ByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := r.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, created_at, updated_at
		FROM users WHERE lower(email) = lower($1)`, email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select user by email: %w", err)
	}
	return u, nil
}

func (r *Repository) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := r.db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, created_at, updated_at
		FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("select user by id: %w", err)
	}
	return u, nil
}

func (r *Repository) ProfileByUserID(ctx context.Context, id uuid.UUID) (Profile, error) {
	var p Profile
	err := r.db.QueryRowContext(ctx, `
		SELECT user_id, name, timezone, created_at, updated_at
		FROM user_profiles WHERE user_id = $1`, id,
	).Scan(&p.UserID, &p.Name, &p.Timezone, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("select profile: %w", err)
	}
	return p, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
