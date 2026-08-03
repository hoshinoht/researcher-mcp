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
}

var pdfMagic = []byte("%PDF-")

func fetchDocument(ctx context.Context, fetcher DocFetcher, rawURL string) (*document, *scholar.ToolError) {
	doc, err := fetcher.GetDocument(ctx, rawURL)
	if err != nil {
		if errors.Is(err, scholar.ErrBodyTooLarge) {
			return nil, &scholar.ToolError{
				Code:    "upstream_error",
				Message: fmt.Sprintf("document at %s exceeds the size limit", hostOf(rawURL)),
				Hint:    "Raise SCHOLAR_MAX_FETCH_MB to fetch larger documents.",
			}
		}
		return nil, &scholar.ToolError{Code: "upstream_error", Message: fmt.Sprintf("request to %s failed: %v", hostOf(rawURL), err)}
	}

	switch doc.Status {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return nil, &scholar.ToolError{
			Code:    "blocked",
			Message: fmt.Sprintf("%s returned status %d", hostOf(rawURL), doc.Status),
			Hint:    "Increase SCHOLAR_MIN_DELAY and SCHOLAR_MAX_DELAY, reduce request volume, or configure SCHOLAR_PROXY_LIST.",
		}
	default:
		return nil, &scholar.ToolError{Code: "upstream_error", Message: fmt.Sprintf("%s returned status %d", hostOf(rawURL), doc.Status)}
	}

	kind, ok := sniffKind(doc.Body, doc.ContentType)
	if !ok {
		return nil, &scholar.ToolError{
			Code:    "parse_failed",
			Message: fmt.Sprintf("unsupported content type %q from %s", doc.ContentType, hostOf(rawURL)),
		}
	}

	finalURL := doc.FinalURL
	if finalURL == "" {
		finalURL = rawURL
	}
	return &document{Kind: kind, Body: doc.Body, FinalURL: finalURL}, nil
}

// sniffKind trusts the %PDF- magic bytes over any header; otherwise falls
// back to the Content-Type header and then http.DetectContentType.
func sniffKind(body []byte, contentType string) (string, bool) {
	if bytes.HasPrefix(body, pdfMagic) {
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

// citationPDFURL extracts <meta name="citation_pdf_url"> from a landing page,
// resolving relative URLs against the page URL.
func citationPDFURL(body []byte, pageURL string) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	content, ok := doc.Find(`meta[name="citation_pdf_url"]`).First().Attr("content")
	if !ok || strings.TrimSpace(content) == "" {
		return ""
	}
	content = strings.TrimSpace(content)

	base, err := url.Parse(pageURL)
	if err != nil {
		return content
	}
	ref, err := url.Parse(content)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}
