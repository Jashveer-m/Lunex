package documents

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Chunking targets. The brief asks for ~500-token chunks with ~50 tokens of
// overlap; nothing here tokenizes, because the only tokenizer that would be
// exact is the embedding model's own and a chunk 15% off target costs nothing
// (nomic-embed-text's context is 8k, sixteen times a chunk).
//
// English averages roughly 0.75 words per token, so the token budget is
// converted to words once, here, rather than sprinkled through the code as
// magic numbers.
const (
	ChunkTokens   = 500
	OverlapTokens = 50

	// 500 * 0.75 and 50 * 0.75, rounded. Written out because Go will not
	// convert a non-integral constant expression to int.
	ChunkWords   = 375
	OverlapWords = 38
)

// MaxChunkBytes is a hard ceiling for text that is not words at all -- minified
// JSON, base64, a language that does not space-separate. Without it a single
// "word" could be the whole file, and the model server would silently truncate
// it, indexing a vector that does not describe most of the chunk's content.
const MaxChunkBytes = 4000

// MaxChunks bounds one document's work. Processing is synchronous in this
// phase, so this is what keeps a large upload from holding an HTTP request
// open past any reasonable timeout; it is not a storage limit. Roughly 1.5 MB
// of prose. See docs/decisions.md.
const MaxChunks = 800

// SplitIntoChunks splits text into overlapping windows.
//
// Windows are cut on word boundaries and sliced out of the original string, so
// paragraph breaks, indentation and Markdown structure survive into the stored
// chunk -- what gets embedded is what a citation will quote back.
func SplitIntoChunks(text string) []string {
	words := wordSpans(text)
	if len(words) == 0 {
		return nil
	}

	var out []string
	for i := 0; i < len(words); {
		j := i
		for j < len(words) && j-i < ChunkWords && words[j].end-words[i].start <= MaxChunkBytes {
			j++
		}
		if j == i {
			// One word longer than the whole budget; take it and let the
			// splitter below deal with it.
			j = i + 1
		}

		for _, piece := range splitLong(strings.TrimSpace(text[words[i].start:words[j-1].end])) {
			out = append(out, piece)
		}
		if j >= len(words) {
			break
		}

		next := j - OverlapWords
		// A window cut short by MaxChunkBytes can be narrower than the
		// overlap, which would move the cursor backwards and never terminate.
		// Giving up the overlap is the right trade there: that text is not
		// prose, so straddling a sentence boundary was never the concern.
		if next <= i {
			next = j
		}
		i = next
	}
	return out
}

// splitLong breaks a chunk that exceeded the byte ceiling into pieces, never
// mid-rune.
func splitLong(s string) []string {
	if len(s) <= MaxChunkBytes {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var out []string
	for len(s) > MaxChunkBytes {
		cut := MaxChunkBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = MaxChunkBytes
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// span is one word's byte range in the source text.
type span struct{ start, end int }

// wordSpans records where each whitespace-separated run begins and ends,
// rather than returning the words themselves, so the caller can slice the
// original text and keep the whitespace between them.
func wordSpans(text string) []span {
	var out []span
	start := -1
	for i, r := range text {
		if unicode.IsSpace(r) {
			if start >= 0 {
				out = append(out, span{start, i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, span{start, len(text)})
	}
	return out
}
