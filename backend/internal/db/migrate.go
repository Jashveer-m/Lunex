package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// newMigrator builds a golang-migrate instance over an embedded migrations FS,
// so the binary can migrate itself without the migrate CLI being installed.
func newMigrator(pool *sql.DB, files fs.FS) (*migrate.Migrate, error) {
	src, err := iofs.New(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	driver, err := migratepg.WithInstance(pool, &migratepg.Config{})
	if err != nil {
		return nil, fmt.Errorf("migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return nil, fmt.Errorf("migrator: %w", err)
	}
	return m, nil
}

// Up applies all pending migrations. It is safe to call on every boot.
func Up(pool *sql.DB, files fs.FS) error {
	m, err := newMigrator(pool, files)
	if err != nil {
		return err
	}
	// Close() would also close the *sql.DB the caller still owns, so only the
	// source half is released here.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls every migration back. Intended for local development and tests.
func Down(pool *sql.DB, files fs.FS) error {
	m, err := newMigrator(pool, files)
	if err != nil {
		return err
	}
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Version reports the current schema version and whether it is dirty.
func Version(pool *sql.DB, files fs.FS) (uint, bool, error) {
	m, err := newMigrator(pool, files)
	if err != nil {
		return 0, false, err
	}
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return v, dirty, nil
}
