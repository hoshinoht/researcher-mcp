package scholar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func SearchByKeywords(ctx context.Context, requester *Requester, query string, numResults int) ([]PaperResult, *ToolError) {
	searchURL := buildSearchURL(query, "", nil)
	return searchWithFallback(ctx, requester, searchURL, query, "", nil, numResults)
}

func SearchAdvanced(ctx context.Context, requester *Requester, query, author string, yearRange []int, numResults int) ([]PaperResult, *ToolError) {
	searchURL := buildSearchURL(query, author, yearRange)
	return searchWithFallback(ctx, requester, searchURL, query, author, yearRange, numResults)
}

func searchWithFallback(ctx context.Context, requester *Requester, scholarURL, query, author string, yearRange []int, numResults int) ([]PaperResult, *ToolError) {
	results, scholarErr := searchScholar(ctx, requester, scholarURL, numResults)
	if scholarErr == nil {
		return results, nil
	}

	if !shouldFallbackToOpenAlex(scholarErr.Code) {
		return nil, scholarErr
	}

	openAlexResults, openAlexErr := searchOpenAlex(ctx, requester, query, author, yearRange, numResults)
	if openAlexErr == nil {
		return openAlexResults, nil
	}

	return nil, openAlexErr
}

func searchScholar(ctx context.Context, requester *Requester, searchURL string, numResults int) ([]PaperResult, *ToolError) {
	if strings.TrimSpace(searchURL) == "" {
		return nil, &ToolError{Code: "invalid_input", Message: "search URL is empty"}
	}

	body, status, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("request failed: %v", err)}
	}

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
			return nil, &ToolError{Code: "blocked", Message: "Google Scholar appears to have blocked this automated request"}
		}
		return nil, &ToolError{Code: "no_results", Message: "no results found"}
	}

	return results, nil
}

func shouldFallbackToOpenAlex(code string) bool {
	switch code {
	case "blocked", "no_results", "upstream_error", "parse_failed":
		return true
	default:
		return false
	}
}

type openAlexWorksSearchResponse struct {
	Results []openAlexWorkResult `json:"results"`
}

type openAlexAuthorship struct {
	Author struct {
		DisplayName string `json:"display_name"`
	} `json:"author"`
}

type openAlexWorkResult struct {
	ID                    string               `json:"id"`
	DOI                   string               `json:"doi"`
	DisplayName           string               `json:"display_name"`
	PublicationYear       int                  `json:"publication_year"`
	AbstractInvertedIndex map[string][]int     `json:"abstract_inverted_index"`
	Authorships           []openAlexAuthorship `json:"authorships"`
	PrimaryLocation       struct {
		LandingPageURL string `json:"landing_page_url"`
		PDFURL         string `json:"pdf_url"`
	} `json:"primary_location"`
}

func searchOpenAlex(ctx context.Context, requester *Requester, query, author string, yearRange []int, numResults int) ([]PaperResult, *ToolError) {
	searchURL := buildOpenAlexSearchURL(query, author, yearRange, numResults)
	body, status, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("openalex request failed: %v", err)}
	}
	if status != http.StatusOK {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("openalex request failed with status %d", status)}
	}

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

		title := normalizeSpace(item.DisplayName)
		if title == "" {
			title = "No title available"
		}

		authors := openAlexAuthorsToString(item.Authorships)
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

		results = append(results, PaperResult{
			Title:            title,
			Authors:          authors,
			Abstract:         abstract,
			URL:              resultURL,
			SnippetTruncated: false,
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

func openAlexAuthorsToString(authorships []openAlexAuthorship) string {
	names := make([]string, 0, len(authorships))
	seen := map[string]struct{}{}
	for _, authorship := range authorships {
		name := normalizeSpace(authorship.Author.DisplayName)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func openAlexResultURL(item openAlexWorkResult) string {
	if strings.TrimSpace(item.DOI) != "" {
		doi := strings.TrimSpace(item.DOI)
		doi = strings.TrimPrefix(doi, "https://doi.org/")
		doi = strings.TrimPrefix(doi, "http://doi.org/")
		return "https://doi.org/" + doi
	}
	if strings.TrimSpace(item.PrimaryLocation.LandingPageURL) != "" {
		return strings.TrimSpace(item.PrimaryLocation.LandingPageURL)
	}
	if strings.TrimSpace(item.PrimaryLocation.PDFURL) != "" {
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
