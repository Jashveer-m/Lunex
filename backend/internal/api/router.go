// Package api wires the HTTP routes for the LifeOS API.
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

	"github.com/jashveer/lifeos/backend/internal/auth"
)

// Deps are everything the router needs to build the route tree.
type Deps struct {
	Auth        *auth.Handler
	Tokens      *auth.TokenIssuer
	RateLimiter *auth.IPRateLimiter
	DB          *sql.DB
	Logger      *slog.Logger
}

// NewRouter returns the fully wired handler for the API.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// middleware.RealIP is deliberately NOT used: it rewrites RemoteAddr from
	// X-Forwarded-For, which is client-controlled until a trusted proxy is in
	// front of this process, and the rate limiter keys on RemoteAddr.
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(securityHeaders)

	r.Get("/healthz", healthz(d.DB))

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			// Rate limiting guards the endpoints that are worth guessing at.
			r.Group(func(r chi.Router) {
				r.Use(d.RateLimiter.Middleware)
				r.Post("/register", d.Auth.Register)
				r.Post("/login", d.Auth.Login)
			})
			r.Post("/refresh", d.Auth.Refresh)
			r.Post("/logout", d.Auth.Logout)
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth(d.Tokens))
			r.Get("/me", d.Auth.Me)
		})
	})

	return r
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
