package fulltext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"googlescholar-mcp-go/internal/scholar"
)

// Request identifies a paper to fetch. Precedence: URL > ArxivID > DOI > Title.
type Request struct {
	URL     string
	DOI     string
	ArxivID string
	Title   string
}

// resolution is the outcome of Stage 1: an ordered list of candidate
// full-text URLs plus the best-known title for the markdown header.
type resolution struct {
	Title      string
	Candidates []string
}

var (
	arxivNewIDPattern = regexp.MustCompile(`^\d{4}\.\d{4,5}(v\d+)?$`)
	arxivOldIDPattern = regexp.MustCompile(`^[a-z-]+(\.[A-Z]{2})?/\d{7}(v\d+)?$`)
	doiPattern        = regexp.MustCompile(`^10\.\d{4,9}/\S+$`)
)

func resolve(ctx context.Context, fetcher DocFetcher, contactEmail string, req Request) (*resolution, *scholar.ToolError) {
	switch {
	case strings.TrimSpace(req.URL) != "":
		return resolveURL(ctx, fetcher, contactEmail, strings.TrimSpace(req.URL))
	case strings.TrimSpace(req.ArxivID) != "":
		id, ok := normalizeArxivID(req.ArxivID)
		if !ok {
			return nil, &scholar.ToolError{
				Code:    "invalid_input",
				Message: fmt.Sprintf("invalid arXiv identifier: %q", req.ArxivID),
				Hint:    "Expected forms like 1706.03762, 2401.00001v2, or cs/9901001.",
			}
		}
		return &resolution{Candidates: arxivCandidates(id)}, nil
	case strings.TrimSpace(req.DOI) != "":
		doi, ok := normalizeDOI(req.DOI)
		if !ok {
			return nil, &scholar.ToolError{
				Code:    "invalid_input",
				Message: fmt.Sprintf("invalid DOI: %q", req.DOI),
				Hint:    "Expected a DOI like 10.1038/nature14539 (with or without a https://doi.org/ prefix).",
			}
		}
		return resolveDOI(ctx, fetcher, contactEmail, doi)
	case strings.TrimSpace(req.Title) != "":
		return resolveTitle(ctx, fetcher, contactEmail, strings.TrimSpace(req.Title))
	default:
		return nil, &scholar.ToolError{
			Code:    "invalid_input",
			Message: "at least one of url, doi, arxiv_id, or title is required",
		}
	}
}

func resolveURL(ctx context.Context, fetcher DocFetcher, contactEmail, rawURL string) (*resolution, *scholar.ToolError) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, &scholar.ToolError{
			Code:    "invalid_input",
			Message: fmt.Sprintf("invalid URL: %q", rawURL),
			Hint:    "Provide an absolute http(s) URL.",
		}
	}

	host := strings.ToLower(u.Hostname())
	switch {
	case host == "doi.org" || host == "dx.doi.org":
		doi, ok := normalizeDOI(strings.TrimPrefix(u.Path, "/"))
		if !ok {
			return nil, &scholar.ToolError{Code: "invalid_input", Message: fmt.Sprintf("URL does not contain a valid DOI: %q", rawURL)}
		}
		return resolveDOI(ctx, fetcher, contactEmail, doi)
	case isArxivHost(host):
		if id, ok := arxivIDFromURLPath(u.Path); ok {
			return &resolution{Candidates: arxivCandidates(id)}, nil
		}
		return &resolution{Candidates: []string{rawURL}}, nil
	default:
		return &resolution{Candidates: []string{rawURL}}, nil
	}
}

func isArxivHost(host string) bool {
	return host == "arxiv.org" || host == "www.arxiv.org" || host == "export.arxiv.org" || host == "ar5iv.labs.arxiv.org" || host == "ar5iv.org"
}

// arxivIDFromURLPath extracts an arXiv ID from paths like /abs/1706.03762,
// /pdf/1706.03762v5.pdf, or /html/2401.00001.
func arxivIDFromURLPath(path string) (string, bool) {
	path = strings.Trim(path, "/")
	for _, prefix := range []string{"abs/", "pdf/", "html/"} {
		if strings.HasPrefix(path, prefix) {
			id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".pdf")
			return normalizeArxivID(id)
		}
	}
	return "", false
}

func normalizeArxivID(raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	id = strings.TrimPrefix(id, "arXiv:")
	id = strings.TrimPrefix(id, "arxiv:")
	if arxivNewIDPattern.MatchString(id) || arxivOldIDPattern.MatchString(id) {
		return id, true
	}
	return "", false
}

// arxivCandidates orders sources HTML-first: native arXiv HTML, ar5iv
// rendering, then the PDF.
func arxivCandidates(id string) []string {
	return []string{
		"https://arxiv.org/html/" + id,
		"https://ar5iv.labs.arxiv.org/html/" + id,
		"https://arxiv.org/pdf/" + id,
	}
}

func normalizeDOI(raw string) (string, bool) {
	doi := strings.TrimSpace(raw)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:"} {
		doi = strings.TrimPrefix(doi, prefix)
	}
	if doiPattern.MatchString(doi) {
		return doi, true
	}
	return "", false
}

type openAlexLocation struct {
	LandingPageURL string `json:"landing_page_url"`
	PDFURL         string `json:"pdf_url"`
}

type openAlexWork struct {
	DisplayName     string             `json:"display_name"`
	DOI             string             `json:"doi"`
	PrimaryLocation openAlexLocation   `json:"primary_location"`
	BestOALocation  *openAlexLocation  `json:"best_oa_location"`
	Locations       []openAlexLocation `json:"locations"`
	OpenAccess      struct {
		OAURL string `json:"oa_url"`
	} `json:"open_access"`
}

func resolveDOI(ctx context.Context, fetcher DocFetcher, contactEmail, doi string) (*resolution, *scholar.ToolError) {
	apiURL := "https://api.openalex.org/works/https://doi.org/" + url.PathEscape(doi)
	if contactEmail != "" {
		apiURL += "?mailto=" + url.QueryEscape(contactEmail)
	}

	var work openAlexWork
	status, jsonErr := fetchJSON(ctx, fetcher, apiURL, &work)
	if jsonErr != nil && status != http.StatusNotFound {
		return nil, jsonErr
	}

	res := &resolution{Title: work.DisplayName}
	if jsonErr == nil {
		res.Candidates = candidatesFromWork(work)
	}

	if len(res.Candidates) == 0 && contactEmail != "" {
		if upCandidates := unpaywallCandidates(ctx, fetcher, contactEmail, doi); len(upCandidates) > 0 {
			res.Candidates = upCandidates
		}
	}

	if len(res.Candidates) == 0 {
		return nil, &scholar.ToolError{
			Code:    "no_results",
			Message: fmt.Sprintf("no open-access full-text location found for DOI %s", doi),
			Hint:    "The paper may be paywalled. Provide a direct PDF URL if you have access.",
		}
	}

	return res, nil
}

func resolveTitle(ctx context.Context, fetcher DocFetcher, contactEmail, title string) (*resolution, *scholar.ToolError) {
	params := url.Values{}
	params.Set("search", title)
	params.Set("per-page", "1")
	if contactEmail != "" {
		params.Set("mailto", contactEmail)
	}
	apiURL := "https://api.openalex.org/works?" + params.Encode()

	var resp struct {
		Results []openAlexWork `json:"results"`
	}
	if _, jsonErr := fetchJSON(ctx, fetcher, apiURL, &resp); jsonErr != nil {
		return nil, jsonErr
	}
	if len(resp.Results) == 0 {
		return nil, &scholar.ToolError{
			Code:    "no_results",
			Message: fmt.Sprintf("no paper found matching title %q", title),
			Hint:    "Try a DOI, arXiv ID, or direct URL instead.",
		}
	}

	work := resp.Results[0]
	res := &resolution{Title: work.DisplayName, Candidates: candidatesFromWork(work)}
	if len(res.Candidates) == 0 {
		if doi, ok := normalizeDOI(work.DOI); ok && contactEmail != "" {
			res.Candidates = unpaywallCandidates(ctx, fetcher, contactEmail, doi)
		}
	}
	if len(res.Candidates) == 0 {
		return nil, &scholar.ToolError{
			Code:    "no_results",
			Message: fmt.Sprintf("found %q but no open-access full-text location", work.DisplayName),
			Hint:    "The paper may be paywalled. Provide a direct PDF URL if you have access.",
		}
	}
	return res, nil
}

// candidatesFromWork orders full-text candidates from an OpenAlex work. If
// any location points at arXiv, the arXiv HTML-first chain wins outright.
func candidatesFromWork(work openAlexWork) []string {
	locations := make([]openAlexLocation, 0, len(work.Locations)+2)
	if work.BestOALocation != nil {
		locations = append(locations, *work.BestOALocation)
	}
	locations = append(locations, work.PrimaryLocation)
	locations = append(locations, work.Locations...)

	for _, loc := range locations {
		for _, candidate := range []string{loc.PDFURL, loc.LandingPageURL} {
			u, err := url.Parse(candidate)
			if err != nil || u == nil {
				continue
			}
			if isArxivHost(strings.ToLower(u.Hostname())) {
				if id, ok := arxivIDFromURLPath(u.Path); ok {
					return arxivCandidates(id)
				}
			}
		}
	}

	ordered := make([]string, 0, 4)
	if work.BestOALocation != nil {
		ordered = append(ordered, work.BestOALocation.PDFURL)
	}
	ordered = append(ordered, work.OpenAccess.OAURL)
	if work.BestOALocation != nil {
		ordered = append(ordered, work.BestOALocation.LandingPageURL)
	}
	ordered = append(ordered, work.PrimaryLocation.PDFURL)

	return dedupeNonEmpty(ordered)
}

func unpaywallCandidates(ctx context.Context, fetcher DocFetcher, contactEmail, doi string) []string {
	apiURL := "https://api.unpaywall.org/v2/" + url.PathEscape(doi) + "?email=" + url.QueryEscape(contactEmail)

	var resp struct {
		BestOALocation *struct {
			URLForPDF string `json:"url_for_pdf"`
			URL       string `json:"url"`
		} `json:"best_oa_location"`
	}
	if _, jsonErr := fetchJSON(ctx, fetcher, apiURL, &resp); jsonErr != nil || resp.BestOALocation == nil {
		return nil
	}
	return dedupeNonEmpty([]string{resp.BestOALocation.URLForPDF, resp.BestOALocation.URL})
}

// fetchJSON GETs a JSON API endpoint. The HTTP status is returned alongside
// the error so callers can special-case 404 (unknown DOI).
func fetchJSON(ctx context.Context, fetcher DocFetcher, apiURL string, out any) (int, *scholar.ToolError) {
	doc, err := fetcher.GetDocument(ctx, apiURL)
	if err != nil {
		return 0, &scholar.ToolError{Code: "upstream_error", Message: fmt.Sprintf("request to %s failed: %v", hostOf(apiURL), err)}
	}
	if doc.Status != http.StatusOK {
		return doc.Status, &scholar.ToolError{Code: "upstream_error", Message: fmt.Sprintf("%s returned status %d", hostOf(apiURL), doc.Status)}
	}
	if err := json.Unmarshal(doc.Body, out); err != nil {
		return doc.Status, &scholar.ToolError{Code: "parse_failed", Message: fmt.Sprintf("%s response parse failed: %v", hostOf(apiURL), err)}
	}
	return doc.Status, nil
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

func dedupeNonEmpty(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
