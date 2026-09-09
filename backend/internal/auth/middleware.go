package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type contextKey struct{ name string }

var userIDContextKey = contextKey{"userID"}

// UserIDFromContext returns the authenticated user id placed by RequireAuth.
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userIDContextKey).(uuid.UUID)
	return id, ok
}

// ContextWithUserID is exported for tests and for future middleware.
func ContextWithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDContextKey, id)
}

// RequireAuth rejects requests without a valid Bearer access token and puts the
// user id on the request context for downstream handlers.
func RequireAuth(tokens *TokenIssuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			scheme, token, found := strings.Cut(header, " ")
			if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lifeos"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "A Bearer access token is required.")
				return
			}

			userID, err := tokens.Parse(strings.TrimSpace(token))
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lifeos", error="invalid_token"`)
				writeError(w, http.StatusUnauthorized, "invalid_token", "The access token is invalid or has expired.")
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithUserID(r.Context(), userID)))
		})
	}
}
