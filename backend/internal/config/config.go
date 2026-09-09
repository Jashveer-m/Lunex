// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
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

	// Phase 3: documents and retrieval.
	OllamaBaseURL       string
	EmbeddingModel      string
	EmbeddingDimensions int
	MaxUploadBytes      int64
	// DocumentProcessTimeout bounds one synchronous upload: extract, chunk,
	// embed and store. It also sets the server's write timeout, because a
	// response the server has already given up on cannot report the result.
	DocumentProcessTimeout time.Duration
}

// WriteTimeout is how long the HTTP server will spend producing a response.
//
// Uploads process synchronously in this phase, so the server has to outlive
// the pipeline or a slow document would be cut off at the socket with the
// document row left mid-flight. The slack covers writing the response itself.
// When the job queue arrives this drops back to a flat 30s.
func (c Config) WriteTimeout() time.Duration {
	const base = 30 * time.Second
	if d := c.DocumentProcessTimeout + 15*time.Second; d > base {
		return d
	}
	return base
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

		OllamaBaseURL:       envOr("OLLAMA_BASE_URL", embeddings.DefaultBaseURL),
		EmbeddingModel:      envOr("EMBEDDING_MODEL", embeddings.DefaultModel),
		EmbeddingDimensions: embeddings.DefaultDimensions,
		MaxUploadBytes:      documents.MaxUploadBytes,
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
	if cfg.DocumentProcessTimeout, err = durationOr("DOCUMENT_PROCESS_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	// The vector column is `vector(768)` in migration 000003. Changing the
	// model without a migration that changes the column -- and re-embeds every
	// existing chunk, since vectors from two models are not comparable -- would
	// fail on the first insert, so the knob exists but the mismatch is the
	// operator's to resolve.
	if v := os.Getenv("EMBEDDING_DIMENSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("EMBEDDING_DIMENSIONS: want a positive integer, got %q", v)
		}
		cfg.EmbeddingDimensions = n
	}
	if v := os.Getenv("MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("MAX_UPLOAD_BYTES: want a positive integer, got %q", v)
		}
		cfg.MaxUploadBytes = n
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
