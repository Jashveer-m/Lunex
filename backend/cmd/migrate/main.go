// Command migrate applies or rolls back database migrations.
//
// Usage:
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down      # rolls everything back
//	go run ./cmd/migrate version
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jashveer/lifeos/backend/internal/db"
	"github.com/jashveer/lifeos/backend/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: migrate <up|down|version>")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch os.Args[1] {
	case "up":
		if err := db.Up(pool, migrations.FS); err != nil {
			return err
		}
		fmt.Println("migrations applied")
	case "down":
		if err := db.Down(pool, migrations.FS); err != nil {
			return err
		}
		fmt.Println("migrations rolled back")
	case "version":
		v, dirty, err := db.Version(pool, migrations.FS)
		if err != nil {
			return err
		}
		fmt.Printf("version=%d dirty=%t\n", v, dirty)
	default:
		return fmt.Errorf("unknown command %q: want up, down or version", os.Args[1])
	}
	return nil
}
