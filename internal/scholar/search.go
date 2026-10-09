package scholar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"googlescholar-mcp-go/internal/config"

	"github.com/PuerkitoBio/goquery"
)

// SearchRequest is an article search across the configured providers.
type SearchRequest struct {
	Query      string
	Author     string
	YearRange  []int
	NumResults int
}

// SearchOutcome carries results plus which provider produced them and what
// was tried before it.
type SearchOutcome struct {
	Results   []PaperResult
	Provider  string
	Providers []string
	Attempts  []Attempt
}

// SearchByKeywords searches with the requester's configured provider order.
func SearchByKeywords(ctx context.Context, requester *Requester, query string, numResults int) (*SearchOutcome, *ToolError) {
	return Search(ctx, requester, SearchRequest{Query: query, NumResults: numResults}, requester.cfg.SearchProviders)
}

// SearchAdvanced searches with author and year filters using the configured
// provider order.
func SearchAdvanced(ctx context.Context, requester *Requester, query, author string, yearRange []int, numResults int) (*SearchOutcome, *ToolError) {
	return Search(ctx, requester, SearchRequest{Query: query, Author: author, YearRange: yearRange, NumResults: numResults}, requester.cfg.SearchProviders)
}

// Search tries providers in order and returns the first usable result set.
// Recoverable failures (blocked, no results, upstream or parse errors) fall
// through to the next provider; the most informative failure is returned
// when every provider fails.
func Search(ctx context.Context, requester *Requester, req SearchRequest, providers []string) (*SearchOutcome, *ToolError) {
	if len(providers) == 0 {
		providers = config.DefaultSearchProviders(false)
	}
	if req.NumResults <= 0 {
		req.NumResults = 5
	}

	// A batch of quoted titles is an OpenAlex OR query; other providers would
	// treat it as one long phrase.
	if batchQuery, ok := quotedTitleBatchQuery(req.Query); ok && containsString(providers, config.ProviderOpenAlex) {
		req.Query = batchQuery
		providers = []string{config.ProviderOpenAlex}
	}

	outcome := &SearchOutcome{Providers: providers}
	var best *ToolError
	for _, provider := range providers {
		var (
			results []PaperResult
			toolErr *ToolError
		)
		switch provider {
		case config.ProviderScholar:
			results, toolErr = searchScholar(ctx, requester, buildSearchURL(req.Query, req.Author, req.YearRange), req.NumResults)
		case config.ProviderOpenAlex:
			results, toolErr = searchOpenAlex(ctx, requester, req.Query, req.Author, req.YearRange, req.NumResults)
		case config.ProviderCrossref:
			results, toolErr = searchCrossref(ctx, requester, req)
		default:
			continue
		}
		if toolErr == nil {
			outcome.Attempts = AppendAttempt(outcome.Attempts, Attempt{Provider: provider, Stage: "search", Outcome: "ok"})
			outcome.Results = results
			outcome.Provider = provider
			return outcome, nil
		}
		outcome.Attempts = AppendAttempt(outcome.Attempts, Attempt{Provider: provider, Stage: "search", Outcome: toolErr.Code, Message: toolErr.Message})
		if ctxErr := ContextError(ctx.Err(), "article search"); ctxErr != nil {
			ctxErr.Attempts = outcome.Attempts
			return nil, ctxErr
		}
		if MoreInformative(toolErr, best) {
			best = toolErr
		}
		if !shouldFallbackSearch(toolErr.Code) {
			break
		}
	}
	if best == nil {
		best = &ToolError{Code: CodeNoResults, Message: "no results found"}
	}
	out := *best
	out.Attempts = outcome.Attempts
	return nil, &out
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func searchScholar(ctx context.Context, requester *Requester, searchURL string, numResults int) ([]PaperResult, *ToolError) {
	if strings.TrimSpace(searchURL) == "" {
		return nil, &ToolError{Code: "invalid_input", Message: "search URL is empty"}
	}

	doc, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, requestError("google scholar", err)
	}
	body, status := doc.Body, doc.Status

	if status != http.StatusOK {
		if status == http.StatusForbidden || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			return nil, BuildBlockedError(status)
		}
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("request failed with status %d", status)}
	}

	results, parseErr := parseSearchResultsHTML(body, numResults)
	if parseErr != nil {
		return nil, &ToolError{Code: "parse_failed", Message: parseErr.Error()}
	}

	if len(results) == 0 {
		if looksLikeBlocked(body) {
			return nil, &ToolError{Code: "blocked", Message: "Google Scholar appears to have blocked this automated request", Retryable: true}
		}
		return nil, &ToolError{Code: "no_results", Message: "no results found"}
	}

	return results, nil
}

func shouldFallbackSearch(code string) bool {
	switch code {
	case CodeBlocked, CodeNoResults, CodeUpstreamError, CodeParseFailed, CodeTimeout:
		return true
	default:
		return false
	}
}

// requestError converts a transport error into a structured error.
func requestError(provider string, err error) *ToolError {
	if ctxErr := ContextError(err, provider+" request"); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrBodyTooLarge) {
		return &ToolError{Code: CodeUpstreamError, Message: provider + " response exceeded the size limit", Hint: "Raise RESEARCHER_MAX_RESPONSE_MB if this is expected."}
	}
	return &ToolError{Code: CodeUpstreamError, Message: fmt.Sprintf("%s request failed: %s", provider, RedactText(err.Error())), Retryable: true}
}

// statusError converts a non-200 API response into a structured error,
// passing on any Retry-After wait the requester could not honour.
func statusError(provider string, doc *FetchedDoc) *ToolError {
	switch status := doc.Status; status {
	case http.StatusTooManyRequests:
		e := &ToolError{Code: CodeBlocked, Message: fmt.Sprintf("%s rate limited the request (status 429)", provider), Hint: "Configure provider credentials (see README) or retry later.", Retryable: true}
		if doc.RetryAfter > 0 {
			e.RetryAfterSeconds = int(doc.RetryAfter.Seconds() + 0.5)
		}
		return e
	case http.StatusUnauthorized, http.StatusForbidden:
		return &ToolError{Code: CodeUpstreamError, Message: fmt.Sprintf("%s rejected the request (status %d)", provider, status), Hint: "Check the provider credential configured for this server."}
	}
	return &ToolError{Code: CodeUpstreamError, Message: fmt.Sprintf("%s request failed with status %d", provider, doc.Status), Retryable: doc.Status >= 500}
}

type openAlexWorksSearchResponse struct {
	Results []OpenAlexWork `json:"results"`
}

func searchOpenAlex(ctx context.Context, requester *Requester, query, author string, yearRange []int, numResults int) ([]PaperResult, *ToolError) {
	searchURL := buildOpenAlexSearchURL(query, author, yearRange, numResults)
	doc, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, requestError("openalex", err)
	}
	if doc.Status != http.StatusOK {
		return nil, statusError("openalex", doc)
	}
	body := doc.Body

	var resp openAlexWorksSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &ToolError{Code: "parse_failed", Message: fmt.Sprintf("openalex parse failed: %v", err)}
	}
	if len(resp.Results) == 0 {
		return nil, &ToolError{Code: "no_results", Message: "no results found"}
	}

	results := make([]PaperResult, 0, numResults)
	for _, item := range resp.Results {
		if len(results) >= numResults {
			break
		}

		title := item.DisplayTitle()
		if title == "" {
			title = "No title available"
		}

		authors := strings.Join(item.AuthorNames(), ", ")
		if authors == "" {
			authors = "No authors available"
		}

		abstract := openAlexAbstractToText(item.AbstractInvertedIndex)
		if abstract == "" {
			abstract = "No abstract available"
		}

		resultURL := openAlexResultURL(item)
		if resultURL == "" {
			resultURL = "No link available"
		}

		pdfURL := ""
		if item.BestOALocation != nil {
			pdfURL = strings.TrimSpace(item.BestOALocation.PDFURL)
		}
		if pdfURL == "" && item.PrimaryLocation != nil {
			pdfURL = strings.TrimSpace(item.PrimaryLocation.PDFURL)
		}

		openAlexID, _ := NormalizeOpenAlexID(item.ID)
		results = append(results, PaperResult{
			Title:            title,
			Authors:          authors,
			Abstract:         abstract,
			URL:              resultURL,
			DOI:              normalizeDOIString(item.DOI),
			Year:             item.PublicationYear,
			PDFURL:           pdfURL,
			SnippetTruncated: false,
			Source:           config.ProviderOpenAlex,
			OpenAlexID:       openAlexID,
		})
	}

	if len(results) == 0 {
		return nil, &ToolError{Code: "no_results", Message: "no results found"}
	}

	return results, nil
}

func buildOpenAlexSearchURL(query, author string, yearRange []int, numResults int) string {
	params := url.Values{}
	params.Set("search", strings.TrimSpace(query))
	params.Set("per-page", strconv.Itoa(numResults))
	params.Set("sort", "relevance_score:desc")

	filters := make([]string, 0, 3)
	if strings.TrimSpace(author) != "" {
		filters = append(filters, "authorships.author.display_name.search:"+strings.TrimSpace(author))
	}
	if len(yearRange) == 2 {
		start, end := yearRange[0], yearRange[1]
		if start > end {
			start, end = end, start
		}
		filters = append(filters,
			fmt.Sprintf("from_publication_date:%d-01-01", start),
			fmt.Sprintf("to_publication_date:%d-12-31", end),
		)
	}
	if len(filters) > 0 {
		params.Set("filter", strings.Join(filters, ","))
	}

	return "https://api.openalex.org/works?" + params.Encode()
}

func quotedTitleBatchQuery(query string) (string, bool) {
	remaining := strings.TrimSpace(query)
	phrases := make([]string, 0, 2)

	for remaining != "" {
		if !strings.HasPrefix(remaining, `"`) {
			return "", false
		}

		end := strings.IndexByte(remaining[1:], '"')
		if end < 0 {
			return "", false
		}
		end++

		phrase := remaining[:end+1]
		if strings.TrimSpace(phrase[1:len(phrase)-1]) == "" {
			return "", false
		}
		phrases = append(phrases, phrase)

		remaining = remaining[end+1:]
		if remaining == "" {
			break
		}

		next := strings.TrimLeftFunc(remaining, unicode.IsSpace)
		if len(next) == len(remaining) {
			return "", false
		}
		remaining = next
	}

	if len(phrases) < 2 {
		return "", false
	}
	return strings.Join(phrases, " OR "), true
}

func normalizeDOIString(raw string) string {
	doi := strings.TrimSpace(raw)
	doi = strings.TrimPrefix(doi, "https://doi.org/")
	doi = strings.TrimPrefix(doi, "http://doi.org/")
	return doi
}

func openAlexResultURL(item OpenAlexWork) string {
	if strings.TrimSpace(item.DOI) != "" {
		doi := strings.TrimSpace(item.DOI)
		doi = strings.TrimPrefix(doi, "https://doi.org/")
		doi = strings.TrimPrefix(doi, "http://doi.org/")
		return "https://doi.org/" + doi
	}
	if item.PrimaryLocation != nil && strings.TrimSpace(item.PrimaryLocation.LandingPageURL) != "" {
		return strings.TrimSpace(item.PrimaryLocation.LandingPageURL)
	}
	if item.PrimaryLocation != nil && strings.TrimSpace(item.PrimaryLocation.PDFURL) != "" {
		return strings.TrimSpace(item.PrimaryLocation.PDFURL)
	}
	if strings.TrimSpace(item.ID) != "" {
		return strings.TrimSpace(item.ID)
	}
	return ""
}

func openAlexAbstractToText(inverted map[string][]int) string {
	if len(inverted) == 0 {
		return ""
	}

	maxPos := -1
	for _, positions := range inverted {
		for _, pos := range positions {
			if pos > maxPos {
				maxPos = pos
			}
		}
	}
	if maxPos < 0 || maxPos > 5000 {
		return ""
	}

	words := make([]string, maxPos+1)
	for token, positions := range inverted {
		for _, pos := range positions {
			if pos < 0 || pos >= len(words) {
				continue
			}
			if words[pos] == "" {
				words[pos] = token
			}
		}
	}

	b := strings.Builder{}
	for _, w := range words {
		if strings.TrimSpace(w) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(w)
	}

	return normalizeSpace(b.String())
}

func parseSearchResultsHTML(html []byte, numResults int) ([]PaperResult, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, err
	}

	results := make([]PaperResult, 0, numResults)
	doc.Find("div.gs_ri").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if len(results) >= numResults {
			return false
		}

		titleSel := sel.Find("h3.gs_rt").First()
		title := normalizeSpace(titleSel.Text())
		if title == "" {
			title = "No title available"
		}

		link, _ := titleSel.Find("a").Attr("href")
		if strings.TrimSpace(link) == "" {
			link = "No link available"
		}

		authors := normalizeSpace(sel.Find("div.gs_a").First().Text())
		if authors == "" {
			authors = "No authors available"
		}

		abstract := normalizeSpace(sel.Find("div.gs_rs").First().Text())
		if abstract == "" {
			abstract = "No abstract available"
		}

		results = append(results, PaperResult{
			Title:            title,
			Authors:          authors,
			Abstract:         abstract,
			URL:              link,
			SnippetTruncated: looksLikeTruncatedSnippet(abstract),
			Source:           "google_scholar",
		})
		return true
	})

	return results, nil
}

func buildSearchURL(query, author string, yearRange []int) string {
	q := strings.TrimSpace(query)
	params := url.Values{}
	params.Set("q", q)
	if strings.TrimSpace(author) != "" {
		params.Set("as_auth", strings.TrimSpace(author))
	}
	if len(yearRange) == 2 {
		params.Set("as_ylo", strconv.Itoa(yearRange[0]))
		params.Set("as_yhi", strconv.Itoa(yearRange[1]))
	}
	return "https://scholar.google.com/scholar?" + params.Encode()
}

func normalizeSpace(input string) string {
	fields := strings.Fields(strings.ReplaceAll(input, "\u00a0", " "))
	return strings.TrimSpace(strings.Join(fields, " "))
}

func looksLikeBlocked(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "unusual traffic") || strings.Contains(text, "not a robot") || strings.Contains(text, "captcha")
}

func looksLikeTruncatedSnippet(abstract string) bool {
	trimmed := strings.TrimSpace(abstract)
	return strings.HasSuffix(trimmed, "...") || strings.HasSuffix(trimmed, "…")
}
