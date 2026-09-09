package documents

import (
	"bytes"
	"fmt"
	"testing"
)

// testPDF builds a minimal one-page PDF whose text layer says "Aurora
// borealis over the tundra."
//
// It is generated rather than checked in as a base64 blob so the structure a
// test depends on -- a page, a font resource, a text-showing operator -- is
// readable, and so the cross-reference offsets are computed instead of being a
// magic table that silently rots if the content changes.
func testPDF(t *testing.T) []byte {
	t.Helper()

	const line = "Aurora borealis over the tundra."
	content := fmt.Sprintf("BT\n/F1 24 Tf\n72 700 Td\n(%s) Tj\nET\n", line)

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref)

	return buf.Bytes()
}
