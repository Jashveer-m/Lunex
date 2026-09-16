package chat

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// markCited is what turns "offered to the model" into "used". It has to find a
// real citation and ignore everything else that uses square brackets.
func TestMarkCitedReadsTheAnswer(t *testing.T) {
	sources := []Source{
		{Label: "S1", Title: "a"}, {Label: "S2", Title: "b"},
		{Label: "S3", Title: "c"}, {Label: "S4", Title: "d"},
	}

	for _, tc := range []struct {
		name   string
		answer string
		want   []string
	}{
		{"single", "Yes [S1].", []string{"S1"}},
		{"grouped", "Both [S1, S2] say so.", []string{"S1", "S2"}},
		{"adjacent", "See [S1][S3].", []string{"S1", "S3"}},
		{"none", "I could not find that in your data.", nil},
		{"a label that was not offered", "According to [S9].", nil},
		{"a markdown link is not a citation", "See [the docs](http://x) for S2.", nil},
		{"bare text is not a citation", "S1 and S2 are irrelevant here.", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := append([]Source(nil), sources...)
			got := markCited(fresh, tc.answer)
			var cited []string
			for _, s := range got {
				if s.Cited {
					cited = append(cited, s.Label)
				}
			}
			if strings.Join(cited, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("cited = %v, want %v", cited, tc.want)
			}
		})
	}
}

func TestMarkCitedOnNoSources(t *testing.T) {
	if got := markCited(nil, "I cited [S1] out of thin air."); got != nil {
		t.Fatalf("got %v, want nil -- there is nothing to mark", got)
	}
}

// An empty context is stated, not omitted: an absent section reads like an
// oversight, an explicitly empty one reads like an answer.
func TestContextBlockStatesAnEmptyResult(t *testing.T) {
	block := contextBlock(nil)
	if !strings.HasPrefix(block, "CONTEXT") {
		t.Fatalf("block = %q, want the section header", block)
	}
	if !strings.Contains(block, "Nothing relevant was found") {
		t.Fatalf("block = %q, want it to say retrieval found nothing", block)
	}
}

func TestContextBlockRendersOneHeaderPerSource(t *testing.T) {
	idx, sim := 3, 0.6123
	block := contextBlock([]Source{
		{Label: "S1", Type: SourceDocument, Title: "notes.md", ChunkIndex: &idx, Similarity: &sim, Excerpt: "body text"},
		{Label: "S2", Type: SourceTask, Title: "Ship it", Excerpt: "priority high"},
	})
	for _, want := range []string{
		`[S1] document: "notes.md" (chunk 3)`,
		"body text",
		`[S2] task: "Ship it"`,
		"priority high",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
	// The score is metadata for the client, not text for the model: shown it,
	// the model recited "a similarity of 0.61" to the user.
	if strings.Contains(block, "similarity") || strings.Contains(block, "0.61") {
		t.Fatalf("block leaks the retrieval score to the model:\n%s", block)
	}
}

// A source over the budget is dropped whole. Half a source would still be
// listed as retrieved while carrying none of the text it was cited for.
func TestContextBudgetDropsWholeSources(t *testing.T) {
	big := strings.Repeat("x", MaxContextChars/2)
	in := make([]Source, 6)
	for i := range in {
		in[i] = Source{Type: SourceNote, ID: uuid.New(), Title: "n", Excerpt: big}
	}

	out := labelled(in)
	if len(out) == 0 || len(out) >= len(in) {
		t.Fatalf("kept %d of %d sources, want some but not all", len(out), len(in))
	}
	used := 0
	for i, s := range out {
		if s.Label != label(i) {
			t.Fatalf("source %d is labelled %q, want %q -- labels must stay dense", i, s.Label, label(i))
		}
		if len(s.Excerpt) != len(big) {
			t.Fatalf("source %d was truncated rather than dropped", i)
		}
		used += len(s.Excerpt)
	}
	if used > MaxContextChars {
		t.Fatalf("kept %d characters, over the %d budget", used, MaxContextChars)
	}
}

// The first source always survives, however large: answering with no context
// at all because one chunk was long is worse than one oversized prompt.
func TestContextBudgetAlwaysKeepsTheStrongestMatch(t *testing.T) {
	out := labelled([]Source{{Type: SourceDocument, Title: "huge.md", Excerpt: strings.Repeat("y", MaxContextChars*2)}})
	if len(out) != 1 || out[0].Label != "S1" {
		t.Fatalf("got %d sources, want the strongest match kept", len(out))
	}
}

func TestDeriveTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"What did I write about the aurora?", "What did I write about the aurora?"},
		{"  first line\nsecond line  ", "first line"},
		{"lots   of\tspace", "lots of space"},
		{"", ""},
		{"   \n  ", ""},
		{strings.Repeat("a", 100), strings.Repeat("a", 60) + "…"},
	} {
		if got := deriveTitle(tc.in); got != tc.want {
			t.Fatalf("deriveTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Truncation must not split a rune: a half-encoded character in the prompt is
// a decoding error somewhere downstream.
func TestTruncateCountsRunes(t *testing.T) {
	if got := truncate("héllo wörld", 5); got != "héllo…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("short", 50); got != "short" {
		t.Fatalf("truncate left a mark on a string that fits: %q", got)
	}
	if got := truncate("日本語のテキスト", 3); got != "日本語…" {
		t.Fatalf("truncate = %q, want whole runes", got)
	}
}
