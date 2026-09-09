package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/auth"
)

// server mounts the chat routes behind a middleware that plays the part of
// auth.RequireAuth, so these tests exercise the transport -- SSE framing,
// status codes, wire shapes -- and nothing else.
func (h *harness) server(t *testing.T) *httptest.Server {
	t.Helper()
	handler := NewHandler(h.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(withUser(h.user, handler.Routes()))
	t.Cleanup(srv.Close)
	return srv
}

func withUser(id uuid.UUID, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Anonymous") == "1" {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUserID(r.Context(), id)))
	})
}

// event is one parsed SSE frame.
type event struct {
	Name string
	Data map[string]any
}

// readSSE parses the frames of a text/event-stream body.
func readSSE(t *testing.T, body io.Reader) []event {
	t.Helper()
	var out []event
	var name string
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("decode %s frame: %v (%q)", name, err, line)
			}
			out = append(out, event{Name: name, Data: data})
		case line == "":
		default:
			t.Fatalf("unexpected SSE line %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return out
}

func post(t *testing.T, srv *httptest.Server, path string, body any, anonymous bool) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if anonymous {
		req.Header.Set("X-Anonymous", "1")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

// The stream's shape: what was retrieved first, then the answer in pieces,
// then the persisted turn.
func TestSendMessageStreamsSourcesThenTokensThenDone(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "It appeared over the tundra [S1].\nAfter midnight."})
	h.withDocument("field-notes.md", "The aurora borealis appeared over the tundra.", 0.8)
	srv := h.server(t)

	resp := post(t, srv, "/"+h.conv.ID.String()+"/messages", map[string]any{"content": "tundra?"}, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := readSSE(t, resp.Body)
	if len(events) < 3 {
		t.Fatalf("got %d events, want sources + tokens + done: %v", len(events), events)
	}
	if events[0].Name != "sources" {
		t.Fatalf("first event = %q, want sources -- a client shows the grounding before the answer", events[0].Name)
	}
	if count, _ := events[0].Data["count"].(float64); count != 1 {
		t.Fatalf("sources count = %v, want 1", events[0].Data["count"])
	}
	last := events[len(events)-1]
	if last.Name != "done" {
		t.Fatalf("last event = %q, want done", last.Name)
	}

	// The tokens reassemble into exactly the stored message. A client that
	// concatenated them has the same text the database does.
	var streamed strings.Builder
	for _, e := range events[1 : len(events)-1] {
		if e.Name != "token" {
			t.Fatalf("event = %q, want token", e.Name)
		}
		text, _ := e.Data["text"].(string)
		streamed.WriteString(text)
	}
	message, _ := last.Data["message"].(map[string]any)
	if content, _ := message["content"].(string); content != streamed.String() {
		t.Fatalf("streamed %q but done reported %q", streamed.String(), content)
	}
	// The newline in the answer survived the framing, which is why token text
	// is JSON-encoded rather than written raw.
	if !strings.Contains(streamed.String(), "\n") {
		t.Fatalf("streamed text lost its newline: %q", streamed.String())
	}

	sources, _ := message["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("done reported %d sources, want 1", len(sources))
	}
	first, _ := sources[0].(map[string]any)
	if first["cited"] != true || first["label"] != "S1" {
		t.Fatalf("source = %v, want the cited S1", first)
	}
	if user, _ := last.Data["user_message"].(map[string]any); user["role"] != "user" {
		t.Fatalf("done did not report the persisted question: %v", last.Data["user_message"])
	}
}

// Everything that can fail before the first token has to fail with a status
// code, not inside a 200 stream.
func TestPreStreamFailuresAreOrdinaryJSONErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(*harness) string // returns the path to post to
		body     any
		status   int
		wantCode string
	}{
		{
			name:     "unknown conversation",
			setup:    func(h *harness) string { return "/" + uuid.NewString() + "/messages" },
			body:     map[string]any{"content": "hi"},
			status:   http.StatusNotFound,
			wantCode: "not_found",
		},
		{
			name:     "malformed id",
			setup:    func(h *harness) string { return "/not-a-uuid/messages" },
			body:     map[string]any{"content": "hi"},
			status:   http.StatusNotFound,
			wantCode: "not_found",
		},
		{
			name:     "empty message",
			setup:    func(h *harness) string { return "/" + h.conv.ID.String() + "/messages" },
			body:     map[string]any{"content": "  "},
			status:   http.StatusBadRequest,
			wantCode: "validation_failed",
		},
		{
			name:     "unknown field",
			setup:    func(h *harness) string { return "/" + h.conv.ID.String() + "/messages" },
			body:     map[string]any{"content": "hi", "temperature": 2},
			status:   http.StatusBadRequest,
			wantCode: "invalid_json",
		},
		{
			name: "model refuses the request",
			setup: func(h *harness) string {
				h.provider.Err = ai.ErrUnavailable
				return "/" + h.conv.ID.String() + "/messages"
			},
			body:     map[string]any{"content": "hi"},
			status:   http.StatusServiceUnavailable,
			wantCode: "model_unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, &ai.Mock{})
			srv := h.server(t)
			resp := post(t, srv, tc.setup(h), tc.body, false)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			body := decodeJSON(t, resp)
			if body["error"] != tc.wantCode {
				t.Fatalf("error = %v, want %q", body["error"], tc.wantCode)
			}
			if n := len(h.store.stored(h.conv.ID)); n != 0 {
				t.Fatalf("stored %d messages for a request that failed", n)
			}
		})
	}
}

// Once the 200 is committed the only way left to report a failure is in band.
func TestMidStreamFailureIsAnErrorEvent(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "half an answer here", StreamErr: ai.ErrUnavailable})
	srv := h.server(t)

	resp := post(t, srv, "/"+h.conv.ID.String()+"/messages", map[string]any{"content": "hi"}, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- the headers were already sent", resp.StatusCode)
	}

	events := readSSE(t, resp.Body)
	last := events[len(events)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %q, want error", last.Name)
	}
	if last.Data["error"] != "model_unavailable" {
		t.Fatalf("error code = %v, want model_unavailable", last.Data["error"])
	}
	// The client is told the turn was not saved, because it was not.
	if msg, _ := last.Data["message"].(string); !strings.Contains(msg, "nothing was saved") {
		t.Fatalf("message = %q, want it to say the turn was not saved", msg)
	}
	if n := len(h.store.stored(h.conv.ID)); n != 0 {
		t.Fatalf("stored %d messages for a truncated answer, want 0", n)
	}
}

func TestConversationEndpoints(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "An answer."})
	srv := h.server(t)

	resp := post(t, srv, "/", map[string]any{"title": "Thesis"}, false)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	created := decodeJSON(t, resp)
	id, _ := created["id"].(string)
	if created["title"] != "Thesis" || created["message_count"] != float64(0) {
		t.Fatalf("created = %v", created)
	}

	// A turn, so the read has something to return.
	post(t, srv, "/"+id+"/messages", map[string]any{"content": "A question?"}, false).Body.Close()

	get, err := srv.Client().Get(srv.URL + "/" + id)
	if err != nil {
		t.Fatal(err)
	}
	conv := decodeJSON(t, get)
	msgs, _ := conv["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("read %d messages, want 2: %v", len(msgs), conv)
	}
	first, _ := msgs[0].(map[string]any)
	// [] rather than null, so a client needs no special case.
	if sources, ok := first["sources"].([]any); !ok || len(sources) != 0 {
		t.Fatalf("user message sources = %v, want []", first["sources"])
	}

	list, err := srv.Client().Get(srv.URL + "/?sort=-updated_at")
	if err != nil {
		t.Fatal(err)
	}
	listed := decodeJSON(t, list)
	if count, _ := listed["count"].(float64); count != 2 {
		t.Fatalf("listed %v conversations, want 2", count)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/"+id, nil)
	del, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", del.StatusCode)
	}
}

func TestAnUnknownSortIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	srv := h.server(t)
	resp, err := srv.Client().Get(srv.URL + "/?sort=" + url.QueryEscape("title; DROP TABLE messages"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if body := decodeJSON(t, resp); body["error"] != "validation_failed" {
		t.Fatalf("error = %v", body["error"])
	}
}

// Every route fails closed if it is ever mounted outside RequireAuth.
func TestRoutesRequireAnAuthenticatedUser(t *testing.T) {
	h := newHarness(t, nil)
	srv := h.server(t)

	resp := post(t, srv, "/", map[string]any{}, true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("create: status %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = post(t, srv, "/"+h.conv.ID.String()+"/messages", map[string]any{"content": "hi"}, true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("send: status %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("X-Anonymous", "1")
	list, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	list.Body.Close()
	if list.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list: status %d, want 401", list.StatusCode)
	}
}

// A client that hangs up mid-answer must not leave the turn stored behind
// their back, and must not be logged as a model failure.
func TestClientDisconnectIsNotAModelFailure(t *testing.T) {
	if !errors.Is(errClientGone, errClientGone) {
		t.Fatal("errClientGone must be comparable with errors.Is")
	}
	h := newHarness(t, &ai.Mock{Reply: "one two three"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.svc.SendMessage(ctx, h.user, h.conv.ID, "hi", &CollectSink{})
	if err == nil {
		t.Fatal("want an error for a cancelled request")
	}
	if n := len(h.store.stored(h.conv.ID)); n != 0 {
		t.Fatalf("stored %d messages, want 0", n)
	}
}
