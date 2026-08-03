package fulltext

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"googlescholar-mcp-go/internal/config"
)

// makeTestPDF builds a minimal single-page PDF with the given text lines and
// a correctly computed xref table.
func makeTestPDF(t *testing.T, lines []string) []byte {
	t.Helper()

	var content strings.Builder
	y := 720
	for _, line := range lines {
		fmt.Fprintf(&content, "BT /F1 12 Tf 72 %d Td (%s) Tj ET\n", y, line)
		y -= 14
	}
	stream := content.String()

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefPos)
	return buf.Bytes()
}

var testPDFLines = []string{
	"Sample Extraction Paper",
	"Abstract",
	"This paper exists purely to exercise the PDF text extraction pipeline.",
	"1 Introduction",
	"Extraction quality matters for downstream language models.",
}

func TestGoPDFTextExtractsText(t *testing.T) {
	text, err := goPDFText(makeTestPDF(t, testPDFLines))
	if err != nil {
		t.Fatalf("goPDFText error: %v", err)
	}
	if !strings.Contains(text, "Sample Extraction Paper") {
		t.Fatalf("extracted text missing title: %q", text)
	}
}

func TestGoPDFTextRejectsGarbage(t *testing.T) {
	if _, err := goPDFText([]byte("%PDF-1.4 this is not a real pdf")); err == nil {
		t.Fatal("expected error for malformed PDF")
	}
}

func TestConvertPDFWithPdftotext(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}

	text, converter, err := convertPDF(context.Background(), config.Config{}, makeTestPDF(t, testPDFLines))
	if err != nil {
		t.Fatalf("convertPDF error: %v", err)
	}
	if converter != converterPdftotext {
		t.Fatalf("converter = %q, want %q", converter, converterPdftotext)
	}
	if !strings.Contains(text, "Sample Extraction Paper") {
		t.Fatalf("text missing title: %q", text)
	}
	if !strings.Contains(text, "## Abstract") {
		t.Fatalf("Abstract not promoted to heading: %q", text)
	}
}

func TestConvertPDFFallbackWhenDisabled(t *testing.T) {
	text, converter, err := convertPDF(context.Background(), config.Config{PdftotextPath: "off"}, makeTestPDF(t, testPDFLines))
	if err != nil {
		t.Fatalf("convertPDF error: %v", err)
	}
	if converter != converterGoPDF {
		t.Fatalf("converter = %q, want %q", converter, converterGoPDF)
	}
	if !strings.Contains(text, "Sample Extraction Paper") {
		t.Fatalf("text missing title: %q", text)
	}
}

func TestCleanExtractedText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dehyphenate", "extrac-\ntion works", "extraction works"},
		{"formfeed", "page one\fpage two", "page one\n\n---\n\npage two"},
		{"collapse", "a\n\n\n\n\nb", "a\n\nb"},
		{"heading plain", "Abstract\nbody text", "## Abstract\nbody text"},
		{"heading numbered", "3.1 Results\nbody", "## 3.1 Results\nbody"},
		{"no heading mid-sentence", "the results of this study", "the results of this study"},
	}
	for _, c := range cases {
		if got := cleanExtractedText(c.in); got != c.want {
			t.Errorf("%s: cleanExtractedText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
