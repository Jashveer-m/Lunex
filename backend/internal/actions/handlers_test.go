package actions

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/auth"
)

func (e *engine) server(t *testing.T) *httptest.Server {
	t.Helper()
	h := NewHandler(e.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Anonymous") != "1" {
			r = r.WithContext(auth.ContextWithUserID(r.Context(), e.user))
		}
		h.Routes().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, contentType, body string, anonymous bool) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if anonymous {
		req.Header.Set("X-Anonymous", "1")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestApproveOverHTTP(t *testing.T) {
	e := newEngine()
	srv := e.server(t)
	a := e.propose(e.user, "Renew the passport")

	status, body := call(t, srv, http.MethodGet, "/"+a.ID.String(), "", "", false)
	if status != http.StatusOK || body["status"] != StatusProposed || body["permission_level"] != "write" {
		t.Fatalf("GET = %d %v", status, body)
	}
	if body["summary"] != `Create a task "Renew the passport".` || body["result"] != nil {
		t.Fatalf("GET = %v", body)
	}

	status, body = call(t, srv, http.MethodPost, "/"+a.ID.String()+"/approve", "", "", false)
	if status != http.StatusOK || body["status"] != StatusExecuted {
		t.Fatalf("approve = %d %v", status, body)
	}
	result, _ := body["result"].(map[string]any)
	task, _ := result["task"].(map[string]any)
	if task["title"] != "Renew the passport" {
		t.Fatalf("result = %v", body["result"])
	}

	// A second approve is a conflict that names the state it found.
	status, body = call(t, srv, http.MethodPost, "/"+a.ID.String()+"/approve", "application/json", "{}", false)
	if status != http.StatusConflict || body["error"] != "action_not_pending" {
		t.Fatalf("second approve = %d %v", status, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "executed") {
		t.Fatalf("message = %q, want it to say the action is executed", msg)
	}
	if e.tasks.count() != 1 {
		t.Fatalf("created %d tasks", e.tasks.count())
	}
}

// Approve and reject take no parameters, and a body that tries to pass one is
// rejected rather than ignored -- a client must never come to believe it
// edited a proposal on the way through. Nothing runs.
func TestApproveAndRejectBodiesAreStrict(t *testing.T) {
	e := newEngine()
	srv := e.server(t)
	a := e.propose(e.user, "Renew the passport")

	for _, tc := range []struct {
		name, contentType, body string
		status                  int
		code                    string
	}{
		{"an edit to the input", "application/json", `{"input": {"title": "Something else"}}`, 400, "invalid_json"},
		{"a confirm flag", "application/json", `{"confirm": true}`, 400, "invalid_json"},
		{"not json", "application/json", `approve it`, 400, "invalid_json"},
		{"two objects", "application/json", `{}{}`, 400, "invalid_json"},
		{"the wrong content type", "text/plain", `{}`, 415, "unsupported_media_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, verb := range []string{"approve", "reject"} {
				status, body := call(t, srv, http.MethodPost, "/"+a.ID.String()+"/"+verb, tc.contentType, tc.body, false)
				if status != tc.status || body["error"] != tc.code {
					t.Fatalf("%s = %d %v, want %d %s", verb, status, body, tc.status, tc.code)
				}
			}
		})
	}
	if e.tasks.count() != 0 || e.store.get(a.ID).Status != StatusProposed {
		t.Fatal("a refused request changed the action")
	}
}

func TestRejectOverHTTP(t *testing.T) {
	e := newEngine()
	srv := e.server(t)
	a := e.propose(e.user, "Renew the passport")

	status, body := call(t, srv, http.MethodPost, "/"+a.ID.String()+"/reject", "", "", false)
	if status != http.StatusOK || body["status"] != StatusRejected {
		t.Fatalf("reject = %d %v", status, body)
	}
	status, body = call(t, srv, http.MethodPost, "/"+a.ID.String()+"/approve", "", "", false)
	if status != http.StatusConflict || !strings.Contains(body["message"].(string), "rejected") {
		t.Fatalf("approve after reject = %d %v", status, body)
	}
	if e.tasks.count() != 0 {
		t.Fatal("a rejected action ran")
	}
}

// A foreign action and a malformed id are both 404 -- a 409 on somebody else's
// action would confirm its id exists.
func TestForeignAndMalformedIDsAreNotFound(t *testing.T) {
	e := newEngine()
	srv := e.server(t)
	theirs := e.propose(uuid.New(), "Their task")

	for _, path := range []string{
		"/" + theirs.ID.String(),
		"/" + theirs.ID.String() + "/approve",
		"/" + theirs.ID.String() + "/reject",
		"/not-a-uuid",
		"/not-a-uuid/approve",
	} {
		method := http.MethodPost
		if !strings.Contains(path[1:], "/") {
			method = http.MethodGet
		}
		status, body := call(t, srv, method, path, "", "", false)
		if status != http.StatusNotFound || body["error"] != "not_found" {
			t.Fatalf("%s %s = %d %v, want 404", method, path, status, body)
		}
	}
	if e.store.get(theirs.ID).Status != StatusProposed || e.tasks.count() != 0 {
		t.Fatal("another user's action was touched")
	}
}

func TestListFiltersAreValidated(t *testing.T) {
	e := newEngine()
	srv := e.server(t)
	e.propose(e.user, "a")
	done := e.propose(e.user, "b")
	if _, err := e.svc.Reject(t.Context(), e.user, done.ID); err != nil {
		t.Fatal(err)
	}

	status, body := call(t, srv, http.MethodGet, "/?status=proposed", "", "", false)
	if status != http.StatusOK || body["count"] != float64(1) {
		t.Fatalf("?status=proposed = %d %v", status, body)
	}
	for _, q := range []string{"?status=done", "?permission_level=admin", "?conversation_id=nope", "?limit=many", "?offset=-1"} {
		if status, body := call(t, srv, http.MethodGet, "/"+q, "", "", false); status != http.StatusBadRequest {
			t.Fatalf("GET %s = %d %v, want 400", q, status, body)
		}
	}
}

func TestEveryRouteRequiresAUser(t *testing.T) {
	e := newEngine()
	srv := e.server(t)
	id := uuid.NewString()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/" + id},
		{http.MethodPost, "/" + id + "/approve"},
		{http.MethodPost, "/" + id + "/reject"},
	} {
		if status, _ := call(t, srv, tc.method, tc.path, "", "", true); status != http.StatusUnauthorized {
			t.Fatalf("%s %s anonymously = %d, want 401", tc.method, tc.path, status)
		}
	}
}
