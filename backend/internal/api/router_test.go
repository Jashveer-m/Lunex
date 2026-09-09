package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/api"
	"github.com/jashveer/lifeos/backend/internal/auth"
)

// The router is exercised end to end against the real service; only the
// repositories are absent, so these tests need no database. Full persistence
// coverage lives in the integration tests (see docs/testing.md).
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	tokens := auth.NewTokenIssuer([]byte("router-test-secret-at-least-32-bytes"), "lifeos-test", 15*time.Minute)
	svc := auth.NewService(newMemoryUserStore(), newMemorySessionStore(), tokens, 24*time.Hour)
	handler := api.NewRouter(api.Deps{
		Auth:        auth.NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))),
		Tokens:      tokens,
		RateLimiter: auth.NewIPRateLimiter(100, 100),
		DB:          nil, // healthz degrades to a liveness check
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, srv *httptest.Server, method, path, bearer string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp, out
}

func TestFullAuthFlow(t *testing.T) {
	srv := newTestServer(t)
	const email, password = "ada@example.com", "correct horse battery staple"

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": email, "password": password, "name": "Ada", "timezone": "Europe/London",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201: %v", resp.StatusCode, body)
	}
	tokens, _ := body["tokens"].(map[string]any)
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("missing tokens in %v", body)
	}

	resp, me := doJSON(t, srv, http.MethodGet, "/api/v1/me", access, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d, want 200: %v", resp.StatusCode, me)
	}
	if me["email"] != email || me["name"] != "Ada" || me["timezone"] != "Europe/London" {
		t.Fatalf("me = %v", me)
	}

	resp, rotated := doJSON(t, srv, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200: %v", resp.StatusCode, rotated)
	}
	newRefresh, _ := rotated["refresh_token"].(string)
	if newRefresh == refresh {
		t.Fatal("refresh token was not rotated")
	}

	resp, _ = doJSON(t, srv, http.MethodPost, "/api/v1/auth/logout", "", map[string]string{"refresh_token": newRefresh})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	resp, _ = doJSON(t, srv, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refresh_token": newRefresh})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after logout status = %d, want 401", resp.StatusCode)
	}
}

func TestMeIsProtected(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := doJSON(t, srv, http.MethodGet, "/api/v1/me", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRouterSetsSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := doJSON(t, srv, http.MethodGet, "/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := doJSON(t, srv, http.MethodGet, "/api/v1/tasks", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
