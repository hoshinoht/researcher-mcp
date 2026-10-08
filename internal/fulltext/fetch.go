package fulltext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"googlescholar-mcp-go/internal/scholar"

	"github.com/PuerkitoBio/goquery"
)

// DocFetcher is the narrow slice of *scholar.Requester this package needs.
type DocFetcher interface {
	GetDocument(ctx context.Context, rawURL string) (*scholar.FetchedDoc, error)
}

const (
	docKindPDF  = "pdf"
	docKindHTML = "html"
)

type document struct {
	Kind     string
	Body     []byte
	FinalURL string
	Status   int
}

var pdfMagic = []byte("%PDF-")

func fetchDocument(ctx context.Context, fetcher DocFetcher, rawURL string) (*document, *scholar.ToolError) {
	doc, err := fetcher.GetDocument(ctx, rawURL)
	if err != nil {
		if ctxErr := scholar.ContextError(err, "fetching "+hostOf(rawURL)); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, scholar.ErrBodyTooLarge) {
			return nil, &scholar.ToolError{
				Code:    scholar.CodeUpstreamError,
				Message: fmt.Sprintf("document at %s exceeds the size limit", hostOf(rawURL)),
				Hint:    "Raise SCHOLAR_MAX_FETCH_MB to fetch larger documents.",
			}
		}
		return nil, &scholar.ToolError{Code: scholar.CodeUpstreamError, Message: fmt.Sprintf("request to %s failed: %s", hostOf(rawURL), scholar.RedactText(err.Error())), Retryable: true}
	}

	switch doc.Status {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		toolErr := &scholar.ToolError{
			Code:      scholar.CodeBlocked,
			Message:   fmt.Sprintf("%s returned status %d", hostOf(rawURL), doc.Status),
			Hint:      "The host is rate limiting; retry later or reduce request volume.",
			Retryable: true,
		}
		if doc.RetryAfter > 0 {
			toolErr.RetryAfterSeconds = int(doc.RetryAfter.Seconds() + 0.5)
		}
		return nil, toolErr
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		if detectChallenge(doc.Body) {
			return nil, &scholar.ToolError{Code: scholar.CodeBlocked, Message: fmt.Sprintf("%s served a bot-check challenge (status %d)", hostOf(rawURL), doc.Status), Retryable: true}
		}
		return nil, &scholar.ToolError{
			Code:    scholar.CodeAccessRestricted,
			Message: fmt.Sprintf("%s denied access (status %d)", hostOf(rawURL), doc.Status),
			Hint:    "The document is likely paywalled or requires a login; provide an open-access URL if you have one.",
		}
	case http.StatusNotFound, http.StatusGone:
		return nil, &scholar.ToolError{Code: scholar.CodeNoResults, Message: fmt.Sprintf("%s returned status %d", hostOf(rawURL), doc.Status)}
	default:
		return nil, &scholar.ToolError{Code: scholar.CodeUpstreamError, Message: fmt.Sprintf("%s returned status %d", hostOf(rawURL), doc.Status), Retryable: doc.Status >= 500}
	}

	kind, ok := sniffKind(doc.Body, doc.ContentType)
	if !ok {
		return nil, &scholar.ToolError{
			Code:    scholar.CodeParseFailed,
			Message: fmt.Sprintf("unsupported content type %q from %s", doc.ContentType, hostOf(rawURL)),
		}
	}

	finalURL := doc.FinalURL
	if finalURL == "" {
		finalURL = rawURL
	}
	return &document{Kind: kind, Body: doc.Body, FinalURL: finalURL, Status: doc.Status}, nil
}

// sniffKind trusts the %PDF- magic bytes over any header; otherwise falls
// back to the Content-Type header and then http.DetectContentType.
func sniffKind(body []byte, contentType string) (string, bool) {
	if bytes.HasPrefix(bytes.TrimLeft(body, "\r\n\t "), pdfMagic) {
		return docKindPDF, true
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		return docKindHTML, true
	}
	if strings.Contains(strings.ToLower(http.DetectContentType(body)), "text/html") {
		return docKindHTML, true
	}
	return "", false
}

// embeddedLink is full-text evidence found on a page.
type embeddedLink struct {
	URL  string
	Kind string
	Via  string
}

// embeddedFullTextLinks finds machine-readable full-text pointers on a page:
// Highwire/bepress/eprints PDF meta tags, Highwire full-text HTML meta, and
// <link rel="alternate" type="application/pdf">. Relative URLs are resolved
// against the full page URL.
func embeddedFullTextLinks(body []byte, pageURL string) []embeddedLink {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	base := documentBaseURL(doc, pageURL)
	resolve := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return ""
		}
		ref, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		if base != nil {
			ref = base.ResolveReference(ref)
		}
		if ref.Scheme != "http" && ref.Scheme != "https" {
			return ""
		}
		return ref.String()
	}

	out := []embeddedLink{}
	add := func(raw, kind, via string) {
		if u := resolve(raw); u != "" {
			out = append(out, embeddedLink{URL: u, Kind: kind, Via: via})
		}
	}
	for _, name := range []string{"citation_pdf_url", "bepress_citation_pdf_url"} {
		doc.Find(`meta[name="` + name + `"]`).Each(func(_ int, s *goquery.Selection) {
			v, _ := s.Attr("content")
			add(v, docKindPDF, name)
		})
	}
	doc.Find(`meta[name="eprints.document_url"]`).Each(func(_ int, s *goquery.Selection) {
		if v, _ := s.Attr("content"); strings.HasSuffix(strings.ToLower(strings.TrimSpace(v)), ".pdf") {
			add(v, docKindPDF, "eprints.document_url")
		}
	})
	doc.Find(`link[rel="alternate"][type="application/pdf"]`).Each(func(_ int, s *goquery.Selection) {
		v, _ := s.Attr("href")
		add(v, docKindPDF, "link rel=alternate")
	})
	doc.Find(`meta[name="citation_fulltext_html_url"], meta[name="citation_full_html_url"]`).Each(func(_ int, s *goquery.Selection) {
		v, _ := s.Attr("content")
		add(v, docKindHTML, "citation_fulltext_html_url")
	})
	return out
}

// citationPDFURL returns the first citation_pdf_url-style link on a page.
func citationPDFURL(body []byte, pageURL string) string {
	for _, l := range embeddedFullTextLinks(body, pageURL) {
		if l.Kind == docKindPDF {
			return l.URL
		}
	}
	return ""
}
