package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// frames renders a slice of contents as the newline-delimited JSON Ollama
// sends, ending with the done frame.
func frames(contents ...string) string {
	var b strings.Builder
	for _, c := range contents {
		b.WriteString(`{"message":{"role":"assistant","content":` + quote(c) + `},"done":false}` + "\n")
	}
	b.WriteString(`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}` + "\n")
	return b.String()
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func serve(t *testing.T, handler http.HandlerFunc) *Ollama {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewOllama(srv.URL, "test-model", 5*time.Second)
}

// The property a streaming client depends on: the pieces reassemble into
// exactly what the model produced, in order.
func TestOllamaStreamsInOrder(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, frames("The ", "aurora ", "appeared\nover the tundra."))
	})

	stream, err := provider.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var pieces []string
	for stream.Next() {
		pieces = append(pieces, stream.Text())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
	if got, want := strings.Join(pieces, ""), "The aurora appeared\nover the tundra."; got != want {
		t.Fatalf("assembled = %q, want %q", got, want)
	}
	if len(pieces) != 3 {
		t.Fatalf("got %d pieces, want 3 -- the stream was buffered, not streamed", len(pieces))
	}
}

// Only knobs the caller set are sent, so the model's own defaults survive.
func TestOllamaRequestShape(t *testing.T) {
	var body map[string]any
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		io.WriteString(w, frames("ok"))
	})

	for _, tc := range []struct {
		name    string
		opts    Options
		wantOpt bool
	}{
		{"defaults", Options{}, false},
		{"tuned", Options{Temperature: 0.2, MaxTokens: 256, Model: "override"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body = nil
			stream, err := provider.Chat(context.Background(),
				[]Message{{Role: RoleSystem, Content: "rules"}, {Role: RoleUser, Content: "hi"}}, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if _, err := Collect(stream); err != nil {
				t.Fatal(err)
			}

			if body["stream"] != true {
				t.Fatalf("stream = %v, want true", body["stream"])
			}
			wantModel := "test-model"
			if tc.opts.Model != "" {
				wantModel = tc.opts.Model
			}
			if body["model"] != wantModel {
				t.Fatalf("model = %v, want %v", body["model"], wantModel)
			}
			msgs, _ := body["messages"].([]any)
			if len(msgs) != 2 {
				t.Fatalf("sent %d messages, want 2", len(msgs))
			}
			if first, _ := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "rules" {
				t.Fatalf("first message = %v, want the system message unchanged", msgs[0])
			}
			opts, present := body["options"]
			if present != tc.wantOpt {
				t.Fatalf("options present = %v, want %v", present, tc.wantOpt)
			}
			if tc.wantOpt {
				o, _ := opts.(map[string]any)
				if o["temperature"] != 0.2 || o["num_predict"] != float64(256) {
					t.Fatalf("options = %v, want temperature 0.2 and num_predict 256", o)
				}
			}
		})
	}
}

// Everything that fails before generation must fail at Chat, where the HTTP
// layer can still pick a status code.
func TestOllamaRefusalIsUnavailable(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model \"nope\" not found"}`)
	})

	_, err := provider.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the upstream message named", err)
	}
}

func TestOllamaUnreachableIsUnavailable(t *testing.T) {
	// A port nothing is listening on.
	provider := NewOllama("http://127.0.0.1:1", "test-model", time.Second)
	if _, err := provider.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// A mid-stream problem arrives in-band, with a 200 already sent.
func TestOllamaInBandErrorStopsTheStream(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"content":"partial"},"done":false}`+"\n")
		io.WriteString(w, `{"error":"out of memory"}`+"\n")
	})

	stream, err := provider.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	text, err := Collect(stream)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("err = %v, want ErrUnavailable naming the cause", err)
	}
	if text != "partial" {
		t.Fatalf("text = %q, want the piece that did arrive", text)
	}
}

// A body that stops without done=true is a truncated answer. Reporting it is
// what keeps a half-generated reply from being stored as a finished one.
func TestOllamaTruncatedStreamIsAnError(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"content":"half an ans"},"done":false}`+"\n")
	})

	stream, err := provider.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if _, err := Collect(stream); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable for a stream that ended early", err)
	}
}

// A cancelled caller is the caller's own doing, not an outage: mislabelling it
// would turn an abandoned request into a 503 in the logs.
func TestOllamaCancelledCallerIsNotAnOutage(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, frames("hello"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := provider.Chat(ctx, []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, must not be reported as an outage", err)
	}
}

// The done frame can carry text; dropping it would lose the last word.
func TestOllamaKeepsContentOnTheDoneFrame(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"message":{"content":"all "},"done":false}`+"\n")
		io.WriteString(w, `{"message":{"content":"done"},"done":true}`+"\n")
	})

	stream, err := provider.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	text, err := Collect(stream)
	if err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
	if text != "all done" {
		t.Fatalf("text = %q, want %q", text, "all done")
	}
}

func TestOllamaRejectsAnEmptyPrompt(t *testing.T) {
	provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the model must not be called with no messages")
	})
	if _, err := provider.Chat(context.Background(), nil, Options{}); err == nil {
		t.Fatal("want an error for an empty prompt")
	}
}

// JSON mode is the difference between asking a model for JSON and constraining
// it to produce JSON, and the memory extractor depends on it. It must be sent
// only when a caller asked for it: an ordinary chat turn constrained to JSON
// would answer every question with a quoted string.
func TestOllamaSendsFormatOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"an ordinary turn", Options{}, ""},
		{"the extractor", Options{Format: FormatJSON}, "json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			provider := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				io.WriteString(w, frames("ok"))
			})

			stream, err := provider.Chat(context.Background(),
				[]Message{{Role: RoleUser, Content: "hi"}}, tc.opts)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			defer stream.Close()
			if _, err := Collect(stream); err != nil {
				t.Fatal(err)
			}

			got, present := body["format"]
			if tc.want == "" {
				if present {
					t.Fatalf("format = %v, want the field absent", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("format = %v, want %q", got, tc.want)
			}
		})
	}
}
