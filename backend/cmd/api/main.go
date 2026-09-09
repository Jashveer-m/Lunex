// Command api runs the LifeOS HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jashveer/lifeos/backend/internal/api"
	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/config"
	"github.com/jashveer/lifeos/backend/internal/db"
	"github.com/jashveer/lifeos/backend/internal/users"
	"github.com/jashveer/lifeos/backend/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Opt-in so a deploy can choose to migrate as a separate step
	// (cmd/migrate) instead of on boot.
	if os.Getenv("AUTO_MIGRATE") == "true" {
		if err := db.Up(pool, migrations.FS); err != nil {
			return err
		}
		logger.Info("migrations applied")
	}
	version, dirty, err := db.Version(pool, migrations.FS)
	if err != nil {
		return err
	}
	if dirty {
		return errors.New("database schema is dirty; resolve it with cmd/migrate before starting")
	}
	logger.Info("schema ready", "version", version)

	userRepo := users.NewRepository(pool)
	sessionRepo := auth.NewSessionRepository(pool)
	tokens := auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTIssuer, cfg.AccessTokenTTL)
	service := auth.NewService(userRepo, sessionRepo, tokens, cfg.RefreshTokenTTL)

	handler := api.NewRouter(api.Deps{
		Auth:        auth.NewHandler(service, logger),
		Tokens:      tokens,
		RateLimiter: auth.NewIPRateLimiter(cfg.LoginRateLimit, cfg.LoginRateLimitBurst),
		DB:          pool,
		Logger:      logger,
	})

	go sweepExpiredSessions(ctx, sessionRepo, logger)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// sweepExpiredSessions keeps the sessions table from accumulating dead rows.
func sweepExpiredSessions(ctx context.Context, repo *auth.SessionRepository, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := repo.DeleteExpired(ctx)
			if err != nil {
				logger.Error("sweep expired sessions", "error", err)
				continue
			}
			if n > 0 {
				logger.Info("swept expired sessions", "count", n)
			}
		}
	}
}
