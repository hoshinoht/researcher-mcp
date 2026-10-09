package fulltext

import (
	"bytes"
	"context"
	"fmt"
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

// pdfText is extracted PDF text split into pages.
type pdfText struct {
	Pages     []string
	Converter string
	Truncated bool
}

func (p *pdfText) joined() string { return strings.Join(p.Pages, "\n\n") }

// convertPDF extracts per-page text, preferring poppler's pdftotext (best
// reading order on 2-column academic PDFs) and falling back to the pure-Go
// extractor. Cancellation or deadline expiry during pdftotext is returned as
// is: it never triggers a fresh pure-Go extraction. Output is bounded by
// maxChars.
func convertPDF(ctx context.Context, cfg config.Config, body []byte, maxChars int) (*pdfText, error) {
	if maxChars <= 0 {
		maxChars = 2_000_000
	}
	if path := pdftotextPath(cfg); path != "" {
		text, truncated, err := runPdftotext(ctx, path, body, maxChars*4)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil && strings.TrimSpace(text) != "" {
			pages, cut := boundPages(splitPages(text), maxChars)
			return &pdfText{Pages: pages, Converter: converterPdftotext, Truncated: truncated || cut}, nil
		}
	}

	type result struct {
		pages []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		pages, err := goPDFPages(ctx, body, maxChars)
		done <- result{pages, err}
	}()
	select {
	case <-ctx.Done():
		// The pure-Go extractor checks ctx between pages; the goroutine
		// exits at the next page boundary.
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		pages, cut := boundPages(r.pages, maxChars)
		return &pdfText{Pages: pages, Converter: converterGoPDF, Truncated: cut}, nil
	}
}

// boundPages trims pages so their total length stays within maxChars.
func boundPages(pages []string, maxChars int) ([]string, bool) {
	total := 0
	for i, p := range pages {
		n := len([]rune(p))
		if total+n > maxChars {
			rest := maxChars - total
			if rest > 0 {
				return append(pages[:i:i], string([]rune(p)[:rest])), true
			}
			return pages[:i], true
		}
		total += n
	}
	return pages, false
}

// splitPages splits pdftotext output on form feeds, dropping the empty tail
// after the final page break.
func splitPages(text string) []string {
	pages := strings.Split(text, "\f")
	for len(pages) > 1 && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	return pages
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

// PdftotextAvailable reports whether pdftotext would be used, without
// running it.
func PdftotextAvailable(cfg config.Config) (string, bool) {
	path := pdftotextPath(cfg)
	if path == "" {
		return "", false
	}
	if _, err := exec.LookPath(path); err != nil {
		return path, false
	}
	return path, true
}

// capWriter keeps at most max bytes and records whether more arrived.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

// runPdftotext streams the PDF through stdin/stdout — no temp files. Default
// mode (not -layout): layout mode interleaves 2-column text side by side.
func runPdftotext(ctx context.Context, path string, body []byte, maxBytes int) (string, bool, error) {
	cmd := exec.CommandContext(ctx, path, "-enc", "UTF-8", "-eol", "unix", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	stdout := &capWriter{max: maxBytes}
	stderr := &capWriter{max: 4096}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("pdftotext failed: %w (%s)", err, strings.TrimSpace(stderr.buf.String()))
	}
	return strings.ToValidUTF8(stdout.buf.String(), ""), stdout.truncated, nil
}

// goPDFText is the pure-Go extractor over the whole document.
func goPDFText(body []byte) (string, error) {
	pages, err := goPDFPages(context.Background(), body, 0)
	if err != nil {
		return "", err
	}
	return strings.Join(pages, "\n"), nil
}

// goPDFPages extracts text page by page, stopping at cancellation or once
// maxChars (if positive) is exceeded. The library can panic on malformed
// PDFs, so extraction is wrapped in a recover.
func goPDFPages(ctx context.Context, body []byte, maxChars int) (pages []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf extraction panicked: %v", r)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("pdf open failed: %w", err)
	}
	total := 0
	for i := 1; i <= reader.NumPage(); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			return nil, fmt.Errorf("pdf text extraction failed on page %d: %w", i, err)
		}
		pages = append(pages, text)
		total += len(text)
		if maxChars > 0 && total > maxChars*4 {
			break
		}
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("pdf has no extractable pages")
	}
	return pages, nil
}

var (
	hyphenBreakPattern = regexp.MustCompile(`([a-zA-Z])-\n([a-z])`)
	manyNewlines       = regexp.MustCompile(`\n{3,}`)
	sectionHeading     = regexp.MustCompile(`(?i)^(?:\d+(?:\.\d+)*\.?\s+)?(abstract|introduction|background|related work|methods?|methodology|approach|experiments?|experimental setup|results?|discussion|evaluation|conclusions?|future work|references|acknowledge?ments?|appendix(?:\s+[a-z])?)\s*$`)
)

// cleanExtractedText normalizes raw extractor output for one page: rejoins
// hyphenated line breaks, collapses blank runs, and conservatively promotes
// obvious section titles to markdown headings.
func cleanExtractedText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = hyphenBreakPattern.ReplaceAllString(text, "$1$2")
	text = strings.ReplaceAll(text, "\f", "\n\n")

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
