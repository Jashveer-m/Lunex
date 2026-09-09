package memories

import (
	"strings"
	"testing"
)

// The parser is the piece between a model that was asked for JSON and a table
// with a CHECK constraint on it. These pin both halves of its contract: it
// accepts the shapes a model actually produces, and it invents nothing.

func TestParseExtractionAcceptsTheShapesModelsProduce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{"a bare array", `[{"type":"preference","content":"The user prefers studying in the morning.","importance":0.7,"confidence":0.9}]`},
		{"a fenced array", "```json\n[{\"type\":\"preference\",\"content\":\"The user prefers studying in the morning.\",\"importance\":0.7,\"confidence\":0.9}]\n```"},
		{"prose then an array", `Here is what I found:
[{"type":"preference","content":"The user prefers studying in the morning.","importance":0.7,"confidence":0.9}]`},
		{"a wrapper object", `{"memories":[{"type":"preference","content":"The user prefers studying in the morning.","importance":0.7,"confidence":0.9}]}`},
		{"one bare object", `{"type":"preference","content":"The user prefers studying in the morning.","importance":0.7,"confidence":0.9}`},
		{"quoted scores", `[{"type":"preference","content":"The user prefers studying in the morning.","importance":"0.7","confidence":"0.9"}]`},
		{"the documented line fallback", `preference | 0.7 | 0.9 | The user prefers studying in the morning.`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseExtraction(tc.reply)
			if len(got) != 1 {
				t.Fatalf("parsed %d facts, want 1: %+v", len(got), got)
			}
			if got[0].Type != TypePreference {
				t.Fatalf("type = %q, want %q", got[0].Type, TypePreference)
			}
			if got[0].Content != "The user prefers studying in the morning." {
				t.Fatalf("content = %q", got[0].Content)
			}
			if got[0].Importance != 0.7 || got[0].Confidence != 0.9 {
				t.Fatalf("scores = %v / %v, want 0.7 / 0.9", got[0].Importance, got[0].Confidence)
			}
		})
	}
}

// The common case, and the one the prompt asks for most often: nothing here is
// worth remembering.
func TestParseExtractionReturnsNothingForAnEmptyAnswer(t *testing.T) {
	for _, reply := range []string{
		"[]",
		"```json\n[]\n```",
		"",
		"   ",
		"I could not find anything worth remembering in this exchange.",
		"{}",
		`[{"type":"semantic","content":"   "}]`,
	} {
		if got := ParseExtraction(reply); got != nil {
			t.Fatalf("reply %q parsed as %+v, want nothing", reply, got)
		}
	}
}

// A reply that is not JSON in any accepted shape yields no facts rather than a
// guess: the failure mode of a memory system is remembering things that were
// never said.
func TestParseExtractionInventsNothing(t *testing.T) {
	for _, reply := range []string{
		"Sure! I'd be happy to help with that.",
		"<thinking>the user seems tired</thinking>",
		"null",
		`{"error":"model overloaded"}`,
	} {
		if got := ParseExtraction(reply); len(got) != 0 {
			t.Fatalf("reply %q produced %+v, want nothing", reply, got)
		}
	}
}

func TestParseExtractionCapsTheNumberOfFacts(t *testing.T) {
	reply := `[
		{"type":"semantic","content":"one","confidence":0.9},
		{"type":"semantic","content":"two","confidence":0.9},
		{"type":"semantic","content":"three","confidence":0.9},
		{"type":"semantic","content":"four","confidence":0.9},
		{"type":"semantic","content":"five","confidence":0.9}
	]`
	got := ParseExtraction(reply)
	if len(got) != MaxFactsPerTurn {
		t.Fatalf("parsed %d facts, want the cap of %d", len(got), MaxFactsPerTurn)
	}
	if got[0].Content != "one" {
		t.Fatalf("the cap dropped from the wrong end: %+v", got)
	}
}

func TestParseExtractionDropsRepeatsWithinOneReply(t *testing.T) {
	got := ParseExtraction(`[
		{"type":"semantic","content":"The user knows Go."},
		{"type":"semantic","content":"the user knows go."}
	]`)
	if len(got) != 1 {
		t.Fatalf("parsed %d facts, want the repeat collapsed: %+v", len(got), got)
	}
}

// Scores are the model's own judgement and arrive as whatever it felt like
// writing. They still have to land inside the column's CHECK constraint.
func TestParseExtractionClampsScores(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reply      string
		importance float64
		confidence float64
	}{
		{"above the range", `[{"content":"x","importance":80,"confidence":5}]`, 1, 1},
		{"below the range", `[{"content":"x","importance":-1,"confidence":-0.5}]`, 0, 0},
		{"missing", `[{"content":"x"}]`, DefaultScore, DefaultScore},
		{"unreadable", `[{"content":"x","importance":"high","confidence":"sure"}]`, DefaultScore, DefaultScore},
		{"null", `[{"content":"x","importance":null,"confidence":null}]`, DefaultScore, DefaultScore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseExtraction(tc.reply)
			if len(got) != 1 {
				t.Fatalf("parsed %d facts, want 1", len(got))
			}
			if got[0].Importance != tc.importance || got[0].Confidence != tc.confidence {
				t.Fatalf("scores = %v / %v, want %v / %v",
					got[0].Importance, got[0].Confidence, tc.importance, tc.confidence)
			}
		})
	}
}

// The type is a facet the user filters on, not what retrieval matches. An
// unrecognised one falls back to the neutral bucket rather than costing the
// fact -- but it must never reach the column, which has a CHECK on it.
func TestParseExtractionKeepsTheTypeInsideTheAllowList(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{"preference", TypePreference},
		{"PREFERENCE", TypePreference},
		{"  episodic  ", TypeEpisodic},
		{"personal", TypeSemantic},
		{"", TypeSemantic},
		{"'; DROP TABLE memories; --", TypeSemantic},
	} {
		got := ParseExtraction(`[{"type":"` + strings.ReplaceAll(tc.written, `'`, ``) + `","content":"x"}]`)
		if len(got) != 1 {
			t.Fatalf("type %q: parsed %d facts", tc.written, len(got))
		}
		if got[0].Type != tc.want {
			t.Fatalf("type %q became %q, want %q", tc.written, got[0].Type, tc.want)
		}
	}
}

// A model that ignores "content" reaches for one of two other names.
func TestParseExtractionAcceptsTheOtherFieldNames(t *testing.T) {
	for _, reply := range []string{
		`[{"type":"semantic","fact":"The user knows Go."}]`,
		`[{"type":"semantic","text":"The user knows Go."}]`,
	} {
		got := ParseExtraction(reply)
		if len(got) != 1 || got[0].Content != "The user knows Go." {
			t.Fatalf("reply %q parsed as %+v", reply, got)
		}
	}
}

func TestParseExtractionTruncatesAnOversizedFact(t *testing.T) {
	long := strings.Repeat("a", MaxContentLen+500)
	got := ParseExtraction(`[{"type":"semantic","content":"` + long + `"}]`)
	if len(got) != 1 {
		t.Fatalf("parsed %d facts, want 1", len(got))
	}
	if n := len([]rune(got[0].Content)); n > MaxContentLen+1 {
		t.Fatalf("content is %d runes, want it cut to %d plus the ellipsis", n, MaxContentLen)
	}
}

// The line fallback must not read prose as facts -- a model explaining itself
// would otherwise become a memory.
func TestLineFallbackIgnoresProse(t *testing.T) {
	got := ParseExtraction("I did not find anything durable in this exchange.\nNothing to remember.")
	if len(got) != 0 {
		t.Fatalf("prose parsed as %+v, want nothing", got)
	}
}

// --- the threshold ----------------------------------------------------------

func TestWorthExtracting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		user      string
		assistant string
		want      bool
	}{
		{
			name: "a greeting", user: "hi", assistant: "Hello! How can I help?",
			want: false,
		},
		{
			name: "an acknowledgement", user: "thanks", assistant: "You're welcome.",
			want: false,
		},
		{
			name: "whitespace padding does not buy a call",
			user: strings.Repeat(" ", 300), assistant: "Hello!",
			want: false,
		},
		{
			name: "a real exchange",
			user: "I prefer studying in the morning before class, so I want my revision blocks scheduled early.",
			assistant: "Noted -- early revision blocks it is. Your calendar has nothing before 09:00 on weekdays, " +
				"so the 07:00-09:00 window is free every day this week.",
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorthExtracting(tc.user, tc.assistant); got != tc.want {
				t.Fatalf("WorthExtracting = %v, want %v", got, tc.want)
			}
		})
	}
}

// The exchange the model is asked about must actually contain the exchange,
// and must not be shaped like a conversation to continue.
func TestExtractionPromptCarriesTheExchange(t *testing.T) {
	prompt := ExtractionPrompt("I am learning Rust for my systems course.", "That is a good fit for the course.")
	if len(prompt) != 2 {
		t.Fatalf("prompt has %d messages, want a system rule and one user message", len(prompt))
	}
	if !strings.Contains(prompt[1].Content, "I am learning Rust") ||
		!strings.Contains(prompt[1].Content, "That is a good fit") {
		t.Fatalf("the exchange did not reach the model: %q", prompt[1].Content)
	}
	// The user's own words are quoted material inside one message, not replayed
	// as a user turn the model would answer again.
	if prompt[1].Role != "user" || !strings.Contains(prompt[1].Content, "EXCHANGE") {
		t.Fatalf("the exchange is not fenced as material: %q", prompt[1].Content)
	}
	for _, want := range []string{"0 and 3", "episodic", "semantic", "preference", "project", "goal", "JSON"} {
		if !strings.Contains(prompt[0].Content, want) {
			t.Fatalf("the extraction rules do not mention %q", want)
		}
	}
}

func TestExtractionPromptTruncatesAHugeExchange(t *testing.T) {
	huge := strings.Repeat("word ", 5_000)
	prompt := ExtractionPrompt(huge, huge)
	if n := len([]rune(prompt[1].Content)); n > 2*MaxExchangeChars+500 {
		t.Fatalf("prompt is %d runes; each half should be cut to %d", n, MaxExchangeChars)
	}
}
