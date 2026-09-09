// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port            string
	DatabaseURL     string
	JWTSecret       []byte
	JWTIssuer       string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// LoginRateLimit is the per-IP request budget for the auth endpoints.
	LoginRateLimit      float64
	LoginRateLimitBurst int
}

// Load reads configuration from the environment, applying defaults for
// everything except the values that must never have one.
func Load() (Config, error) {
	cfg := Config{
		Port:                envOr("PORT", "8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		JWTSecret:           []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:           envOr("JWT_ISSUER", "lifeos"),
		LoginRateLimitBurst: 10,
		LoginRateLimit:      0.2, // 1 request per 5s sustained, per IP
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	// A short or missing signing key silently weakens every access token, so
	// refuse to boot rather than degrade.
	if len(cfg.JWTSecret) < 32 {
		return Config{}, errors.New("JWT_SECRET is required and must be at least 32 bytes")
	}

	var err error
	if cfg.AccessTokenTTL, err = durationOr("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RefreshTokenTTL, err = durationOr("REFRESH_TOKEN_TTL", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("LOGIN_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("LOGIN_RATE_LIMIT_BURST: want a positive integer, got %q", v)
		}
		cfg.LoginRateLimitBurst = n
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
