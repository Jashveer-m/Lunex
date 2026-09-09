package config

import (
	"strings"
	"testing"
	"time"
)

const validSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8080" {
		t.Fatalf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.AccessTokenTTL != 15*time.Minute {
		t.Fatalf("AccessTokenTTL = %v, want 15m", cfg.AccessTokenTTL)
	}
	if cfg.RefreshTokenTTL != 30*24*time.Hour {
		t.Fatalf("RefreshTokenTTL = %v, want 720h", cfg.RefreshTokenTTL)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", validSecret)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v, want a DATABASE_URL error", err)
	}
}

// A weak signing key must stop the process, not degrade quietly.
func TestLoadRejectsShortJWTSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	for _, secret := range []string{"", "too-short"} {
		t.Setenv("JWT_SECRET", secret)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
			t.Fatalf("secret %q: err = %v, want a JWT_SECRET error", secret, err)
		}
	}
}

func TestLoadParsesDurations(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("ACCESS_TOKEN_TTL", "5m")
	t.Setenv("REFRESH_TOKEN_TTL", "168h")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessTokenTTL != 5*time.Minute || cfg.RefreshTokenTTL != 168*time.Hour {
		t.Fatalf("TTLs = %v / %v", cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	}

	t.Setenv("ACCESS_TOKEN_TTL", "fifteen minutes")
	if _, err := Load(); err == nil {
		t.Fatal("an unparseable duration was accepted")
	}
}
