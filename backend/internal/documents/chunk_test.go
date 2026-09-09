package documents

import (
	"strings"
	"testing"
)

func TestSplitIntoChunksEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n\t\n"} {
		if got := SplitIntoChunks(in); got != nil {
			t.Fatalf("SplitIntoChunks(%q) = %v, want nil", in, got)
		}
	}
}

func TestSplitIntoChunksShortTextIsOneChunk(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog."
	got := SplitIntoChunks(text)
	if len(got) != 1 || got[0] != text {
		t.Fatalf("SplitIntoChunks(short) = %#v, want one chunk equal to the input", got)
	}
}

// Structure is what a citation quotes back, so the chunk has to be a slice of
// the original text, not a re-joined word list.
func TestSplitIntoChunksPreservesLineStructure(t *testing.T) {
	text := "# Heading\n\n- one\n- two\n\nA paragraph."
	got := SplitIntoChunks(text)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	if got[0] != text {
		t.Fatalf("chunk = %q, want the original text verbatim", got[0])
	}
}

func TestSplitIntoChunksOverlaps(t *testing.T) {
	// Distinct words so an overlap is identifiable rather than coincidental.
	words := make([]string, 0, ChunkWords*3)
	for i := range cap(words) {
		words = append(words, wordFor(i))
	}
	chunks := SplitIntoChunks(strings.Join(words, " "))
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks for %d words, want at least 3", len(chunks), len(words))
	}

	first := strings.Fields(chunks[0])
	second := strings.Fields(chunks[1])
	if len(first) != ChunkWords {
		t.Fatalf("first chunk has %d words, want %d", len(first), ChunkWords)
	}
	// The tail of one chunk is the head of the next, OverlapWords wide.
	wantHead := first[len(first)-OverlapWords:]
	gotHead := second[:OverlapWords]
	for i := range wantHead {
		if gotHead[i] != wantHead[i] {
			t.Fatalf("overlap word %d = %q, want %q", i, gotHead[i], wantHead[i])
		}
	}
}

// Every word must appear somewhere: an off-by-one in the stride would drop
// text silently, and the only symptom would be a document that cannot be found.
func TestSplitIntoChunksLosesNothing(t *testing.T) {
	words := make([]string, 0, ChunkWords*4+7)
	for i := range cap(words) {
		words = append(words, wordFor(i))
	}
	joined := strings.Join(SplitIntoChunks(strings.Join(words, " ")), " ")
	for _, w := range words {
		if !strings.Contains(joined, w) {
			t.Fatalf("word %q is in no chunk", w)
		}
	}
}

// A file with no whitespace -- base64, minified JSON, some scripts -- must
// still chunk, and must still terminate.
func TestSplitIntoChunksHandlesOneEnormousWord(t *testing.T) {
	text := strings.Repeat("a", MaxChunkBytes*3+17)
	got := SplitIntoChunks(text)
	if len(got) != 4 {
		t.Fatalf("got %d chunks, want 4", len(got))
	}
	if strings.Join(got, "") != text {
		t.Fatal("the pieces do not reassemble into the original text")
	}
	for i, c := range got {
		if len(c) > MaxChunkBytes {
			t.Fatalf("chunk %d is %d bytes, over the %d ceiling", i, len(c), MaxChunkBytes)
		}
	}
}

// The overlap stride must never move the cursor backwards, which is what would
// happen when MaxChunkBytes cuts a window shorter than OverlapWords.
func TestSplitIntoChunksTerminatesOnLongWords(t *testing.T) {
	long := strings.Repeat("x", MaxChunkBytes/2)
	text := strings.Repeat(long+" ", 20)
	got := SplitIntoChunks(text)
	if len(got) == 0 {
		t.Fatal("got no chunks")
	}
	for i, c := range got {
		if len(c) > MaxChunkBytes {
			t.Fatalf("chunk %d is %d bytes, over the %d ceiling", i, len(c), MaxChunkBytes)
		}
	}
}

// Multi-byte text must not be cut mid-rune when the byte ceiling applies.
func TestSplitIntoChunksNeverSplitsARune(t *testing.T) {
	// Three bytes per rune, so a naive cut at MaxChunkBytes lands inside one.
	text := strings.Repeat("日", MaxChunkBytes)
	for i, c := range SplitIntoChunks(text) {
		for _, r := range c {
			if r == '�' {
				t.Fatalf("chunk %d contains a broken rune", i)
			}
		}
	}
}

func wordFor(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return string([]byte{letters[i/676%26], letters[i/26%26], letters[i%26]})
}
