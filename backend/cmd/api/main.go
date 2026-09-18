// Command api runs the Lunex HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/api"
	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/config"
	"github.com/jashveer/lifeos/backend/internal/db"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
	"github.com/jashveer/lifeos/backend/internal/users"
	"github.com/jashveer/lifeos/backend/migrations"
)

func main() {
	// LOG_LEVEL is read here rather than in config.Load because the logger has
	// to exist before configuration can fail and be reported. debug is what
	// shows why an extraction dropped a memory or an edge.
	level := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			slog.Error("fatal", "error", "LOG_LEVEL: want debug, info, warn or error, got "+strconv.Quote(v))
			os.Exit(1)
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
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

	// The embedding client is built, not dialled: Ollama being down is a
	// per-request failure that the document ends up recording, not a reason to
	// refuse to serve tasks and notes.
	embedder := embeddings.NewOllama(cfg.OllamaBaseURL, cfg.EmbeddingModel,
		cfg.EmbeddingDimensions, cfg.DocumentProcessTimeout)
	logger.Info("embeddings configured",
		"base_url", cfg.OllamaBaseURL, "model", embedder.Model(), "dimensions", embedder.Dimensions())

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

	// Phase 6's knowledge graph is built before the resources that write into
	// it, because each of them takes it as a construction option: node sync is
	// part of the write path rather than something bolted on after it. That
	// ordering is the only reason the provider is created above rather than
	// below -- the graph shares it, so a relationship is read out of a turn by
	// the same kind of model that answered it.
	graphSvc := graph.NewService(graph.Deps{
		Store:    graph.NewRepository(pool),
		Provider: provider,
		Logger:   logger,
		Options: graph.Options{
			Model:       cfg.GraphModel,
			Temperature: cfg.GraphTemperature,
			MaxTokens:   cfg.GraphMaxTokens,
			Timeout:     cfg.GraphExtractTimeout,
			JSONMode:    cfg.GraphJSONMode,
		},
	})

	taskSvc := tasks.NewService(tasks.NewRepository(pool), tasks.WithNodeSync(graphSvc))
	goalSvc := goals.NewService(goals.NewRepository(pool), goals.WithNodeSync(graphSvc))
	noteSvc := notes.NewService(notes.NewRepository(pool), notes.WithNodeSync(graphSvc))
	docSvc := documents.NewService(documents.NewRepository(pool), embedder, logger,
		cfg.DocumentProcessTimeout, documents.WithNodeSync(graphSvc))
	calendarSvc := calendar.NewService(calendar.NewRepository(pool), calendar.WithNodeSync(graphSvc))
	financeSvc := finance.NewService(finance.NewRepository(pool), finance.WithNodeSync(graphSvc))

	// The memory system. It shares the chat provider and the embedder: a fact
	// is extracted by the same kind of model that answered, and embedded by the
	// same one that embedded the documents -- which it has to be, since both
	// kinds of vector live in `vector(768)` columns and are compared with the
	// same operator.
	memorySvc := memories.NewService(memories.Deps{
		Store:    memories.NewRepository(pool),
		Provider: provider,
		Embedder: embedder,
		Logger:   logger,
		Options: memories.Options{
			Model:       cfg.MemoryModel,
			Temperature: cfg.MemoryTemperature,
			MaxTokens:   cfg.MemoryMaxTokens,
			Timeout:     cfg.MemoryExtractTimeout,
			JSONMode:    cfg.MemoryJSONMode,
		},
	})
	// Extraction is the half with a switch: it costs a second model call on
	// every substantial turn. Switching it off leaves retrieval running, so the
	// assistant still uses what it already knows.
	var extractor chat.MemoryExtractor
	if cfg.MemoryExtraction {
		extractor = memorySvc
	}
	logger.Info("memory configured",
		"extraction", cfg.MemoryExtraction, "model", orElse(cfg.MemoryModel, provider.Model()),
		"min_similarity", cfg.MemoryMinSimilarity, "extract_timeout", cfg.MemoryExtractTimeout)

	// Phase 6 has the same switch on the same half, and for the same reason: a
	// third model call per substantial turn. Switching it off leaves node sync
	// and the 1-hop lookup running, so the assistant still uses the graph it
	// has -- including the nodes every task and note keep writing into it.
	var linker chat.GraphExtractor
	if cfg.GraphExtraction {
		linker = graphSvc
	}
	logger.Info("knowledge graph configured",
		"extraction", cfg.GraphExtraction, "model", orElse(cfg.GraphModel, provider.Model()),
		"extract_timeout", cfg.GraphExtractTimeout)

	// Phase 7's tools and the action engine. The registry is built over the
	// same services the HTTP API uses -- a tool that creates a task goes
	// through tasks.Service.Create, validation, ownership and graph sync
	// included -- and its ledger is the actions table, which is what makes
	// "no write without an approved action" a property of the wiring rather
	// than of the callers: the registry can run a write tool only after that
	// repository has approved a proposed row. There is one production path to
	// a write tool, and it starts at POST /actions/{id}/approve.
	actionRepo := actions.NewRepository(pool)
	registry, err := tools.NewRegistry(actionRepo, tools.Standard(tools.Services{
		Tasks: taskSvc, Goals: goalSvc, Notes: noteSvc, Documents: docSvc,
		Calendar: calendarSvc, Finance: financeSvc,
		DocumentMinSimilarity: cfg.ChatMinSimilarity,
	})...)
	if err != nil {
		return err
	}
	actionSvc := actions.NewService(actionRepo, registry, logger)
	// The switch removes the tools from the chat turn and nothing else: the
	// approval endpoints stay, so a proposal made before it was flipped can
	// still be decided.
	var (
		router     chat.ToolRouter
		toolRunner chat.ToolRunner
		actionLog  chat.ActionLog
	)
	if cfg.AgentTools {
		router = agents.NewRouter(provider, registry, logger, agents.Options{
			Model: cfg.AgentModel, Temperature: cfg.AgentTemperature,
			MaxTokens: cfg.AgentMaxTokens, Timeout: cfg.AgentTimeout,
		})
		toolRunner, actionLog = registry, actionSvc
	}
	logger.Info("agent tools configured",
		"enabled", cfg.AgentTools, "agent", agents.General.Name, "tools", len(registry.Tools()),
		"model", orElse(cfg.AgentModel, provider.Model()), "timeout", cfg.AgentTimeout)

	chatSvc := chat.NewService(chat.Deps{
		Store:    chat.NewRepository(pool),
		Provider: provider,
		// Retrieval reads through the Phase 2/3 services, not their
		// repositories: the assistant sees exactly what the API would return,
		// validation and ownership included.
		Documents: docSvc,
		// Both halves of Phase 5, wired separately so either can be absent.
		Memories:        memorySvc,
		MemoryExtractor: extractor,
		// Both halves of Phase 6, on the same terms.
		Graph:          graphSvc,
		GraphExtractor: linker,
		Tasks:          taskSvc,
		Goals:          goalSvc,
		Notes:          noteSvc,
		// Phase 8: what is on in the next day or two, as background.
		Calendar: calendarSvc,
		// Phase 7, all three or none.
		Router:  router,
		Tools:   toolRunner,
		Actions: actionLog,
		Logger:  logger,
		Options: chat.Options{
			Temperature:         cfg.ChatTemperature,
			MaxTokens:           cfg.ChatMaxTokens,
			MinSimilarity:       cfg.ChatMinSimilarity,
			MemoryMinSimilarity: cfg.MemoryMinSimilarity,
		},
	})

	// An approved change is what the relationship extractor could not see when
	// the turn that proposed it ran; the chat service reads that turn again,
	// anchored to the record the approval produced. A no-op when graph
	// extraction is switched off.
	actionSvc.OnExecuted(chatSvc.ActionExecuted)

	handler := api.NewRouter(api.Deps{
		Auth:        auth.NewHandler(service, logger),
		Actions:     actions.NewHandler(actionSvc, logger),
		Tasks:       tasks.NewHandler(taskSvc, logger),
		Goals:       goals.NewHandler(goalSvc, logger),
		Notes:       notes.NewHandler(noteSvc, logger),
		Calendar:    calendar.NewHandler(calendarSvc, logger),
		Finance:     finance.NewHandler(financeSvc, logger),
		Documents:   documents.NewHandler(docSvc, logger, cfg.MaxUploadBytes),
		Chat:        chat.NewHandler(chatSvc, logger),
		Memories:    memories.NewHandler(memorySvc, logger),
		Graph:       graph.NewHandler(graphSvc, logger),
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

// orElse is the fallback for a logged value that is only set when it overrides
// something else.
func orElse(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
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
