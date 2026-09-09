package documents

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// pdfMagic is the header every PDF starts with. The extension alone is a
// client-supplied claim; this is the file actually saying what it is.
var pdfMagic = []byte("%PDF-")

// DetectType maps an upload to one of the types this phase can extract. It
// returns "" when the file is something else -- DOCX, an image, a CSV -- which
// the caller turns into a 415 rather than a failed document.
//
// The extension decides, and the content is then checked against it: a .txt
// holding a PDF is not silently indexed as its own binary, and a .pdf that is
// not a PDF is rejected before the parser is handed something it will only
// fail on later.
func DetectType(filename string, content []byte) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		if bytes.HasPrefix(content, pdfMagic) {
			return TypePDF
		}
	case ".txt", ".text":
		if isProbablyText(content) {
			return TypeText
		}
	case ".md", ".markdown":
		if isProbablyText(content) {
			return TypeMarkdown
		}
	}
	return ""
}

// isProbablyText rejects binary content offered under a text extension. UTF-8
// validity plus the absence of NUL is the cheap, reliable pair of checks: no
// real text file contains a NUL byte, and binary rarely decodes as valid UTF-8.
func isProbablyText(b []byte) bool {
	return utf8.Valid(b) && !bytes.ContainsRune(b, 0)
}

// Extract pulls plain text out of an upload. The returned error wraps
// ErrExtraction or ErrEmptyText so the caller can record the right message
// without inspecting the parser's own error text.
func Extract(fileType string, content []byte) (string, error) {
	var (
		text string
		err  error
	)
	switch fileType {
	case TypePDF:
		text, err = extractPDF(content)
	case TypeText, TypeMarkdown:
		// A UTF-8 BOM is invisible in an editor but becomes a stray rune at
		// the head of the first chunk, so it goes here rather than in the
		// chunker.
		text = strings.TrimPrefix(string(content), "\ufeff")
	default:
		// Unreachable: DetectType gates this. Kept so a new type added to one
		// switch and not the other fails loudly.
		return "", fmt.Errorf("%w: unsupported file type %q", ErrExtraction, fileType)
	}
	if err != nil {
		return "", err
	}

	text = normalize(text)
	if strings.TrimSpace(text) == "" {
		return "", ErrEmptyText
	}
	return text, nil
}

// extractPDF reads the text layer of a PDF.
//
// It reads the layer only: a scanned page is an image, carries no text, and
// yields nothing here. That is ErrEmptyText, not a crash, and OCR is a later
// phase.
func extractPDF(content []byte) (result string, err error) {
	// ledongthuc/pdf panics on some malformed files rather than returning an
	// error. A user-uploaded file is exactly the input that will find those,
	// and a panic in a request handler would take the process's goroutine with
	// it, so it is converted into the error the caller already handles.
	defer func() {
		if r := recover(); r != nil {
			result, err = "", fmt.Errorf("%w: the file could not be read as a PDF", ErrExtraction)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", fmt.Errorf("%w: the file could not be read as a PDF", ErrExtraction)
	}
	var buf strings.Builder
	pages := reader.NumPage()
	for i := 1; i <= pages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		// GetPlainText joins the page's text runs. A nil font map makes it
		// build one per page, which is what a page-at-a-time loop wants: the
		// alternative shares a map across pages that do not share fonts.
		text, perr := page.GetPlainText(nil)
		if perr != nil {
			// One unreadable page should not lose the other two hundred.
			continue
		}
		buf.WriteString(text)
		// Pages are separated by a blank line so a chunk boundary can land
		// between them instead of running two pages into one sentence.
		buf.WriteString("\n\n")
	}

	out := buf.String()
	if strings.TrimSpace(out) == "" && pages == 0 {
		return "", fmt.Errorf("%w: the PDF has no pages", ErrExtraction)
	}
	return out, nil
}

// ExtractReader is the streaming form used by the handler, which has an
// io.Reader and a byte ceiling rather than a slice.
func ExtractReader(fileType string, r io.Reader, limit int64) (string, error) {
	content, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrExtraction, err)
	}
	return Extract(fileType, content)
}

// normalize makes extracted text safe to store and sane to chunk: one line
// ending, no control characters that would corrupt a JSON response or a
// terminal, and no runs of blank lines from a PDF's page furniture.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == utf8.RuneError:
			// Invalid bytes from a mis-declared encoding; dropping them beats
			// storing a column of replacement characters.
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	s = b.String()

	return strings.TrimSpace(collapseBlankLines(s))
}

// collapseBlankLines caps a run of newlines at two: a blank line is a
// paragraph break worth keeping, ten of them are a PDF's page furniture worth
// losing. One pass, because repeated ReplaceAll over a megabyte of text is
// quadratic.
func collapseBlankLines(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	run := 0
	for _, r := range s {
		if r == '\n' {
			run++
			if run > 2 {
				continue
			}
		} else {
			run = 0
		}
		b.WriteRune(r)
	}
	return b.String()
}
