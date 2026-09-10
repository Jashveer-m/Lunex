// Package api wires the HTTP routes for the Lunex API.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// Deps are everything the router needs to build the route tree.
type Deps struct {
	Auth        *auth.Handler
	Actions     *actions.Handler
	Tasks       *tasks.Handler
	Goals       *goals.Handler
	Notes       *notes.Handler
	Documents   *documents.Handler
	Chat        *chat.Handler
	Memories    *memories.Handler
	Graph       *graph.Handler
	Tokens      *auth.TokenIssuer
	RateLimiter *auth.IPRateLimiter
	DB          *sql.DB
	Logger      *slog.Logger
	// DocumentTimeout overrides the global request timeout for /documents.
	// Zero falls back to the global one.
	DocumentTimeout time.Duration
	// ChatTimeout does the same for /conversations, where a turn waits on a
	// model rather than on an embedder. Zero falls back to the global one.
	ChatTimeout time.Duration
}

// NewRouter returns the fully wired handler for the API.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// middleware.RealIP is deliberately NOT used: it rewrites RemoteAddr from
	// X-Forwarded-For, which is client-controlled until a trusted proxy is in
	// front of this process, and the rate limiter keys on RemoteAddr.
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	// The request budget is applied per subtree rather than once at the root.
	//
	// middleware.Timeout derives its context from the one already on the
	// request, so a nested Timeout can only ever shorten the deadline it
	// inherits: a two-minute budget underneath a thirty-second one is thirty
	// seconds. A root-level 30s would silently cap uploads and chat turns at
	// 30s however large their own budgets were set -- so each subtree names
	// its own, and none of them nest.
	r.With(middleware.Timeout(requestTimeout)).Get("/healthz", healthz(d.DB))

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Use(middleware.Timeout(requestTimeout))
			// Rate limiting guards the endpoints that are worth guessing at.
			r.Group(func(r chi.Router) {
				r.Use(d.RateLimiter.Middleware)
				r.Post("/register", d.Auth.Register)
				r.Post("/login", d.Auth.Login)
			})
			r.Post("/refresh", d.Auth.Refresh)
			r.Post("/logout", d.Auth.Logout)
		})

		// RequireAuth is repeated per group rather than lifted onto the
		// /api/v1 router itself, because a router-level middleware runs
		// before routing: an unknown path under /api/v1 would then answer
		// 401 instead of 404, which tells an unauthenticated caller nothing
		// but is still the wrong answer.

		// Everything that answers from the database alone.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(d.Tokens))
			r.Use(middleware.Timeout(requestTimeout))
			r.Get("/me", d.Auth.Me)

			// Phase 2 resources. Each module owns its own subtree, so adding
			// a route never means editing a shared switch statement, and the
			// RequireAuth wrapper is applied once for all of them rather than
			// remembered per route.
			r.Mount("/tasks", d.Tasks.Routes())
			r.Mount("/goals", d.Goals.Routes())
			r.Mount("/notes", d.Notes.Routes())

			// Phase 5. Managing memories is database work: listing, editing
			// and deleting rows. The model and the embedder are only involved
			// in the chat turn that creates one -- and in the re-embed a PATCH
			// to `content` triggers, which is a single vector and comfortably
			// inside the ordinary budget.
			r.Mount("/memories", d.Memories.Routes())

			// Phase 6. Reading and pruning the knowledge graph is database
			// work: two indexed selects and a delete. The model is only
			// involved in the chat turn that grows an edge, and the sync that
			// creates a node is a single upsert on the resource's own request.
			r.Mount("/knowledge-graph", d.Graph.Routes())

			// Phase 7. Approving an action runs its tool, and every tool is a
			// single insert or update plus a graph sync -- database work with
			// no model anywhere, so the ordinary budget. The model's part was
			// the chat turn that proposed it.
			r.Mount("/actions", d.Actions.Routes())
		})

		// Phase 3. The upload pipeline runs inside the request -- extract,
		// chunk, embed, store -- so this subtree gets a longer budget.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(d.Tokens))
			r.Use(middleware.Timeout(orDefault(d.DocumentTimeout)))
			r.Mount("/documents", d.Documents.Routes())
		})

		// Phase 4. A chat turn retrieves, generates and streams, all inside
		// the request, and a local model outlives 30 seconds routinely.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(d.Tokens))
			r.Use(middleware.Timeout(orDefault(d.ChatTimeout)))
			r.Mount("/conversations", d.Chat.Routes())
		})
	})

	return r
}

// requestTimeout is the budget for a request that only touches the database.
const requestTimeout = 30 * time.Second

// orDefault falls back to the ordinary budget for a subtree whose own is unset.
func orDefault(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return requestTimeout
}

// healthz reports process liveness plus database reachability.
func healthz(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body := http.StatusOK, map[string]string{"status": "ok", "database": "ok"}
		if db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := db.PingContext(ctx); err != nil {
				status = http.StatusServiceUnavailable
				body = map[string]string{"status": "degraded", "database": "unreachable"}
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// securityHeaders sets the handful that matter for a JSON API.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Auth responses carry tokens; keep them out of shared caches.
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
