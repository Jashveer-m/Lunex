package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestHandler(t *testing.T) (*Handler, *Service) {
	t.Helper()
	svc, _, _ := newTestService(t)
	return NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))), svc
}

func post(t *testing.T, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestRegisterHandlerReturns201WithTokens(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(t, h.Register, "/api/v1/auth/register", map[string]string{
		"email": testEmail, "password": testPassword, "name": "Ada",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	got := decodeBody[authResponse](t, rec)
	if got.User.Email != testEmail || got.User.ID == "" {
		t.Fatalf("user = %+v", got.User)
	}
	if got.Tokens.AccessToken == "" || got.Tokens.RefreshToken == "" {
		t.Fatal("response is missing tokens")
	}
	if got.Tokens.TokenType != "Bearer" {
		t.Fatalf("token_type = %q, want Bearer", got.Tokens.TokenType)
	}
	if got.Tokens.ExpiresIn < 14*60 || got.Tokens.ExpiresIn > 15*60 {
		t.Fatalf("expires_in = %d, want ~900", got.Tokens.ExpiresIn)
	}
	// The password must never appear anywhere in the response.
	if strings.Contains(rec.Body.String(), testPassword) || strings.Contains(rec.Body.String(), "password_hash") {
		t.Fatalf("response leaks password material: %s", rec.Body)
	}
}

func TestRegisterHandlerConflictOnDuplicate(t *testing.T) {
	h, _ := newTestHandler(t)
	body := map[string]string{"email": testEmail, "password": testPassword}
	if rec := post(t, h.Register, "/api/v1/auth/register", body); rec.Code != http.StatusCreated {
		t.Fatalf("first register status = %d", rec.Code)
	}
	rec := post(t, h.Register, "/api/v1/auth/register", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if got := decodeBody[ErrorBody](t, rec); got.Error != "email_taken" {
		t.Fatalf("error = %q, want email_taken", got.Error)
	}
}

func TestRegisterHandlerValidationErrors(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(t, h.Register, "/api/v1/auth/register", map[string]string{"email": "nope", "password": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	got := decodeBody[ErrorBody](t, rec)
	if got.Error != "validation_failed" || len(got.Fields) != 2 {
		t.Fatalf("body = %+v, want both fields reported", got)
	}
}

func TestHandlersRejectMalformedRequests(t *testing.T) {
	h, _ := newTestHandler(t)

	t.Run("invalid json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.Login(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		rec := post(t, h.Register, "/api/v1/auth/register", map[string]string{
			"email": testEmail, "password": testPassword, "is_admin": "true",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
		}
	})

	t.Run("wrong content type", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("email=a"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.Login(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", rec.Code)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(strings.Repeat("a", maxBodyBytes+1)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.Login(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want the body to be rejected", rec.Code)
		}
	})
}

func TestLoginHandler(t *testing.T) {
	h, _ := newTestHandler(t)
	post(t, h.Register, "/api/v1/auth/register", map[string]string{"email": testEmail, "password": testPassword})

	t.Run("success", func(t *testing.T) {
		rec := post(t, h.Login, "/api/v1/auth/login", map[string]string{"email": testEmail, "password": testPassword})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	t.Run("wrong password is 401", func(t *testing.T) {
		rec := post(t, h.Login, "/api/v1/auth/login", map[string]string{"email": testEmail, "password": "nope-nope-nope-1"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := decodeBody[ErrorBody](t, rec); got.Error != "invalid_credentials" {
			t.Fatalf("error = %q", got.Error)
		}
	})
}

func TestRefreshAndLogoutHandlers(t *testing.T) {
	h, _ := newTestHandler(t)
	created := decodeBody[authResponse](t, post(t, h.Register, "/api/v1/auth/register",
		map[string]string{"email": testEmail, "password": testPassword}))

	rec := post(t, h.Refresh, "/api/v1/auth/refresh", map[string]string{"refresh_token": created.Tokens.RefreshToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200: %s", rec.Code, rec.Body)
	}
	rotated := decodeBody[tokenResponse](t, rec)
	if rotated.RefreshToken == created.Tokens.RefreshToken {
		t.Fatal("refresh returned the same token")
	}

	if rec := post(t, h.Refresh, "/api/v1/auth/refresh", map[string]string{"refresh_token": created.Tokens.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed token status = %d, want 401", rec.Code)
	}

	if rec := post(t, h.Logout, "/api/v1/auth/logout", map[string]string{"refresh_token": rotated.RefreshToken}); rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204: %s", rec.Code, rec.Body)
	}
	if rec := post(t, h.Refresh, "/api/v1/auth/refresh", map[string]string{"refresh_token": rotated.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout status = %d, want 401", rec.Code)
	}
}

func TestMeHandlerRequiresValidToken(t *testing.T) {
	h, svc := newTestHandler(t)
	created := decodeBody[authResponse](t, post(t, h.Register, "/api/v1/auth/register",
		map[string]string{"email": testEmail, "password": testPassword, "name": "Ada"}))

	protected := RequireAuth(svc.tokens)(http.HandlerFunc(h.Me))

	t.Run("with a valid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+created.Tokens.AccessToken)
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		got := decodeBody[userResponse](t, rec)
		if got.ID != created.User.ID || got.Email != testEmail || got.Name != "Ada" {
			t.Fatalf("body = %+v", got)
		}
	})

	expired := NewTokenIssuer(testSecret, "lifeos-test", 15*time.Minute)
	expired.now = func() time.Time { return time.Now().Add(-time.Hour) }
	userID, err := uuid.Parse(created.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	staleToken, _, err := expired.Issue(userID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, header string }{
		{"no header", ""},
		{"wrong scheme", "Basic " + created.Tokens.AccessToken},
		{"empty bearer", "Bearer "},
		{"garbage token", "Bearer not-a-jwt"},
		{"refresh token used as access token", "Bearer " + created.Tokens.RefreshToken},
		{"expired token", "Bearer " + staleToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 without a WWW-Authenticate header")
			}
		})
	}
}

// A lowercase "bearer" scheme is valid per RFC 7235.
func TestRequireAuthAcceptsLowercaseScheme(t *testing.T) {
	h, svc := newTestHandler(t)
	created := decodeBody[authResponse](t, post(t, h.Register, "/api/v1/auth/register",
		map[string]string{"email": testEmail, "password": testPassword}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "bearer "+created.Tokens.AccessToken)
	rec := httptest.NewRecorder()
	RequireAuth(svc.tokens)(http.HandlerFunc(h.Me)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
