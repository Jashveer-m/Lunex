package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The mock is used by three packages' tests, so its own contract is worth
// pinning: the pieces reassemble into the reply, and the prompt is recorded.
func TestMockStreamsTheWholeReply(t *testing.T) {
	m := &Mock{Reply: "Yes, that is in [S1] your notes."}
	stream, err := m.Chat(context.Background(), []Message{{Role: RoleUser, Content: "is it?"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	text, err := Collect(stream)
	if err != nil {
		t.Fatal(err)
	}
	if text != m.Reply {
		t.Fatalf("text = %q, want %q", text, m.Reply)
	}
	if got := m.LastPrompt(); len(got) != 1 || got[0].Content != "is it?" {
		t.Fatalf("LastPrompt = %v, want the prompt it was given", got)
	}
}

func TestMockReplyFuncSeesThePrompt(t *testing.T) {
	m := &Mock{ReplyFunc: func(msgs []Message) string {
		if strings.Contains(PromptText(msgs), "[S1]") {
			return "From [S1]."
		}
		return "I could not find that in your data."
	}}

	stream, _ := m.Chat(context.Background(), []Message{{Role: RoleSystem, Content: "CONTEXT\n[S1] note: x"}}, Options{})
	text, _ := Collect(stream)
	if text != "From [S1]." {
		t.Fatalf("text = %q", text)
	}

	stream, _ = m.Chat(context.Background(), []Message{{Role: RoleSystem, Content: "CONTEXT\n(nothing)"}}, Options{})
	text, _ = Collect(stream)
	if !strings.Contains(text, "could not find") {
		t.Fatalf("text = %q", text)
	}
	if len(m.Calls()) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(m.Calls()))
	}
}

func TestMockFailureModes(t *testing.T) {
	boom := errors.New("boom")

	if _, err := (&Mock{Err: boom}).Chat(context.Background(), []Message{{}}, Options{}); !errors.Is(err, boom) {
		t.Fatalf("Err = %v, want the configured failure", err)
	}

	// StreamErr is the other failure: the call succeeded, the generation did
	// not finish. A caller must be able to tell the two apart.
	m := &Mock{Reply: "one two three", StreamErr: boom}
	stream, err := m.Chat(context.Background(), []Message{{}}, Options{})
	if err != nil {
		t.Fatalf("Chat = %v, want the call itself to succeed", err)
	}
	text, err := Collect(stream)
	if !errors.Is(err, boom) {
		t.Fatalf("Err = %v, want the stream failure", err)
	}
	if text == "" || text == "one two three" {
		t.Fatalf("text = %q, want a partial reply", text)
	}
}
