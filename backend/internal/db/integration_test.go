package db_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/db"
	"github.com/jashveer/lifeos/backend/internal/users"
	"github.com/jashveer/lifeos/backend/migrations"
)

// These tests exercise the real SQL. They are skipped unless
// TEST_DATABASE_URL points at a throwaway Postgres database:
//
//	createdb lunex_test
//	TEST_DATABASE_URL=postgres://localhost:5432/lunex_test?sslmode=disable go test ./...
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database integration tests")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if err := db.Up(pool, migrations.FS); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// Every test starts from an empty users table; sessions and profiles go
	// with it through ON DELETE CASCADE.
	if _, err := pool.Exec(`TRUNCATE users CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func TestMigrationsUpDownUp(t *testing.T) {
	pool := testDB(t)

	if err := db.Down(pool, migrations.FS); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(`SELECT to_regclass('public.users') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("users table survived a full rollback")
	}

	if err := db.Up(pool, migrations.FS); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	version, dirty, err := db.Version(pool, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	// Bump this with every migration added; a stale value here is how a
	// migration that never runs in CI goes unnoticed.
	const wantVersion = 8
	if version != wantVersion || dirty {
		t.Fatalf("version = %d dirty = %t, want %d / false", version, dirty, wantVersion)
	}
}

func TestSchemaConstraints(t *testing.T) {
	pool := testDB(t)
	repo := users.NewRepository(pool)
	ctx := context.Background()

	user, profile, err := repo.Create(ctx, "ada@example.com", "hash", "Ada", "Europe/London")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if profile.UserID != user.ID {
		t.Fatal("profile is not linked to the user")
	}

	t.Run("email is unique case-insensitively", func(t *testing.T) {
		if _, _, err := repo.Create(ctx, "ADA@example.com", "hash", "", "UTC"); !errors.Is(err, users.ErrEmailTaken) {
			t.Fatalf("err = %v, want ErrEmailTaken", err)
		}
	})

	t.Run("sessions require a real user", func(t *testing.T) {
		_, err := auth.NewSessionRepository(pool).Create(ctx, uuid.New(), "orphan-hash", time.Now().Add(time.Hour), nil)
		if err == nil {
			t.Fatal("a session was created for a non-existent user")
		}
	})

	t.Run("deleting a user cascades", func(t *testing.T) {
		sessions := auth.NewSessionRepository(pool)
		if _, err := sessions.Create(ctx, user.ID, "hash-to-cascade", time.Now().Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(`DELETE FROM users WHERE id = $1`, user.ID); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = $1`, user.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d sessions survived the user deletion", n)
		}
		if err := pool.QueryRow(`SELECT count(*) FROM user_profiles WHERE user_id = $1`, user.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("the profile survived the user deletion")
		}
	})
}

func TestRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	repo := users.NewRepository(pool)
	ctx := context.Background()

	created, _, err := repo.Create(ctx, "Ada@Example.com", "hash", "Ada", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if created.Email != "ada@example.com" {
		t.Fatalf("stored email = %q, want it lowercased", created.Email)
	}

	byEmail, err := repo.ByEmail(ctx, "ADA@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("ByEmail: %v", err)
	}
	if byEmail.ID != created.ID {
		t.Fatal("ByEmail returned a different user")
	}

	if _, err := repo.ByEmail(ctx, "nobody@example.com"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := repo.ByID(ctx, uuid.New()); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSessionRotationIsAtomic(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	user, _, err := users.NewRepository(pool).Create(ctx, "ada@example.com", "hash", "Ada", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessionRepository(pool)

	original, err := sessions.Create(ctx, user.ID, "hash-v1", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := sessions.Rotate(ctx, "hash-v1", "hash-v2", time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.ID != original.ID {
		t.Fatal("rotation created a new session row")
	}

	// The old hash is gone, so a replay finds nothing.
	if _, err := sessions.Rotate(ctx, "hash-v1", "hash-v3", time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("replay err = %v, want ErrSessionNotFound", err)
	}

	t.Run("expired sessions cannot rotate", func(t *testing.T) {
		if _, err := pool.Exec(`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, original.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.Rotate(ctx, "hash-v2", "hash-v4", time.Now().Add(time.Hour)); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("expired sessions are swept", func(t *testing.T) {
		n, err := sessions.DeleteExpired(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("swept %d rows, want 1", n)
		}
	})
}

func TestUpdatedAtTriggerFires(t *testing.T) {
	pool := testDB(t)
	user, _, err := users.NewRepository(pool).Create(context.Background(), "ada@example.com", "hash", "Ada", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	var updatedAt time.Time
	if err := pool.QueryRow(`
		UPDATE users SET password_hash = 'new-hash' WHERE id = $1
		RETURNING updated_at`, user.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if !updatedAt.After(user.UpdatedAt) {
		t.Fatalf("updated_at = %v, want it moved past %v", updatedAt, user.UpdatedAt)
	}
}
