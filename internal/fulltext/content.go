package fulltext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"

	"github.com/PuerkitoBio/goquery"
)

const (
	DefaultMaxChars = 40000
	MaxMaxChars     = 150000

	// maxFetches bounds how many candidate documents one request downloads.
	maxFetches = 8

	cacheMaxEntries   = 16
	cacheTTL          = 15 * time.Minute
	cachePartialTTL   = 2 * time.Minute
	defaultCacheBytes = 64 * 1024 * 1024
	// maxConcurrentExtractions bounds CPU/memory spent converting documents.
	maxConcurrentExtractions = 2
	defaultFlightTimeout     = 2 * time.Minute
)

// PaperContent is one page of a paper's markdown, as returned to the client.
type PaperContent struct {
	// DocumentID is a stable scholarly identifier (doi:, arxiv:, openalex:,
	// pmcid: or url:) for citing and caching.
	DocumentID string `json:"document_id"`
	// ContentID hashes the extracted text, so clients can tell whether two
	// calls returned the same extraction.
	ContentID      string            `json:"content_id"`
	ContentStatus  string            `json:"content_status"`
	Title          string            `json:"title,omitempty"`
	SourceURL      string            `json:"source_url"`
	SourceType     string            `json:"source_type"`
	SourceProvider string            `json:"source_provider,omitempty"`
	SourceVersion  string            `json:"source_version,omitempty"`
	Converter      string            `json:"converter"`
	Identity       *PaperIdentity    `json:"identity,omitempty"`
	Markdown       string            `json:"markdown"`
	TotalChars     int               `json:"total_chars"`
	Offset         int               `json:"offset"`
	Truncated      bool              `json:"truncated"`
	NextOffset     int               `json:"next_offset,omitempty"`
	Provenance     Provenance        `json:"provenance"`
	Warnings       []string          `json:"warnings,omitempty"`
	Limitations    []string          `json:"limitations,omitempty"`
	Attempts       []scholar.Attempt `json:"attempts,omitempty"`
}

// Provenance locates the returned window inside the source document.
type Provenance struct {
	StartPage       int          `json:"start_page,omitempty"`
	EndPage         int          `json:"end_page,omitempty"`
	TotalPages      int          `json:"total_pages,omitempty"`
	SectionAtOffset string       `json:"section_at_offset,omitempty"`
	Sections        []SectionRef `json:"sections,omitempty"`
}

// SectionRef is a heading and its character offset in the full markdown.
type SectionRef struct {
	Heading string `json:"heading"`
	Level   int    `json:"level"`
	Offset  int    `json:"offset"`
}

// extractedDoc is a fully converted paper held in the pagination cache.
type extractedDoc struct {
	DocumentID     string
	ContentID      string
	Status         string
	Title          string
	SourceURL      string
	SourceType     string
	SourceProvider string
	SourceVersion  string
	Converter      string
	Identity       *PaperIdentity
	Warnings       []string
	Limitations    []string
	Attempts       []scholar.Attempt

	Markdown   string
	TotalChars int
	Sections   []SectionRef
	// PageStarts[i] is the character offset where page i+1 begins (PDFs).
	PageStarts []int
}

type cacheEntry struct {
	doc     *extractedDoc
	expires time.Time
	size    int64
}

type flight struct {
	done    chan struct{}
	doc     *extractedDoc
	err     *scholar.ToolError
	waiters int
	cancel  context.CancelFunc
}

type Service struct {
	fetcher DocFetcher
	cfg     config.Config

	mu         sync.Mutex
	cache      map[string]*cacheEntry
	order      []string
	cacheBytes int64
	inflight   map[string]*flight

	extractSem chan struct{}
}

func NewService(fetcher DocFetcher, cfg config.Config) *Service {
	return &Service{
		fetcher:    fetcher,
		cfg:        cfg,
		cache:      make(map[string]*cacheEntry),
		inflight:   make(map[string]*flight),
		extractSem: make(chan struct{}, maxConcurrentExtractions),
	}
}

// GetPaperContent runs the full pipeline: resolve identity → try candidate
// locations → convert → classify → paginate. Results are cached so follow-up
// pages skip the network, and concurrent identical requests share one fetch.
func (s *Service) GetPaperContent(ctx context.Context, req Request, maxChars, offset int) (*PaperContent, *scholar.ToolError) {
	maxChars, offset = sanitizePagination(maxChars, offset)
	if req.empty() {
		return nil, invalid("at least one of url, doi, arxiv_id, openalex_id, or title is required", "")
	}
	n, toolErr := normalizeRequest(req)
	if toolErr != nil {
		return nil, toolErr
	}

	key := cacheKey(n)
	if doc := s.cacheGet(key); doc != nil {
		return paginate(doc, maxChars, offset)
	}
	doc, toolErr := s.load(ctx, key, req)
	if toolErr != nil {
		return nil, toolErr
	}
	return paginate(doc, maxChars, offset)
}

// load deduplicates concurrent identical extractions. The shared work runs
// on a context detached from any single caller and is cancelled once every
// waiter has gone.
func (s *Service) load(ctx context.Context, key string, req Request) (*extractedDoc, *scholar.ToolError) {
	s.mu.Lock()
	f := s.inflight[key]
	if f == nil {
		// Each waiter enforces its own deadline; the shared work is bounded
		// by the operation timeout and cancelled when no waiter remains.
		limit := s.cfg.OperationTimeout
		if limit <= 0 {
			limit = defaultFlightTimeout
		}
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		s.inflight[key] = f
		go func() {
			doc, err := s.extract(runCtx, req)
			s.mu.Lock()
			f.doc, f.err = doc, err
			delete(s.inflight, key)
			if err == nil {
				s.cachePutLocked(key, doc)
			}
			s.mu.Unlock()
			close(f.done)
			cancel()
		}()
	}
	f.waiters++
	s.mu.Unlock()

	select {
	case <-f.done:
		return f.doc, f.err
	case <-ctx.Done():
		s.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
		}
		s.mu.Unlock()
		return nil, scholar.ContextError(ctx.Err(), "paper retrieval")
	}
}

// extractOutcome is the result of trying one candidate.
type extractOutcome struct {
	doc        *extractedDoc
	discovered []candidate
	attempt    scholar.Attempt
	err        *scholar.ToolError
}

func (s *Service) extract(ctx context.Context, req Request) (*extractedDoc, *scholar.ToolError) {
	res, toolErr := resolve(ctx, s.fetcher, s.cfg.ContactEmail, req)
	if toolErr != nil {
		return nil, toolErr
	}

	queue := res.sortedCandidates()
	tried := map[string]bool{}
	attempts := res.Attempts
	unpaywallPending := res.unpaywallDOI != ""

	var (
		best    *extractedDoc
		bestErr *scholar.ToolError
		fetches int
	)
	enqueue := func(cands []candidate) {
		for _, c := range cands {
			if !tried[canonicalURLKey(c.URL)] {
				queue = append(queue, c)
			}
		}
		sort.SliceStable(queue, func(i, j int) bool { return queue[i].Priority > queue[j].Priority })
	}

	for ctx.Err() == nil {
		// Unpaywall is an independent source: consult it once the remaining
		// candidates are only landing pages, or when nothing is left.
		if unpaywallPending && (len(queue) == 0 || queue[0].Priority < unpaywallBeforeBelow) {
			unpaywallPending = false
			cands, attempt, err := unpaywallCandidates(ctx, s.fetcher, s.cfg.ContactEmail, res.unpaywallDOI)
			attempts = scholar.AppendAttempt(attempts, attempt)
			if err != nil && scholar.MoreInformative(err, bestErr) && !isContextErr(err) {
				bestErr = err
			}
			enqueue(cands)
			continue
		}
		if len(queue) == 0 || fetches >= maxFetches {
			break
		}
		c := queue[0]
		queue = queue[1:]
		key := canonicalURLKey(c.URL)
		if tried[key] {
			continue
		}
		tried[key] = true
		fetches++

		out := s.extractOne(ctx, res, c)
		attempts = scholar.AppendAttempt(attempts, out.attempt)
		if out.doc != nil {
			if out.doc.Status == StatusFullText {
				out.doc.Attempts = attempts
				return s.finalize(out.doc, res), nil
			}
			if best == nil || statusRank[out.doc.Status] > statusRank[best.Status] {
				best = out.doc
			}
		}
		if out.err != nil && !isContextErr(out.err) && scholar.MoreInformative(out.err, bestErr) {
			bestErr = out.err
		}
		enqueue(out.discovered)
	}

	if ctxErr := ctx.Err(); ctxErr != nil && best == nil {
		e := scholar.ContextError(ctxErr, "paper retrieval")
		e.Attempts = attempts
		return nil, e
	}
	if best != nil {
		best.Attempts = attempts
		best.Warnings = append(best.Warnings, fmt.Sprintf("no complete full text was found; returning %s content from %s", best.Status, hostOf(best.SourceURL)))
		if ctx.Err() != nil {
			best.Warnings = append(best.Warnings, "the operation deadline was reached before every source was tried")
		}
		return s.finalize(best, res), nil
	}
	if res.Abstract != "" {
		doc := s.metadataAbstract(res)
		doc.Attempts = attempts
		return s.finalize(doc, res), nil
	}
	if bestErr == nil {
		bestErr = &scholar.ToolError{Code: scholar.CodeNoResults, Message: "no full-text candidates could be retrieved"}
	}
	out := *bestErr
	out.Attempts = attempts
	if out.Hint == "" {
		out.Hint = "The paper may be paywalled. Provide a direct PDF URL if you have access."
	}
	return nil, &out
}

// extractOne fetches, converts and classifies a single candidate URL, and
// reports any full-text links the page points to.
func (s *Service) extractOne(ctx context.Context, res *resolution, c candidate) extractOutcome {
	out := extractOutcome{attempt: scholar.Attempt{Provider: c.Source, Stage: "fetch", URL: c.URL}}
	fail := func(stage string, err *scholar.ToolError) extractOutcome {
		out.err = err
		out.attempt.Stage = stage
		out.attempt.Outcome = err.Code
		out.attempt.Message = err.Message
		return out
	}

	doc, toolErr := fetchDocument(ctx, s.fetcher, c.URL)
	if toolErr != nil {
		return fail("fetch", toolErr)
	}
	out.attempt.Status = doc.Status
	if doc.FinalURL != c.URL {
		out.attempt.Message = "redirected to " + scholar.RedactURL(doc.FinalURL)
	}

	if err := s.acquire(ctx); err != nil {
		return fail("convert", scholar.ContextError(err, "document conversion"))
	}
	defer s.release()

	switch doc.Kind {
	case docKindPDF:
		text, err := convertPDF(ctx, s.cfg, doc.Body, s.maxExtractChars())
		if err != nil {
			if ctxErr := scholar.ContextError(ctx.Err(), "PDF extraction"); ctxErr != nil {
				return fail("convert", ctxErr)
			}
			return fail("convert", &scholar.ToolError{Code: scholar.CodeParseFailed, Message: fmt.Sprintf("failed to convert PDF from %s: %v", hostOf(c.URL), err), Hint: parseFailedHint(converterGoPDF)})
		}
		pages := make([]string, len(text.Pages))
		for i, p := range text.Pages {
			pages[i] = cleanExtractedText(p)
		}
		cls := classifyPDF(strings.Join(pages, "\n\n"), len(pages))
		out.attempt.Stage, out.attempt.Outcome = "classify", cls.Status
		if cls.Status == statusEmpty {
			return fail("classify", &scholar.ToolError{Code: scholar.CodeParseFailed, Message: fmt.Sprintf("PDF from %s has no extractable text (%s)", hostOf(c.URL), strings.Join(cls.Reasons, "; ")), Hint: parseFailedHint(text.Converter)})
		}
		ed := &extractedDoc{
			Status: cls.Status, SourceURL: doc.FinalURL, SourceType: docKindPDF, SourceProvider: c.Source, SourceVersion: c.Version,
			Converter: text.Converter, Title: res.Identity.Title,
			Limitations: []string{"PDF text extraction does not preserve equations, tables or figures; formulas and table cells may be garbled or out of order"},
		}
		if text.Converter == converterGoPDF {
			ed.Limitations = append(ed.Limitations, "pure-Go PDF extractor used; reading order on multi-column layouts may be wrong (install poppler's pdftotext for better results)")
		}
		if text.Truncated {
			ed.Warnings = append(ed.Warnings, fmt.Sprintf("extraction stopped at the %d character limit (RESEARCHER_MAX_EXTRACT_CHARS)", s.maxExtractChars()))
		}
		if cls.Status != StatusFullText {
			ed.Warnings = append(ed.Warnings, "classified as "+cls.Status+": "+strings.Join(cls.Reasons, "; "))
		}
		assemblePDF(ed, pages)
		out.doc = ed
		return out

	default:
		var discovered []candidate
		for _, l := range embeddedFullTextLinks(doc.Body, doc.FinalURL) {
			discovered = append(discovered, candidate{URL: l.URL, Source: "embedded:" + l.Via, Kind: l.Kind, Version: c.Version, Priority: c.Priority + 1})
		}
		out.discovered = discovered

		conv, err := convertHTMLDetailed(doc.Body, doc.FinalURL)
		if err != nil {
			return fail("convert", &scholar.ToolError{Code: scholar.CodeParseFailed, Message: fmt.Sprintf("failed to convert HTML from %s: %v", hostOf(c.URL), err)})
		}
		cls := classifyHTML(doc.Body, conv.Markdown)
		out.attempt.Stage, out.attempt.Outcome = "classify", cls.Status
		switch cls.Status {
		case statusChallenge:
			return fail("classify", &scholar.ToolError{Code: scholar.CodeBlocked, Message: fmt.Sprintf("%s served a bot-check / challenge page instead of the paper", hostOf(doc.FinalURL)), Retryable: true})
		case statusAccessRestricted:
			return fail("classify", &scholar.ToolError{Code: scholar.CodeAccessRestricted, Message: fmt.Sprintf("%s shows a login or purchase page instead of the paper", hostOf(doc.FinalURL)), Hint: "The paper is likely paywalled; provide an open-access URL if you have one."})
		case statusEmpty:
			return fail("classify", &scholar.ToolError{Code: scholar.CodeParseFailed, Message: fmt.Sprintf("extracted content from %s is too short — likely a landing page or failed extraction", hostOf(doc.FinalURL))})
		}

		markdown := conv.Markdown
		ed := &extractedDoc{
			Status: cls.Status, SourceURL: doc.FinalURL, SourceType: docKindHTML, SourceProvider: c.Source, SourceVersion: c.Version,
			Converter: "html-to-markdown", Title: firstNonEmpty(res.Identity.Title, pageTitle(doc.Body)), Limitations: conv.Limitations,
		}
		if max := s.maxExtractChars(); utf8.RuneCountInString(markdown) > max {
			markdown = string([]rune(markdown)[:max])
			ed.Warnings = append(ed.Warnings, fmt.Sprintf("extraction stopped at the %d character limit (RESEARCHER_MAX_EXTRACT_CHARS)", max))
		}
		if cls.Status != StatusFullText {
			ed.Warnings = append(ed.Warnings, "classified as "+cls.Status+": "+strings.Join(cls.Reasons, "; "))
		}
		assembleText(ed, markdown)
		out.doc = ed
		return out
	}
}

func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.extractSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) release() { <-s.extractSem }

func (s *Service) maxExtractChars() int {
	if s.cfg.MaxExtractChars > 0 {
		return s.cfg.MaxExtractChars
	}
	return 2_000_000
}

// metadataAbstract is the last-resort result: the abstract from metadata,
// explicitly labelled as such.
func (s *Service) metadataAbstract(res *resolution) *extractedDoc {
	ed := &extractedDoc{
		Status: StatusAbstractOnly, SourceType: "metadata", SourceProvider: "openalex", Converter: "openalex-abstract",
		Title:    res.Identity.Title,
		Warnings: []string{"no full-text source could be retrieved; returning only the abstract from OpenAlex metadata"},
	}
	if res.Identity.OpenAlexID != "" {
		ed.SourceURL = "https://openalex.org/" + res.Identity.OpenAlexID
	}
	assembleText(ed, "## Abstract\n\n"+res.Abstract)
	return ed
}

// finalize attaches identity, IDs, header and section/page offsets.
func (s *Service) finalize(doc *extractedDoc, res *resolution) *extractedDoc {
	identity := res.Identity
	doc.Identity = &identity
	doc.Warnings = append(append([]string{}, res.Warnings...), doc.Warnings...)
	doc.DocumentID = documentID(identity, doc.SourceURL)

	header := buildHeader(doc.Title, doc.SourceURL, doc.Status)
	shift := utf8.RuneCountInString(header)
	doc.Markdown = header + doc.Markdown
	doc.TotalChars += shift
	for i := range doc.PageStarts {
		doc.PageStarts[i] += shift
	}
	doc.Sections = sectionsOf(doc.Markdown)
	return doc
}

// assembleText sets the body markdown and its content hash.
func assembleText(doc *extractedDoc, markdown string) {
	doc.Markdown = markdown
	doc.TotalChars = utf8.RuneCountInString(markdown)
	doc.ContentID = contentHash(markdown)
}

// assemblePDF joins pages with visible page markers and records where each
// page starts.
func assemblePDF(doc *extractedDoc, pages []string) {
	b := strings.Builder{}
	starts := make([]int, 0, len(pages))
	offset := 0
	for i, p := range pages {
		marker := fmt.Sprintf("<!-- page %d -->\n\n", i+1)
		if i > 0 {
			marker = "\n\n" + marker
		}
		b.WriteString(marker)
		offset += utf8.RuneCountInString(marker)
		starts = append(starts, offset)
		b.WriteString(p)
		offset += utf8.RuneCountInString(p)
	}
	doc.Markdown = b.String()
	doc.TotalChars = offset
	doc.PageStarts = starts
	doc.ContentID = contentHash(doc.Markdown)
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func documentID(id PaperIdentity, sourceURL string) string {
	switch {
	case id.DOI != "":
		return "doi:" + id.DOI
	case id.ArxivID != "":
		return "arxiv:" + arxivVersion.ReplaceAllString(id.ArxivID, "")
	case id.OpenAlexID != "":
		return "openalex:" + id.OpenAlexID
	case id.PMCID != "":
		return "pmcid:" + id.PMCID
	default:
		return "url:" + canonicalURLKey(sourceURL)
	}
}

var headingLine = regexp.MustCompile(`(?m)^(#{1,6})[ \t]+(.+?)[ \t#]*$`)

// sectionsOf lists markdown headings with character offsets.
func sectionsOf(markdown string) []SectionRef {
	out := []SectionRef{}
	for _, m := range headingLine.FindAllStringSubmatchIndex(markdown, -1) {
		out = append(out, SectionRef{
			Heading: strings.TrimSpace(markdown[m[4]:m[5]]),
			Level:   m[3] - m[2],
			Offset:  utf8.RuneCountInString(markdown[:m[0]]),
		})
	}
	return out
}

func pageTitle(body []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	if v, ok := doc.Find(`meta[name="citation_title"]`).First().Attr("content"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.Join(strings.Fields(doc.Find("title").First().Text()), " ")
}

func parseFailedHint(converter string) string {
	if converter == converterGoPDF {
		return "Install poppler (pdftotext) for better PDF extraction, e.g. brew install poppler."
	}
	return ""
}

var statusNotes = map[string]string{
	StatusUnverified:   "substantial text, but completeness could not be verified",
	StatusPartial:      "partial text — this is not the complete paper",
	StatusAbstractOnly: "abstract only — the full text was not available",
	StatusLandingPage:  "landing page text only — the full text was not available",
}

func buildHeader(title, sourceURL, status string) string {
	b := strings.Builder{}
	if strings.TrimSpace(title) != "" {
		b.WriteString("# " + strings.TrimSpace(title) + "\n\n")
	}
	b.WriteString("> Source: " + sourceURL + " — retrieved " + time.Now().UTC().Format("2006-01-02") + "\n")
	if note, ok := statusNotes[status]; ok {
		b.WriteString("> Content status: " + status + " (" + note + ")\n")
	}
	b.WriteString("\n")
	return b.String()
}

func sanitizePagination(maxChars, offset int) (int, int) {
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	if maxChars > MaxMaxChars {
		maxChars = MaxMaxChars
	}
	if offset < 0 {
		offset = 0
	}
	return maxChars, offset
}

// byteOffset converts a rune offset into a byte offset in s.
func byteOffset(s string, runes int) int {
	if runes <= 0 {
		return 0
	}
	i := 0
	for pos := range s {
		if i == runes {
			return pos
		}
		i++
	}
	return len(s)
}

func paginate(doc *extractedDoc, maxChars, offset int) (*PaperContent, *scholar.ToolError) {
	total := doc.TotalChars
	if offset >= total && total > 0 {
		return nil, &scholar.ToolError{
			Code:    scholar.CodeInvalidInput,
			Message: fmt.Sprintf("offset %d is beyond the end of the document (total_chars=%d)", offset, total),
		}
	}

	end := offset + maxChars
	truncated := end < total
	startB := byteOffset(doc.Markdown, offset)
	var endB int
	if truncated {
		end, endB = preferParagraphBreak(doc.Markdown, offset, end, startB)
	} else {
		end, endB = total, len(doc.Markdown)
	}

	content := &PaperContent{
		DocumentID:     doc.DocumentID,
		ContentID:      doc.ContentID,
		ContentStatus:  doc.Status,
		Title:          doc.Title,
		SourceURL:      doc.SourceURL,
		SourceType:     doc.SourceType,
		SourceProvider: doc.SourceProvider,
		SourceVersion:  doc.SourceVersion,
		Converter:      doc.Converter,
		Identity:       doc.Identity,
		Markdown:       doc.Markdown[startB:endB],
		TotalChars:     total,
		Offset:         offset,
		Truncated:      truncated,
		Provenance:     provenanceFor(doc, offset, end),
		Warnings:       doc.Warnings,
		Limitations:    doc.Limitations,
	}
	// Attempt history is diagnostic; return it with the first page only.
	if offset == 0 && len(doc.Attempts) > 1 {
		content.Attempts = doc.Attempts
	}
	if truncated {
		content.NextOffset = end
	}
	return content, nil
}

func provenanceFor(doc *extractedDoc, start, end int) Provenance {
	p := Provenance{}
	if len(doc.PageStarts) > 0 {
		p.TotalPages = len(doc.PageStarts)
		pageAt := func(off int) int {
			return sort.Search(len(doc.PageStarts), func(i int) bool { return doc.PageStarts[i] > off })
		}
		p.StartPage = max(pageAt(start), 1)
		p.EndPage = max(pageAt(max(end-1, start)), 1)
	}
	for _, s := range doc.Sections {
		if s.Offset <= start {
			p.SectionAtOffset = s.Heading
		}
		if s.Offset >= start && s.Offset < end {
			p.Sections = append(p.Sections, s)
		}
	}
	return p
}

// preferParagraphBreak walks back from the hard limit looking for a blank
// line, so pages split between paragraphs rather than mid-sentence. Only the
// last 20% of the window is considered to avoid tiny pages. Returns the rune
// and byte offsets of the chosen end.
func preferParagraphBreak(s string, offset, end, startB int) (int, int) {
	floor := offset + (end-offset)*8/10
	floorB := startB + byteOffset(s[startB:], floor-offset)
	endB := floorB + byteOffset(s[floorB:], end-floor)
	if idx := strings.LastIndex(s[floorB:endB], "\n\n"); idx > 0 {
		cut := floorB + idx
		return floor + utf8.RuneCountInString(s[floorB:cut]), cut
	}
	return end, endB
}

func cacheKey(n *normalized) string {
	parts := []string{}
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("url", n.contentURL)
	add("doi", n.doi)
	add("arxiv", n.arxiv)
	add("openalex", n.openalex)
	add("title", strings.ToLower(strings.Join(strings.Fields(n.title), " ")))
	add("author", strings.ToLower(n.author))
	if n.year != 0 {
		add("year", strconv.Itoa(n.year))
	}
	return strings.Join(parts, "|")
}

func (s *Service) cacheGet(key string) *extractedDoc {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.cache[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expires) {
		s.removeLocked(key)
		return nil
	}
	s.removeFromOrder(key)
	s.order = append(s.order, key)
	return entry.doc
}

// cachePutLocked stores a document under LRU eviction bounded by entry
// count and total bytes. Non-full-text results expire sooner so a transient
// failure does not pin a partial result.
func (s *Service) cachePutLocked(key string, doc *extractedDoc) {
	limit := s.cfg.CacheMaxBytes
	if limit <= 0 {
		limit = defaultCacheBytes
	}
	size := int64(len(doc.Markdown)) + 4096
	if size > limit {
		return
	}
	if _, ok := s.cache[key]; ok {
		s.removeLocked(key)
	}
	ttl := cacheTTL
	if doc.Status != StatusFullText {
		ttl = cachePartialTTL
	}
	s.cache[key] = &cacheEntry{doc: doc, expires: time.Now().Add(ttl), size: size}
	s.cacheBytes += size
	s.order = append(s.order, key)
	for len(s.order) > cacheMaxEntries || s.cacheBytes > limit {
		s.removeLocked(s.order[0])
	}
}

func (s *Service) removeLocked(key string) {
	if entry, ok := s.cache[key]; ok {
		s.cacheBytes -= entry.size
		delete(s.cache, key)
	}
	s.removeFromOrder(key)
}

func (s *Service) removeFromOrder(key string) {
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
