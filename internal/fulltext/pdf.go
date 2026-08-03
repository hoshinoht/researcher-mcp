package fulltext

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"googlescholar-mcp-go/internal/config"
	"github.com/ledongthuc/pdf"
)

const (
	converterPdftotext = "pdftotext"
	converterGoPDF     = "go-pdf"
)

// convertPDF extracts text from a PDF, preferring the poppler pdftotext
// binary (best reading order on 2-column academic PDFs) and falling back to
// the pure-Go extractor. Returns the cleaned text and which converter ran.
func convertPDF(ctx context.Context, cfg config.Config, body []byte) (string, string, error) {
	if path := pdftotextPath(cfg); path != "" {
		text, err := runPdftotext(ctx, path, body)
		if err == nil && strings.TrimSpace(text) != "" {
			return cleanExtractedText(text), converterPdftotext, nil
		}
	}

	text, err := goPDFText(body)
	if err != nil {
		return "", converterGoPDF, err
	}
	return cleanExtractedText(text), converterGoPDF, nil
}

// pdftotextPath resolves the poppler binary: SCHOLAR_PDFTOTEXT_PATH overrides
// ("off" disables the subprocess entirely), otherwise PATH lookup.
func pdftotextPath(cfg config.Config) string {
	switch cfg.PdftotextPath {
	case "off":
		return ""
	case "":
		path, err := exec.LookPath("pdftotext")
		if err != nil {
			return ""
		}
		return path
	default:
		return cfg.PdftotextPath
	}
}

// runPdftotext streams the PDF through stdin/stdout — no temp files. Default
// mode (not -layout): layout mode interleaves 2-column text side by side.
func runPdftotext(ctx context.Context, path string, body []byte) (string, error) {
	cmd := exec.CommandContext(ctx, path, "-enc", "UTF-8", "-eol", "unix", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// goPDFText is the pure-Go fallback. The library can panic on malformed
// PDFs, so extraction is wrapped in a recover.
func goPDFText(body []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf extraction panicked: %v", r)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", fmt.Errorf("pdf open failed: %w", err)
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("pdf text extraction failed: %w", err)
	}
	raw, err := io.ReadAll(plain)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

var (
	hyphenBreakPattern = regexp.MustCompile(`([a-zA-Z])-\n([a-z])`)
	manyNewlines       = regexp.MustCompile(`\n{3,}`)
	sectionHeading     = regexp.MustCompile(`(?i)^(?:\d+(?:\.\d+)*\.?\s+)?(abstract|introduction|background|related work|methods?|methodology|approach|experiments?|experimental setup|results?|discussion|evaluation|conclusions?|future work|references|acknowledge?ments?|appendix(?:\s+[a-z])?)\s*$`)
)

// cleanExtractedText normalizes raw extractor output: rejoins hyphenated
// line breaks, turns form feeds into horizontal rules, collapses blank runs,
// and conservatively promotes obvious section titles to markdown headings.
func cleanExtractedText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = hyphenBreakPattern.ReplaceAllString(text, "$1$2")
	text = strings.ReplaceAll(text, "\f", "\n\n---\n\n")

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 0 && len(trimmed) < 60 && sectionHeading.MatchString(trimmed) {
			lines[i] = "## " + trimmed
		}
	}
	text = strings.Join(lines, "\n")

	text = manyNewlines.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}
