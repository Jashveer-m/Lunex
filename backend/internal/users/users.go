// Package users holds the user aggregate and its persistence layer.
package users

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound is returned when no row matches the lookup.
	ErrNotFound = errors.New("user not found")
	// ErrEmailTaken is returned when the unique email index rejects an insert.
	ErrEmailTaken = errors.New("email already registered")
)

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Profile struct {
	UserID    uuid.UUID
	Name      string
	Timezone  string
	CreatedAt time.Time
	UpdatedAt time.Time
}
