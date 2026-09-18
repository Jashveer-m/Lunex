package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/api"
	"github.com/jashveer/lifeos/backend/internal/auth"
	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/finance"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
	"github.com/jashveer/lifeos/backend/internal/users"
)

// The router is exercised end to end against the real service; only the
// repositories are absent, so these tests need no database. Full persistence
// coverage lives in the integration tests (see docs/testing.md).
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newServer(t, nil)
}

// newServer builds the whole route tree. Passing a nil pool is deliberate: the
// auth tests never reach a resource handler, so the Phase 2 repositories are
// wired but never queried, and the tests that do query them (see
// isolation_test.go) pass a real pool. Faking the three resource stores
// instead would only prove that the fakes scope by user — which is exactly the
// property worth testing against the real SQL.
func newServer(t *testing.T, pool *sql.DB) *httptest.Server {
	t.Helper()
	return newServerWithProvider(t, pool, &ai.Mock{})
}

// newServerWithProvider is the same tree with a chosen model, so a test can
// decide what the assistant answers.
func newServerWithProvider(t *testing.T, pool *sql.DB, provider *ai.Mock) *httptest.Server {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := auth.NewTokenIssuer([]byte("router-test-secret-at-least-32-bytes"), "lunex-test", 15*time.Minute)

	// With a pool, auth runs on the real repositories too: the Phase 2 tables
	// have a foreign key to users, so a user id invented by an in-memory store
	// could not own a task.
	var userStore auth.UserStore = newMemoryUserStore()
	var sessionStore auth.SessionStore = newMemorySessionStore()
	if pool != nil {
		userStore = users.NewRepository(pool)
		sessionStore = auth.NewSessionRepository(pool)
	}
	svc := auth.NewService(userStore, sessionStore, tokens, 24*time.Hour)
	// The document service gets a deterministic embedder rather than a live
	// Ollama, so these tests measure the routing and the SQL scoping and
	// nothing else. See embedder_test.go.
	// Phase 6's graph runs on the same mock model, and is wired into all four
	// resource services -- so these tests exercise the real sync-on-write path
	// and the real SQL behind it, which is where "a task always has a node"
	// actually has to hold.
	graphSvc := graph.NewService(graph.Deps{
		Store: graph.NewRepository(pool), Provider: provider, Logger: discard,
	})
	docSvc := documents.NewService(documents.NewRepository(pool), &hashingEmbedder{}, discard,
		30*time.Second, documents.WithNodeSync(graphSvc))
	taskSvc := tasks.NewService(tasks.NewRepository(pool), tasks.WithNodeSync(graphSvc))
	goalSvc := goals.NewService(goals.NewRepository(pool), goals.WithNodeSync(graphSvc))
	noteSvc := notes.NewService(notes.NewRepository(pool), notes.WithNodeSync(graphSvc))
	calendarSvc := calendar.NewService(calendar.NewRepository(pool), calendar.WithNodeSync(graphSvc))
	financeSvc := finance.NewService(finance.NewRepository(pool), finance.WithNodeSync(graphSvc))
	// The assistant runs against a mock model for the same reason the document
	// tests run against a deterministic embedder: what these tests measure is
	// the routing and the SQL scoping, not whether a model understood a
	// sentence. The genuine end-to-end check is scripts/e2e.sh.
	// Phase 5 runs on the same mock model and the same deterministic embedder:
	// what these tests measure is whether a memory belongs to the caller and
	// reaches the right prompt, not whether a model chose a good fact.
	memorySvc := memories.NewService(memories.Deps{
		Store: memories.NewRepository(pool), Provider: provider, Embedder: &hashingEmbedder{},
		Logger: discard,
	})
	// Phase 7 is wired exactly as cmd/api wires it: the registry over the real
	// services with the real actions table as its ledger, so these tests run
	// the genuine approval path against the genuine SQL -- including the gate
	// every write tool has to pass through.
	actionRepo := actions.NewRepository(pool)
	registry, err := tools.NewRegistry(actionRepo, tools.Standard(tools.Services{
		Tasks: taskSvc, Goals: goalSvc, Notes: noteSvc, Documents: docSvc,
		Calendar: calendarSvc, Finance: financeSvc,
		DocumentMinSimilarity: 0.5,
	})...)
	if err != nil {
		t.Fatal(err)
	}
	actionSvc := actions.NewService(actionRepo, registry, discard)
	chatSvc := chat.NewService(chat.Deps{
		Store: chat.NewRepository(pool), Provider: provider,
		Documents: docSvc, Memories: memorySvc, MemoryExtractor: memorySvc,
		Graph: graphSvc, GraphExtractor: graphSvc,
		Tasks: taskSvc, Goals: goalSvc, Notes: noteSvc, Calendar: calendarSvc,
		Router: agents.NewRouter(provider, registry, discard, agents.Options{}),
		Tools:  registry, Actions: actionSvc,
		Logger: discard,
		Options: chat.Options{
			// The memory floor is named here rather than left at its default.
			// memories.DefaultMinSimilarity is calibrated for
			// nomic-embed-text; hashingEmbedder is a bag of words, and its
			// scores are on a different scale, so carrying the production
			// number over would be testing an arbitrary threshold. What these
			// tests measure is whose memories are searched and where the
			// retrieved ones end up -- the floor itself is pinned in
			// internal/chat and internal/db, against numbers those tests set.
			MemoryMinSimilarity: 0.5,
		},
	})
	handler := api.NewRouter(api.Deps{
		Auth:        auth.NewHandler(svc, discard),
		Actions:     actions.NewHandler(actionSvc, discard),
		Tasks:       tasks.NewHandler(taskSvc, discard),
		Goals:       goals.NewHandler(goalSvc, discard),
		Notes:       notes.NewHandler(noteSvc, discard),
		Calendar:    calendar.NewHandler(calendarSvc, discard),
		Finance:     finance.NewHandler(financeSvc, discard),
		Documents:   documents.NewHandler(docSvc, discard, 0),
		Chat:        chat.NewHandler(chatSvc, discard),
		Memories:    memories.NewHandler(memorySvc, discard),
		Graph:       graph.NewHandler(graphSvc, discard),
		Tokens:      tokens,
		RateLimiter: auth.NewIPRateLimiter(100, 100),
		DB:          pool, // nil degrades healthz to a liveness check
		Logger:      discard,

		DocumentTimeout: 60 * time.Second,
		ChatTimeout:     60 * time.Second,
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
	resp, _ := doJSON(t, srv, http.MethodGet, "/api/v1/nonexistent", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Every Phase 2-8 route sits behind RequireAuth. This is the cheap half of the
// isolation story: without a token there is no user id on the context, so a
// handler never runs at all. The other half — one user reaching another
// user's rows — is in isolation_test.go, against real SQL.
func TestResourceRoutesRequireAuth(t *testing.T) {
	srv := newTestServer(t)
	id := "5c2c9b6f-0f8e-4f4c-9f2b-0f0d9b6f0f8e"
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tasks"},
		{http.MethodPost, "/api/v1/tasks"},
		{http.MethodGet, "/api/v1/tasks/" + id},
		{http.MethodPatch, "/api/v1/tasks/" + id},
		{http.MethodDelete, "/api/v1/tasks/" + id},
		{http.MethodPost, "/api/v1/tasks/" + id + "/dependencies"},
		{http.MethodGet, "/api/v1/goals"},
		{http.MethodPost, "/api/v1/goals"},
		{http.MethodGet, "/api/v1/goals/" + id},
		{http.MethodPatch, "/api/v1/goals/" + id},
		{http.MethodDelete, "/api/v1/goals/" + id},
		{http.MethodPost, "/api/v1/goals/" + id + "/milestones"},
		{http.MethodPatch, "/api/v1/goals/" + id + "/milestones/" + id},
		{http.MethodGet, "/api/v1/calendar"},
		{http.MethodPost, "/api/v1/calendar"},
		{http.MethodGet, "/api/v1/calendar/" + id},
		{http.MethodPatch, "/api/v1/calendar/" + id},
		{http.MethodDelete, "/api/v1/calendar/" + id},
		{http.MethodGet, "/api/v1/notes"},
		{http.MethodPost, "/api/v1/notes"},
		{http.MethodGet, "/api/v1/notes/" + id},
		{http.MethodPatch, "/api/v1/notes/" + id},
		{http.MethodDelete, "/api/v1/notes/" + id},
		{http.MethodGet, "/api/v1/documents"},
		{http.MethodPost, "/api/v1/documents"},
		{http.MethodPost, "/api/v1/documents/search"},
		{http.MethodGet, "/api/v1/documents/" + id},
		{http.MethodDelete, "/api/v1/documents/" + id},
		{http.MethodGet, "/api/v1/conversations"},
		{http.MethodPost, "/api/v1/conversations"},
		{http.MethodGet, "/api/v1/conversations/" + id},
		{http.MethodDelete, "/api/v1/conversations/" + id},
		{http.MethodPost, "/api/v1/conversations/" + id + "/messages"},
		{http.MethodGet, "/api/v1/memories"},
		{http.MethodDelete, "/api/v1/memories"},
		{http.MethodPatch, "/api/v1/memories/" + id},
		{http.MethodDelete, "/api/v1/memories/" + id},
		{http.MethodGet, "/api/v1/actions"},
		{http.MethodGet, "/api/v1/actions/" + id},
		{http.MethodPost, "/api/v1/actions/" + id + "/approve"},
		{http.MethodPost, "/api/v1/actions/" + id + "/reject"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp, _ := doJSON(t, srv, tc.method, tc.path, "", nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}
