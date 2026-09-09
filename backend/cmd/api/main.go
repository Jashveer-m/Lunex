// Command api runs the Lunex HTTP API.
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

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/api"
	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/config"
	"github.com/jashveer/lifeos/backend/internal/db"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
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

	taskSvc := tasks.NewService(tasks.NewRepository(pool))
	goalSvc := goals.NewService(goals.NewRepository(pool))
	noteSvc := notes.NewService(notes.NewRepository(pool))

	// The embedding client is built, not dialled: Ollama being down is a
	// per-request failure that the document ends up recording, not a reason to
	// refuse to serve tasks and notes.
	embedder := embeddings.NewOllama(cfg.OllamaBaseURL, cfg.EmbeddingModel,
		cfg.EmbeddingDimensions, cfg.DocumentProcessTimeout)
	logger.Info("embeddings configured",
		"base_url", cfg.OllamaBaseURL, "model", embedder.Model(), "dimensions", embedder.Dimensions())
	docSvc := documents.NewService(documents.NewRepository(pool), embedder, logger, cfg.DocumentProcessTimeout)

	// Same story for the chat model: built, not dialled. Ollama being down
	// makes a chat turn a 503; it does not stop the API serving tasks.
	//
	// This is the one place a provider is chosen. Swapping in a hosted model
	// later is a change to this expression and nothing else -- internal/chat
	// depends on ai.Provider, not on Ollama.
	provider := ai.NewOllama(cfg.OllamaBaseURL, cfg.ChatModel, cfg.ChatTimeout)
	logger.Info("chat provider configured",
		"base_url", cfg.OllamaBaseURL, "model", provider.Model(),
		"min_similarity", cfg.ChatMinSimilarity)
	chatSvc := chat.NewService(chat.Deps{
		Store:    chat.NewRepository(pool),
		Provider: provider,
		// Retrieval reads through the Phase 2/3 services, not their
		// repositories: the assistant sees exactly what the API would return,
		// validation and ownership included.
		Documents: docSvc,
		Tasks:     taskSvc,
		Goals:     goalSvc,
		Notes:     noteSvc,
		Logger:    logger,
		Options: chat.Options{
			Temperature:   cfg.ChatTemperature,
			MaxTokens:     cfg.ChatMaxTokens,
			MinSimilarity: cfg.ChatMinSimilarity,
		},
	})

	handler := api.NewRouter(api.Deps{
		Auth:        auth.NewHandler(service, logger),
		Tasks:       tasks.NewHandler(taskSvc, logger),
		Goals:       goals.NewHandler(goalSvc, logger),
		Notes:       notes.NewHandler(noteSvc, logger),
		Documents:   documents.NewHandler(docSvc, logger, cfg.MaxUploadBytes),
		Chat:        chat.NewHandler(chatSvc, logger),
		Tokens:      tokens,
		RateLimiter: auth.NewIPRateLimiter(cfg.LoginRateLimit, cfg.LoginRateLimitBurst),
		DB:          pool,
		Logger:      logger,
		// A little under the server's write timeout, so a request that runs
		// long is ended by the handler -- which can still answer -- rather
		// than by the socket, which cannot.
		DocumentTimeout: cfg.DocumentProcessTimeout + 5*time.Second,
		ChatTimeout:     cfg.ChatTimeout + 5*time.Second,
	})

	go sweepExpiredSessions(ctx, sessionRepo, logger)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Reading a 10 MB upload over a slow link can outlast 30 seconds, and
		// ReadTimeout covers the body as well as the headers. ReadHeaderTimeout
		// above is what actually guards against a slowloris, so widening this
		// one costs nothing.
		ReadTimeout:  cfg.WriteTimeout(),
		WriteTimeout: cfg.WriteTimeout(),
		IdleTimeout:  120 * time.Second,
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
