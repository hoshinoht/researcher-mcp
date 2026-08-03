package fulltext

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"
)

const (
	DefaultMaxChars = 40000
	MaxMaxChars     = 150000

	// minContentChars guards against landing pages and failed extractions
	// masquerading as full text.
	minContentChars = 500

	maxCandidates = 6
	cacheCapacity = 8
	cacheTTL      = 15 * time.Minute
)

// PaperContent is one page of a paper's markdown, as returned to the client.
type PaperContent struct {
	Title      string `json:"title,omitempty"`
	SourceURL  string `json:"source_url"`
	SourceType string `json:"source_type"`
	Converter  string `json:"converter"`
	Markdown   string `json:"markdown"`
	TotalChars int    `json:"total_chars"`
	Offset     int    `json:"offset"`
	Truncated  bool   `json:"truncated"`
	NextOffset int    `json:"next_offset,omitempty"`
}

// extractedDoc is a fully converted paper held in the pagination cache.
type extractedDoc struct {
	Title      string
	SourceURL  string
	SourceType string
	Converter  string
	Markdown   []rune
}

type cacheEntry struct {
	doc     *extractedDoc
	expires time.Time
}

type Service struct {
	fetcher DocFetcher
	cfg     config.Config

	mu    sync.Mutex
	cache map[string]cacheEntry
	order []string
}

func NewService(fetcher DocFetcher, cfg config.Config) *Service {
	return &Service{
		fetcher: fetcher,
		cfg:     cfg,
		cache:   make(map[string]cacheEntry),
	}
}

// GetPaperContent runs the full pipeline: resolve → fetch → convert →
// paginate, with an in-memory cache so follow-up pages skip the network.
func (s *Service) GetPaperContent(ctx context.Context, req Request, maxChars, offset int) (*PaperContent, *scholar.ToolError) {
	maxChars, offset = sanitizePagination(maxChars, offset)

	key := cacheKey(req)
	if doc := s.cacheGet(key); doc != nil {
		return paginate(doc, maxChars, offset)
	}

	res, toolErr := resolve(ctx, s.fetcher, s.cfg.ContactEmail, req)
	if toolErr != nil {
		return nil, toolErr
	}

	doc, toolErr := s.extractFromCandidates(ctx, res)
	if toolErr != nil {
		return nil, toolErr
	}

	s.cachePut(key, doc)
	return paginate(doc, maxChars, offset)
}

func (s *Service) extractFromCandidates(ctx context.Context, res *resolution) (*extractedDoc, *scholar.ToolError) {
	candidates := res.Candidates
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	var lastErr *scholar.ToolError
	for _, candidate := range candidates {
		doc, toolErr := s.extractOne(ctx, res.Title, candidate, true)
		if toolErr != nil {
			lastErr = toolErr
			continue
		}
		return doc, nil
	}

	if lastErr == nil {
		lastErr = &scholar.ToolError{Code: "no_results", Message: "no full-text candidates could be resolved"}
	}
	return nil, lastErr
}

// extractOne fetches and converts a single candidate URL. When an HTML page
// yields too little text (a landing page), it follows the page's
// citation_pdf_url meta tag once before giving up on the candidate.
func (s *Service) extractOne(ctx context.Context, title, candidate string, followMeta bool) (*extractedDoc, *scholar.ToolError) {
	doc, toolErr := fetchDocument(ctx, s.fetcher, candidate)
	if toolErr != nil {
		return nil, toolErr
	}

	var (
		markdown  string
		converter string
		err       error
	)
	switch doc.Kind {
	case docKindPDF:
		markdown, converter, err = convertPDF(ctx, s.cfg, doc.Body)
	case docKindHTML:
		converter = "html-to-markdown"
		markdown, err = convertHTML(doc.Body, doc.FinalURL)
	}
	if err != nil {
		return nil, &scholar.ToolError{
			Code:    "parse_failed",
			Message: fmt.Sprintf("failed to convert %s: %v", hostOf(candidate), err),
			Hint:    parseFailedHint(converter),
		}
	}

	if len([]rune(markdown)) < minContentChars {
		if doc.Kind == docKindHTML && followMeta {
			if pdfURL := citationPDFURL(doc.Body, doc.FinalURL); pdfURL != "" && pdfURL != candidate {
				return s.extractOne(ctx, title, pdfURL, false)
			}
		}
		return nil, &scholar.ToolError{
			Code:    "parse_failed",
			Message: fmt.Sprintf("extracted content from %s is too short (%d chars) — likely a landing page or failed extraction", hostOf(candidate), len(markdown)),
			Hint:    parseFailedHint(converter),
		}
	}

	full := buildHeader(title, doc.FinalURL) + markdown
	return &extractedDoc{
		Title:      title,
		SourceURL:  doc.FinalURL,
		SourceType: doc.Kind,
		Converter:  converter,
		Markdown:   []rune(full),
	}, nil
}

func parseFailedHint(converter string) string {
	if converter == converterGoPDF {
		return "Install poppler (pdftotext) for better PDF extraction, e.g. brew install poppler."
	}
	return ""
}

func buildHeader(title, sourceURL string) string {
	b := strings.Builder{}
	if strings.TrimSpace(title) != "" {
		b.WriteString("# " + strings.TrimSpace(title) + "\n\n")
	}
	b.WriteString("> Source: " + sourceURL + " — retrieved " + time.Now().UTC().Format("2006-01-02") + "\n\n")
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

func paginate(doc *extractedDoc, maxChars, offset int) (*PaperContent, *scholar.ToolError) {
	total := len(doc.Markdown)
	if offset >= total && total > 0 {
		return nil, &scholar.ToolError{
			Code:    "invalid_input",
			Message: fmt.Sprintf("offset %d is beyond the end of the document (total_chars=%d)", offset, total),
		}
	}

	end := offset + maxChars
	truncated := end < total
	if truncated {
		end = preferParagraphBreak(doc.Markdown, offset, end)
	} else {
		end = total
	}

	content := &PaperContent{
		Title:      doc.Title,
		SourceURL:  doc.SourceURL,
		SourceType: doc.SourceType,
		Converter:  doc.Converter,
		Markdown:   string(doc.Markdown[offset:end]),
		TotalChars: total,
		Offset:     offset,
		Truncated:  truncated,
	}
	if truncated {
		content.NextOffset = end
	}
	return content, nil
}

// preferParagraphBreak walks back from the hard limit looking for a blank
// line, so pages split between paragraphs rather than mid-sentence. Only the
// last 20% of the window is considered to avoid tiny pages.
func preferParagraphBreak(runes []rune, offset, end int) int {
	floor := offset + (end-offset)*8/10
	for i := end - 1; i > floor; i-- {
		if runes[i] == '\n' && i+1 < len(runes) && runes[i+1] == '\n' {
			return i
		}
	}
	return end
}

func cacheKey(req Request) string {
	switch {
	case strings.TrimSpace(req.URL) != "":
		return "url:" + strings.TrimSpace(req.URL)
	case strings.TrimSpace(req.ArxivID) != "":
		if id, ok := normalizeArxivID(req.ArxivID); ok {
			return "arxiv:" + id
		}
		return "arxiv:" + strings.TrimSpace(req.ArxivID)
	case strings.TrimSpace(req.DOI) != "":
		if doi, ok := normalizeDOI(req.DOI); ok {
			return "doi:" + doi
		}
		return "doi:" + strings.TrimSpace(req.DOI)
	default:
		return "title:" + strings.ToLower(strings.Join(strings.Fields(req.Title), " "))
	}
}

func (s *Service) cacheGet(key string) *extractedDoc {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.cache[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expires) {
		delete(s.cache, key)
		s.removeFromOrder(key)
		return nil
	}
	// Refresh LRU position.
	s.removeFromOrder(key)
	s.order = append(s.order, key)
	return entry.doc
}

func (s *Service) cachePut(key string, doc *extractedDoc) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.cache[key]; ok {
		s.removeFromOrder(key)
	}
	s.cache[key] = cacheEntry{doc: doc, expires: time.Now().Add(cacheTTL)}
	s.order = append(s.order, key)

	for len(s.order) > cacheCapacity {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.cache, oldest)
	}
}

func (s *Service) removeFromOrder(key string) {
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}
