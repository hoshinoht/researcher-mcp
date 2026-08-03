package fulltext

import (
	"context"
	"strings"
	"testing"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"
)

func longHTMLPage(paragraphs int) []byte {
	b := strings.Builder{}
	b.WriteString("<html><body><article><h1>Cached Paper</h1>")
	for range paragraphs {
		b.WriteString("<p>This paragraph pads the article body far beyond the minimum content threshold used by the extractor.</p>")
	}
	b.WriteString("</article></body></html>")
	return []byte(b.String())
}

func TestGetPaperContentEndToEndHTML(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"example.org/paper": {Status: 200, Body: longHTMLPage(20), ContentType: "text/html"},
	}}
	svc := NewService(fetcher, config.Config{})

	content, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/paper"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if content.SourceType != docKindHTML || content.Converter != "html-to-markdown" {
		t.Fatalf("source_type=%q converter=%q", content.SourceType, content.Converter)
	}
	if !strings.Contains(content.Markdown, "Cached Paper") {
		t.Fatalf("markdown missing title: %q", content.Markdown[:200])
	}
	if !strings.Contains(content.Markdown, "> Source: https://example.org/paper") {
		t.Fatalf("markdown missing source header: %q", content.Markdown[:200])
	}
	if content.Truncated || content.NextOffset != 0 {
		t.Fatalf("small doc should not be truncated: %+v", content)
	}
	if content.TotalChars != len([]rune(content.Markdown)) {
		t.Fatalf("TotalChars=%d, markdown len=%d", content.TotalChars, len([]rune(content.Markdown)))
	}
}

func TestGetPaperContentPaginationAndCache(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"example.org/paper": {Status: 200, Body: longHTMLPage(50), ContentType: "text/html"},
	}}
	svc := NewService(fetcher, config.Config{})

	req := Request{URL: "https://example.org/paper"}
	page1, toolErr := svc.GetPaperContent(context.Background(), req, 600, 0)
	if toolErr != nil {
		t.Fatalf("page1 error: %+v", toolErr)
	}
	if !page1.Truncated || page1.NextOffset == 0 {
		t.Fatalf("page1 should be truncated: %+v", page1)
	}
	fetchesAfterPage1 := len(fetcher.calls)

	page2, toolErr := svc.GetPaperContent(context.Background(), req, 600, page1.NextOffset)
	if toolErr != nil {
		t.Fatalf("page2 error: %+v", toolErr)
	}
	if len(fetcher.calls) != fetchesAfterPage1 {
		t.Fatalf("page2 refetched: calls went from %d to %d", fetchesAfterPage1, len(fetcher.calls))
	}
	if page2.Offset != page1.NextOffset {
		t.Fatalf("page2 offset = %d, want %d", page2.Offset, page1.NextOffset)
	}
	if strings.HasPrefix(page2.Markdown, page1.Markdown[:50]) {
		t.Fatal("page2 repeats page1 content")
	}

	// Offset past the end is an input error.
	if _, toolErr = svc.GetPaperContent(context.Background(), req, 600, page1.TotalChars+1); toolErr == nil || toolErr.Code != "invalid_input" {
		t.Fatalf("out-of-range offset: %+v, want invalid_input", toolErr)
	}
}

func TestGetPaperContentAdvancesToNextCandidate(t *testing.T) {
	// arXiv chain: native HTML 404s (fakeFetcher default), ar5iv succeeds.
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"ar5iv.labs.arxiv.org": {Status: 200, Body: longHTMLPage(20), ContentType: "text/html"},
	}}
	svc := NewService(fetcher, config.Config{})

	content, toolErr := svc.GetPaperContent(context.Background(), Request{ArxivID: "1706.03762"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if !strings.Contains(content.SourceURL, "ar5iv") {
		t.Fatalf("SourceURL = %q, want ar5iv fallback", content.SourceURL)
	}
	if len(fetcher.calls) < 2 {
		t.Fatalf("expected at least 2 fetches, got %v", fetcher.calls)
	}
}

func TestGetPaperContentFollowsCitationPDFMeta(t *testing.T) {
	landing := []byte(`<html><head><meta name="citation_pdf_url" content="https://example.org/files/real.pdf"></head><body><p>Download our paper.</p></body></html>`)
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"example.org/landing": {Status: 200, Body: landing, ContentType: "text/html"},
	}}
	svc := NewService(fetcher, config.Config{})

	_, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/landing"}, 0, 0)
	if toolErr == nil {
		t.Fatal("expected error (pdf target 404s)")
	}

	followed := false
	for _, call := range fetcher.calls {
		if strings.Contains(call, "files/real.pdf") {
			followed = true
		}
	}
	if !followed {
		t.Fatalf("citation_pdf_url was not followed; calls: %v", fetcher.calls)
	}
}

func TestGetPaperContentShortContentFails(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"example.org/stub": {Status: 200, Body: []byte("<html><body><p>tiny</p></body></html>"), ContentType: "text/html"},
	}}
	svc := NewService(fetcher, config.Config{})

	_, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/stub"}, 0, 0)
	if toolErr == nil || toolErr.Code != "parse_failed" {
		t.Fatalf("toolErr = %+v, want parse_failed", toolErr)
	}
}
