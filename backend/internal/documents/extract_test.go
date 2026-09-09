package documents

import (
	"errors"
	"strings"
	"testing"
)

func TestDetectType(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		content  string
		want     string
	}{
		{"plain text", "notes.txt", "hello", TypeText},
		{"markdown", "README.md", "# hi", TypeMarkdown},
		{"markdown long extension", "README.markdown", "# hi", TypeMarkdown},
		{"uppercase extension", "NOTES.TXT", "hello", TypeText},
		{"pdf", "paper.pdf", "%PDF-1.7\n...", TypePDF},
		// Deferred formats: rejected up front, not attempted and failed.
		{"docx is deferred", "report.docx", "PK\x03\x04", ""},
		{"csv is deferred", "rows.csv", "a,b\n1,2", ""},
		{"image is deferred", "scan.png", "\x89PNG", ""},
		{"no extension", "LICENSE", "text", ""},
		// The extension is a claim; the bytes are the evidence.
		{"pdf extension without a pdf", "fake.pdf", "not a pdf at all", ""},
		{"txt extension holding binary", "fake.txt", "\x00\x01\x02", ""},
		{"txt extension holding invalid utf-8", "fake.txt", "\xff\xfe\xfd", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectType(tc.filename, []byte(tc.content)); got != tc.want {
				t.Fatalf("DetectType(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	got, err := Extract(TypeText, []byte("line one\r\nline two\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "line one\nline two" {
		t.Fatalf("Extract = %q, want CRLF normalized and trailing space trimmed", got)
	}
}

// A BOM is invisible in an editor but becomes a stray rune at the head of the
// first chunk, and therefore of the first citation.
func TestExtractStripsBOM(t *testing.T) {
	got, err := Extract(TypeMarkdown, []byte("\ufeff# Title"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "# Title" {
		t.Fatalf("Extract = %q, want the BOM stripped", got)
	}
}

func TestExtractCollapsesBlankLines(t *testing.T) {
	got, err := Extract(TypeText, []byte("a\n\n\n\n\n\nb"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\n\nb" {
		t.Fatalf("Extract = %q, want blank-line runs collapsed to one", got)
	}
}

// Control characters would survive into a JSON response and a terminal;
// tabs and newlines are the two that carry meaning.
func TestExtractDropsControlCharacters(t *testing.T) {
	got, err := Extract(TypeText, []byte("a\x07b\tc\nd"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "ab\tc\nd" {
		t.Fatalf("Extract = %q, want the bell dropped and tab/newline kept", got)
	}
}

func TestExtractEmptyIsAnError(t *testing.T) {
	for _, in := range []string{"", "   \n\t  "} {
		if _, err := Extract(TypeText, []byte(in)); !errors.Is(err, ErrEmptyText) {
			t.Fatalf("Extract(%q) error = %v, want ErrEmptyText", in, err)
		}
	}
}

// A file that claims to be a PDF and is not must fail as an extraction error,
// including when the parser panics rather than returning one.
func TestExtractGarbagePDF(t *testing.T) {
	cases := map[string]string{
		"header only":   "%PDF-1.4\n",
		"truncated":     "%PDF-1.4\n1 0 obj\n<< /Type /Catalog",
		"random binary": "%PDF-1.4\n\x00\xff\x01\xfe garbage",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Extract(TypePDF, []byte(content))
			if err == nil {
				t.Fatal("want an error for a malformed PDF")
			}
			if !errors.Is(err, ErrExtraction) && !errors.Is(err, ErrEmptyText) {
				t.Fatalf("error = %v, want ErrExtraction or ErrEmptyText", err)
			}
		})
	}
}

func TestExtractRealPDF(t *testing.T) {
	text, err := Extract(TypePDF, testPDF(t))
	if err != nil {
		t.Fatalf("Extract(pdf) = %v", err)
	}
	if !strings.Contains(text, "Aurora") {
		t.Fatalf("extracted text %q does not contain the word in the PDF", text)
	}
}

func TestExtractUnknownTypeIsRejected(t *testing.T) {
	if _, err := Extract("docx", []byte("PK")); !errors.Is(err, ErrExtraction) {
		t.Fatalf("error = %v, want ErrExtraction", err)
	}
}
