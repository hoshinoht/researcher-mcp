package fulltext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"
)

// makeMultiPagePDF builds a PDF with one page per entry, each page holding
// the given text lines.
func makeMultiPagePDF(t *testing.T, pages [][]string) []byte {
	t.Helper()
	n := len(pages)
	// Objects: 1 catalog, 2 pages, 3 font, then (page, content) pairs.
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	kids := []string{}
	for i, lines := range pages {
		pageObj, contentObj := 4+2*i, 5+2*i
		kids = append(kids, fmt.Sprintf("%d 0 R", pageObj))
		var content strings.Builder
		y := 740
		for _, line := range lines {
			fmt.Fprintf(&content, "BT /F1 10 Tf 50 %d Td (%s) Tj ET\n", y, line)
			y -= 12
		}
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", contentObj),
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n)

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}

func filler(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s line %d continues the discussion of the experimental findings.", prefix, i)
	}
	return out
}

// fullPaperPDF is a 3-page PDF with recognisable paper structure.
func fullPaperPDF(t *testing.T) []byte {
	page1 := append([]string{"Robust Paper", "Abstract", "We study robustness.", "1 Introduction"}, filler("Intro", 40)...)
	page2 := append([]string{"2 Methods"}, filler("Methods", 40)...)
	page3 := append(append([]string{"3 Results"}, filler("Results", 30)...), "References", "[1] Prior work.")
	return makeMultiPagePDF(t, [][]string{page1, page2, page3})
}

func requirePdftotext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
}

func htmlDoc(body []byte) *scholar.FetchedDoc {
	return &scholar.FetchedDoc{Status: 200, Body: body, ContentType: "text/html"}
}

func pdfDoc(body []byte) *scholar.FetchedDoc {
	return &scholar.FetchedDoc{Status: 200, Body: body, ContentType: "application/pdf"}
}

func TestGetPaperContentEndToEndHTML(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"example.org/paper": htmlDoc(loadFixture(t, "paper_full.html")),
	}}
	svc := NewService(fetcher, config.Config{})

	content, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/paper"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if content.SourceType != docKindHTML || content.Converter != "html-to-markdown" || content.ContentStatus != StatusFullText {
		t.Fatalf("source_type=%q converter=%q status=%q", content.SourceType, content.Converter, content.ContentStatus)
	}
	if content.Title != "Structured Full Paper" || !strings.HasPrefix(content.Markdown, "# Structured Full Paper") {
		t.Fatalf("title = %q markdown = %q", content.Title, content.Markdown[:80])
	}
	if !strings.Contains(content.Markdown, "> Source: https://example.org/paper") || strings.Contains(content.Markdown, "Content status") {
		t.Fatalf("markdown header unexpected: %q", content.Markdown[:200])
	}
	if content.DocumentID != "url:example.org/paper" || !strings.HasPrefix(content.ContentID, "sha256:") {
		t.Fatalf("ids = %q %q", content.DocumentID, content.ContentID)
	}
	if content.Truncated || content.NextOffset != 0 {
		t.Fatalf("small doc should not be truncated: %+v", content)
	}
	if content.TotalChars != len([]rune(content.Markdown)) {
		t.Fatalf("TotalChars=%d, markdown len=%d", content.TotalChars, len([]rune(content.Markdown)))
	}
	headings := []string{}
	for _, s := range content.Provenance.Sections {
		headings = append(headings, s.Heading)
		if !strings.HasPrefix(string([]rune(content.Markdown)[s.Offset:]), "#") {
			t.Fatalf("section %q offset %d does not point at a heading", s.Heading, s.Offset)
		}
	}
	if !strings.Contains(strings.Join(headings, "|"), "2 Methods") {
		t.Fatalf("sections = %v", headings)
	}
}

func TestGetPaperContentPaginationAndCache(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"example.org/paper": htmlDoc(loadFixture(t, "paper_full.html")),
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
	fetchesAfterPage1 := len(fetcher.callsSnapshot())

	page2, toolErr := svc.GetPaperContent(context.Background(), req, 600, page1.NextOffset)
	if toolErr != nil {
		t.Fatalf("page2 error: %+v", toolErr)
	}
	if len(fetcher.callsSnapshot()) != fetchesAfterPage1 {
		t.Fatalf("page2 refetched")
	}
	if page2.Offset != page1.NextOffset || page2.ContentID != page1.ContentID {
		t.Fatalf("page2 offset/content id mismatch: %d %q vs %d %q", page2.Offset, page2.ContentID, page1.NextOffset, page1.ContentID)
	}
	joined := []rune(page1.Markdown + page2.Markdown)
	if len([]rune(page1.Markdown)) != page1.NextOffset || string(joined[:20]) != string([]rune(page1.Markdown)[:20]) {
		t.Fatal("page boundaries do not line up with rune offsets")
	}
	if page2.Provenance.SectionAtOffset == "" {
		t.Fatalf("page2 should report the section it starts in: %+v", page2.Provenance)
	}

	if _, toolErr = svc.GetPaperContent(context.Background(), req, 600, page1.TotalChars+1); toolErr == nil || toolErr.Code != "invalid_input" {
		t.Fatalf("out-of-range offset: %+v, want invalid_input", toolErr)
	}
}

func TestGetPaperContentAdvancesToNextCandidate(t *testing.T) {
	// arXiv chain: native HTML 404s (default), ar5iv succeeds.
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"ar5iv.labs.arxiv.org": htmlDoc(loadFixture(t, "paper_full.html")),
	}}
	svc := NewService(fetcher, config.Config{})

	content, toolErr := svc.GetPaperContent(context.Background(), Request{ArxivID: "1706.03762"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if !strings.Contains(content.SourceURL, "ar5iv") || content.DocumentID != "arxiv:1706.03762" || content.SourceVersion != "submittedVersion" {
		t.Fatalf("content = %+v", content)
	}
	if len(content.Attempts) != 2 || content.Attempts[0].Outcome != scholar.CodeNoResults {
		t.Fatalf("attempts = %+v", content.Attempts)
	}
}

func TestLongAbstractLandingPageFollowsCitationPDF(t *testing.T) {
	requirePdftotext(t)
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"publisher.example.org/article/1": htmlDoc(loadFixture(t, "landing_long_abstract.html")),
		"publisher.example.org/doi/pdf/":  pdfDoc(fullPaperPDF(t)),
	}}
	svc := NewService(fetcher, config.Config{})
	content, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://publisher.example.org/article/1"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if content.ContentStatus != StatusFullText || content.SourceType != docKindPDF || content.SourceURL != "https://publisher.example.org/doi/pdf/10.1000/landing" {
		t.Fatalf("content = %s %s %s", content.ContentStatus, content.SourceType, content.SourceURL)
	}
	if content.SourceProvider != "embedded:citation_pdf_url" {
		t.Fatalf("source_provider = %q", content.SourceProvider)
	}
	if content.Provenance.TotalPages != 3 || content.Provenance.StartPage != 1 || content.Provenance.EndPage != 3 {
		t.Fatalf("provenance = %+v", content.Provenance)
	}
	if !strings.Contains(content.Markdown, "<!-- page 2 -->") {
		t.Fatal("page markers missing")
	}
}

func TestLongAbstractIsNotLabelledFullText(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"publisher.example.org/article/1": htmlDoc(loadFixture(t, "landing_long_abstract.html")),
		// citation_pdf_url requires a login.
		"publisher.example.org/doi/pdf/": {Status: 403, Body: []byte("<html>Forbidden</html>")},
	}}
	svc := NewService(fetcher, config.Config{})
	content, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://publisher.example.org/article/1"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if content.ContentStatus != StatusAbstractOnly {
		t.Fatalf("status = %s, want abstract_only", content.ContentStatus)
	}
	if !strings.Contains(content.Markdown, "> Content status: abstract_only") {
		t.Fatalf("header does not flag the status: %q", content.Markdown[:300])
	}
	if !fetcher.called("doi/pdf/10.1000/landing") {
		t.Fatal("citation_pdf_url was not inspected even though the page was long")
	}
	if len(content.Warnings) == 0 || len(content.Attempts) < 2 {
		t.Fatalf("warnings = %v attempts = %+v", content.Warnings, content.Attempts)
	}
}

func TestLoginAndChallengePagesAreErrors(t *testing.T) {
	for _, c := range []struct {
		fixture string
		code    string
	}{
		{"login_wall.html", scholar.CodeAccessRestricted},
		{"challenge.html", scholar.CodeBlocked},
	} {
		fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{"example.org/p": htmlDoc(loadFixture(t, c.fixture))}}
		content, toolErr := NewService(fetcher, config.Config{}).GetPaperContent(context.Background(), Request{URL: "https://example.org/p"}, 0, 0)
		if content != nil || toolErr == nil || toolErr.Code != c.code {
			t.Errorf("%s: content=%v toolErr=%+v, want %s", c.fixture, content != nil, toolErr, c.code)
		}
	}
}

func TestFailedArxivCandidatesFallBackToRepository(t *testing.T) {
	requirePdftotext(t)
	fetcher := &fakeFetcher{
		responses: map[string]*scholar.FetchedDoc{
			"api.openalex.org/works/":           {Status: 200, Body: loadFixture(t, "openalex_work_arxiv_and_repo.json")},
			"arxiv.org/pdf/2001.00001":          {Status: 503, Body: []byte("busy")},
			"repository.example.edu/bitstream/": pdfDoc(fullPaperPDF(t)),
		},
		errs: map[string]error{"ar5iv.labs.arxiv.org": errors.New("connection reset")},
	}
	content, toolErr := NewService(fetcher, config.Config{}).GetPaperContent(context.Background(), Request{DOI: "10.1000/robust"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if !strings.Contains(content.SourceURL, "repository.example.edu") || content.SourceVersion != "acceptedVersion" || content.ContentStatus != StatusFullText {
		t.Fatalf("content = %s %s %s", content.SourceURL, content.SourceVersion, content.ContentStatus)
	}
	if content.Identity == nil || content.Identity.DOI != "10.1000/robust" || content.DocumentID != "doi:10.1000/robust" {
		t.Fatalf("identity = %+v id = %s", content.Identity, content.DocumentID)
	}
	if n := len(content.Attempts); n < 5 {
		t.Fatalf("attempt history too short: %+v", content.Attempts)
	}
}

func TestOpenAlexOutageStillConsultsUnpaywall(t *testing.T) {
	requirePdftotext(t)
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 500, Body: []byte("internal error")},
		"api.unpaywall.org/v2/":   {Status: 200, Body: []byte(`{"best_oa_location":{"url_for_pdf":"https://oa.example.org/robust.pdf","version":"publishedVersion"},"oa_locations":[]}`)},
		"oa.example.org/robust":   pdfDoc(fullPaperPDF(t)),
	}}
	content, toolErr := NewService(fetcher, config.Config{ContactEmail: "me@example.org"}).GetPaperContent(context.Background(), Request{DOI: "10.1000/robust"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if content.SourceProvider != "unpaywall" || content.ContentStatus != StatusFullText {
		t.Fatalf("content = %s %s", content.SourceProvider, content.ContentStatus)
	}
	for _, a := range content.Attempts {
		if strings.Contains(a.URL, "me@example.org") {
			t.Fatalf("attempt leaks contact email: %+v", a)
		}
	}
	if fetcher.calledPrefix("https://doi.org/") {
		t.Fatal("DOI landing page tried before Unpaywall's PDF")
	}
}

func (f *fakeFetcher) calledPrefix(prefix string) bool {
	for _, c := range f.callsSnapshot() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestUnpaywallConsultedAfterDiscoveredCandidatesFail(t *testing.T) {
	requirePdftotext(t)
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work.json")},
		"api.unpaywall.org/v2/":   {Status: 200, Body: []byte(`{"oa_locations":[{"url_for_pdf":"https://repo2.example.org/x.pdf","version":"acceptedVersion"}]}`)},
		"repo2.example.org/x.pdf": pdfDoc(fullPaperPDF(t)),
	}}
	content, toolErr := NewService(fetcher, config.Config{ContactEmail: "me@example.org"}).GetPaperContent(context.Background(), Request{DOI: "10.1109/cvpr.2016.90"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if content.SourceURL != "https://repo2.example.org/x.pdf" {
		t.Fatalf("source = %s; attempts %+v", content.SourceURL, content.Attempts)
	}
}

func TestMostInformativeFailureIsReturned(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/":        {Status: 200, Body: loadFixture(t, "openalex_work.json")},
		"publisher.example.org/article/": htmlDoc(loadFixture(t, "login_wall.html")),
	}}
	_, toolErr := NewService(fetcher, config.Config{}).GetPaperContent(context.Background(), Request{DOI: "10.1109/cvpr.2016.90"}, 0, 0)
	if toolErr == nil || toolErr.Code != scholar.CodeAccessRestricted {
		t.Fatalf("toolErr = %+v, want access_restricted over 404s", toolErr)
	}
	if len(toolErr.Attempts) == 0 || len(toolErr.Attempts) > scholar.MaxAttempts {
		t.Fatalf("attempts = %d", len(toolErr.Attempts))
	}
}

func TestMetadataAbstractIsLastResort(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work_arxiv_and_repo.json")},
	}}
	content, toolErr := NewService(fetcher, config.Config{}).GetPaperContent(context.Background(), Request{DOI: "10.1000/robust"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if content.ContentStatus != StatusAbstractOnly || content.SourceType != "metadata" || !strings.Contains(content.Markdown, "Robust abstract.") {
		t.Fatalf("content = %s %s %q", content.ContentStatus, content.SourceType, content.Markdown)
	}
}

func TestCancellationDuringFetch(t *testing.T) {
	fetcher := &fakeFetcher{delay: time.Second, responses: map[string]*scholar.FetchedDoc{"example.org/p": htmlDoc(loadFixture(t, "paper_full.html"))}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, toolErr := NewService(fetcher, config.Config{}).GetPaperContent(ctx, Request{URL: "https://example.org/p"}, 0, 0)
	if toolErr == nil || toolErr.Code != scholar.CodeTimeout {
		t.Fatalf("toolErr = %+v, want timeout", toolErr)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("cancellation took %v", time.Since(start))
	}
}

func TestPdftotextCancellationDoesNotStartGoExtraction(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow-pdftotext")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := convertPDF(ctx, config.Config{PdftotextPath: script}, makeTestPDF(t, testPDFLines), 0)
	if !errors.Is(err, context.DeadlineExceeded) || out != nil {
		t.Fatalf("convertPDF = %+v, %v; want deadline error and no go-pdf result", out, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("pdftotext not killed on cancellation (%v)", time.Since(start))
	}

	// A pdftotext failure that is not a cancellation still falls back.
	failing := filepath.Join(dir, "broken-pdftotext")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = convertPDF(context.Background(), config.Config{PdftotextPath: failing}, makeTestPDF(t, testPDFLines), 0)
	if err != nil || out.Converter != converterGoPDF {
		t.Fatalf("fallback = %+v, %v", out, err)
	}
}

func TestConcurrentIdenticalRequestsShareOneFetch(t *testing.T) {
	fetcher := &fakeFetcher{delay: 100 * time.Millisecond, responses: map[string]*scholar.FetchedDoc{"example.org/p": htmlDoc(loadFixture(t, "paper_full.html"))}}
	svc := NewService(fetcher, config.Config{})
	var wg sync.WaitGroup
	errs := make(chan *scholar.ToolError, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/p"}, 1000, 0)
			errs <- toolErr
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("toolErr = %+v", e)
		}
	}
	if n := len(fetcher.callsSnapshot()); n != 1 {
		t.Fatalf("fetches = %d, want 1", n)
	}
}

func TestOneCancelledWaiterDoesNotCancelOthers(t *testing.T) {
	fetcher := &fakeFetcher{delay: 150 * time.Millisecond, responses: map[string]*scholar.FetchedDoc{"example.org/p": htmlDoc(loadFixture(t, "paper_full.html"))}}
	svc := NewService(fetcher, config.Config{})
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan *scholar.ToolError, 1)
	go func() {
		_, e := svc.GetPaperContent(short, Request{URL: "https://example.org/p"}, 0, 0)
		done <- e
	}()
	time.Sleep(5 * time.Millisecond)
	if _, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/p"}, 0, 0); toolErr != nil {
		t.Fatalf("second waiter failed: %+v", toolErr)
	}
	if e := <-done; e == nil || e.Code != scholar.CodeTimeout {
		t.Fatalf("first waiter = %+v, want timeout", e)
	}
}

func TestExtractionAndCacheAreBounded(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{"example.org/": htmlDoc(loadFixture(t, "paper_full.html"))}}
	svc := NewService(fetcher, config.Config{MaxExtractChars: 1500, CacheMaxBytes: 3 * 4096})
	content, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/a"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if !strings.Contains(strings.Join(content.Warnings, "|"), "character limit") {
		t.Fatalf("warnings = %v", content.Warnings)
	}
	for _, p := range []string{"b", "c", "d", "e"} {
		if _, toolErr := svc.GetPaperContent(context.Background(), Request{URL: "https://example.org/" + p}, 0, 0); toolErr != nil {
			t.Fatal(toolErr)
		}
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.cacheBytes > 3*4096 || len(svc.cache) >= 5 {
		t.Fatalf("cache not bounded: %d bytes, %d entries", svc.cacheBytes, len(svc.cache))
	}
}

func TestPaginationReportsPages(t *testing.T) {
	doc := &extractedDoc{Status: StatusFullText}
	assemblePDF(doc, []string{"## 1 Introduction\n" + strings.Repeat("a", 100), strings.Repeat("b", 100), "## References\n" + strings.Repeat("c", 100)})
	doc = (&Service{}).finalize(doc, &resolution{})
	page, toolErr := paginate(doc, 150, doc.PageStarts[1]-5)
	if toolErr != nil {
		t.Fatal(toolErr)
	}
	if page.Provenance.StartPage != 1 || page.Provenance.EndPage != 2 || page.Provenance.TotalPages != 3 {
		t.Fatalf("provenance = %+v", page.Provenance)
	}
	if page.Provenance.SectionAtOffset != "1 Introduction" {
		t.Fatalf("section = %q", page.Provenance.SectionAtOffset)
	}
}
