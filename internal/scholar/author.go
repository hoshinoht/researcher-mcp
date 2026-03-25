package scholar

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

func GetAuthorInfo(ctx context.Context, requester *Requester, authorName string) (*AuthorInfo, *ToolError) {
	trimmedName := strings.TrimSpace(authorName)
	if trimmedName == "" {
		return nil, &ToolError{Code: "invalid_input", Message: "author_name is required"}
	}
	candidates := splitAuthorNameCandidates(trimmedName)
	if len(candidates) == 0 {
		return nil, &ToolError{Code: "invalid_input", Message: "author_name is required"}
	}

	var lastErr *ToolError
	var firstHardErr *ToolError
	for _, candidate := range candidates {
		author, toolErr := getAuthorInfoSingleName(ctx, requester, candidate)
		if toolErr == nil {
			return author, nil
		}

		lastErr = toolErr
		if toolErr.Code != "no_results" && firstHardErr == nil {
			firstHardErr = toolErr
		}
	}

	if firstHardErr != nil {
		return nil, firstHardErr
	}
	if lastErr != nil {
		return nil, lastErr
	}

	return nil, &ToolError{Code: "no_results", Message: "author not found"}
}

func getAuthorInfoSingleName(ctx context.Context, requester *Requester, authorName string) (*AuthorInfo, *ToolError) {

	var lastErr *ToolError
	var firstHardErr *ToolError

	if author, err := getAuthorInfoFromOpenAlex(ctx, requester, authorName); err == nil {
		return author, nil
	} else {
		lastErr = err
		if err.Code != "no_results" && firstHardErr == nil {
			firstHardErr = err
		}
	}

	if author, err := getAuthorInfoFromCrossref(ctx, requester, authorName); err == nil {
		return author, nil
	} else {
		lastErr = err
		if err.Code != "no_results" && firstHardErr == nil {
			firstHardErr = err
		}
	}

	if author, err := getAuthorInfoFromORCID(ctx, requester, authorName); err == nil {
		return author, nil
	} else {
		lastErr = err
		if err.Code != "no_results" && firstHardErr == nil {
			firstHardErr = err
		}
	}

	// Scholar is now a final fallback only.
	if author, err := getAuthorInfoFromScholar(ctx, requester, authorName); err == nil {
		author.Source = "google_scholar"
		if author.ExternalIDs == nil {
			author.ExternalIDs = map[string]string{}
		}
		return author, nil
	} else {
		lastErr = err
		if err.Code != "no_results" && firstHardErr == nil {
			firstHardErr = err
		}
	}

	if firstHardErr != nil {
		return nil, firstHardErr
	}
	if lastErr == nil {
		return nil, &ToolError{Code: "no_results", Message: "author not found"}
	}
	return nil, lastErr
}

func splitAuthorNameCandidates(authorName string) []string {
	trimmed := normalizeSpace(authorName)
	if trimmed == "" {
		return nil
	}

	parts := strings.Split(trimmed, ",")
	if len(parts) == 1 {
		return []string{trimmed}
	}

	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := normalizeSpace(part)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}

	if len(out) == 0 {
		return []string{trimmed}
	}
	return out
}

type openAlexAuthorResponse struct {
	Results []openAlexAuthorResult `json:"results"`
}

type openAlexAuthorResult struct {
		ID           string `json:"id"`
		ORCID        string `json:"orcid"`
		DisplayName  string `json:"display_name"`
		WorksCount   int    `json:"works_count"`
		CitedByCount int    `json:"cited_by_count"`
		WorksAPIURL  string `json:"works_api_url"`
		Affiliations []struct {
			Institution struct {
				DisplayName string `json:"display_name"`
			} `json:"institution"`
		} `json:"affiliations"`
		XConcepts []struct {
			DisplayName string  `json:"display_name"`
			Score       float64 `json:"score"`
		} `json:"x_concepts"`
}

type openAlexWorksResponse struct {
	Results []struct {
		Title           string `json:"title"`
		PublicationYear int    `json:"publication_year"`
		CitedByCount    int    `json:"cited_by_count"`
	} `json:"results"`
}

func getAuthorInfoFromOpenAlex(ctx context.Context, requester *Requester, authorName string) (*AuthorInfo, *ToolError) {
	searchURL := "https://api.openalex.org/authors?search=" + url.QueryEscape(authorName) + "&per-page=10"
	body, status, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("openalex author search failed: %v", err)}
	}
	if status != http.StatusOK {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("openalex author search failed with status %d", status)}
	}

	var resp openAlexAuthorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &ToolError{Code: "parse_failed", Message: fmt.Sprintf("openalex author parse failed: %v", err)}
	}
	if len(resp.Results) == 0 {
		return nil, &ToolError{Code: "no_results", Message: "openalex author not found"}
	}

	r := chooseBestOpenAlexAuthor(resp.Results, authorName)
	affiliation := "N/A"
	if len(r.Affiliations) > 0 && strings.TrimSpace(r.Affiliations[0].Institution.DisplayName) != "" {
		affiliation = r.Affiliations[0].Institution.DisplayName
	}

	interests := make([]string, 0, 5)
	for _, c := range r.XConcepts {
		if len(interests) >= 5 {
			break
		}
		topic := normalizeSpace(c.DisplayName)
		if topic != "" {
			interests = append(interests, topic)
		}
	}

	publications := make([]Publication, 0, 5)
	if strings.TrimSpace(r.WorksAPIURL) != "" {
		worksURL := r.WorksAPIURL + "&per-page=5&sort=cited_by_count:desc"
		worksBody, worksStatus, worksErr := requester.Get(ctx, worksURL)
		if worksErr == nil && worksStatus == http.StatusOK {
			var worksResp openAlexWorksResponse
			if err := json.Unmarshal(worksBody, &worksResp); err == nil {
				for _, work := range worksResp.Results {
					year := "N/A"
					if work.PublicationYear > 0 {
						year = strconv.Itoa(work.PublicationYear)
					}
					title := normalizeSpace(work.Title)
					if title == "" {
						title = "N/A"
					}
					publications = append(publications, Publication{
						Title:     title,
						Year:      year,
						Citations: work.CitedByCount,
					})
				}
			}
		}
	}

	externalIDs := map[string]string{}
	if strings.TrimSpace(r.ID) != "" {
		externalIDs["openalex"] = r.ID
	}
	if strings.TrimSpace(r.ORCID) != "" {
		externalIDs["orcid"] = r.ORCID
	}

	name := normalizeSpace(r.DisplayName)
	if name == "" {
		name = authorName
	}

	return &AuthorInfo{
		Name:         name,
		Affiliation:  affiliation,
		Interests:    interests,
		CitedBy:      r.CitedByCount,
		Publications: publications,
		Source:       "openalex",
		ExternalIDs:  externalIDs,
	}, nil
}

func chooseBestOpenAlexAuthor(results []openAlexAuthorResult, requestedName string) openAlexAuthorResult {
	if len(results) == 0 {
		return openAlexAuthorResult{}
	}
	target := canonicalPersonName(requestedName)
	if target == "" {
		return results[0]
	}

	bestIdx := 0
	bestScore := -1
	bestCitations := -1

	for i, result := range results {
		score := nameMatchScore(target, canonicalPersonName(result.DisplayName))
		if score > bestScore || (score == bestScore && result.CitedByCount > bestCitations) {
			bestIdx = i
			bestScore = score
			bestCitations = result.CitedByCount
		}
	}

	return results[bestIdx]
}

func canonicalPersonName(name string) string {
	name = normalizeSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}

	b := strings.Builder{}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}

	return normalizeSpace(b.String())
}

func nameMatchScore(target, candidate string) int {
	if target == "" || candidate == "" {
		return 0
	}
	if target == candidate {
		return 1000
	}

	score := 0
	if strings.Contains(candidate, target) || strings.Contains(target, candidate) {
		score += 200
	}

	targetTokens := strings.Fields(target)
	candidateTokens := strings.Fields(candidate)
	if len(targetTokens) == 0 || len(candidateTokens) == 0 {
		return score
	}

	candidateSet := make(map[string]struct{}, len(candidateTokens))
	for _, token := range candidateTokens {
		candidateSet[token] = struct{}{}
	}

	overlap := 0
	for _, token := range targetTokens {
		if _, ok := candidateSet[token]; ok {
			overlap++
		}
	}
	score += overlap * 40

	if targetTokens[0] == candidateTokens[0] {
		score += 20
	}
	if targetTokens[len(targetTokens)-1] == candidateTokens[len(candidateTokens)-1] {
		score += 20
	}

	if len(targetTokens) > len(candidateTokens) {
		score -= (len(targetTokens) - len(candidateTokens)) * 5
	} else {
		score -= (len(candidateTokens) - len(targetTokens)) * 5
	}

	return score
}

type crossrefWorksResponse struct {
	Message struct {
		Items []struct {
			DOI                 string   `json:"DOI"`
			Title               []string `json:"title"`
			IsReferencedByCount int      `json:"is-referenced-by-count"`
			Issued              struct {
				DateParts [][]int `json:"date-parts"`
			} `json:"issued"`
			Author []struct {
				Given       string `json:"given"`
				Family      string `json:"family"`
				Affiliation []struct {
					Name string `json:"name"`
				} `json:"affiliation"`
			} `json:"author"`
		} `json:"items"`
	} `json:"message"`
}

func getAuthorInfoFromCrossref(ctx context.Context, requester *Requester, authorName string) (*AuthorInfo, *ToolError) {
	searchURL := "https://api.crossref.org/works?query.author=" + url.QueryEscape(authorName) + "&rows=5"
	body, status, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("crossref author search failed: %v", err)}
	}
	if status != http.StatusOK {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("crossref author search failed with status %d", status)}
	}

	var resp crossrefWorksResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &ToolError{Code: "parse_failed", Message: fmt.Sprintf("crossref parse failed: %v", err)}
	}
	if len(resp.Message.Items) == 0 {
		return nil, &ToolError{Code: "no_results", Message: "crossref author not found"}
	}

	name := authorName
	affiliation := "N/A"
	citedBy := 0
	externalIDs := map[string]string{}
	publications := make([]Publication, 0, 5)

	for _, item := range resp.Message.Items {
		if item.IsReferencedByCount > citedBy {
			citedBy = item.IsReferencedByCount
		}
		title := "N/A"
		if len(item.Title) > 0 {
			title = normalizeSpace(item.Title[0])
			if title == "" {
				title = "N/A"
			}
		}
		year := "N/A"
		if len(item.Issued.DateParts) > 0 && len(item.Issued.DateParts[0]) > 0 {
			year = strconv.Itoa(item.Issued.DateParts[0][0])
		}
		publications = append(publications, Publication{
			Title:     title,
			Year:      year,
			Citations: item.IsReferencedByCount,
		})

		if len(item.Author) > 0 {
			a := item.Author[0]
			candidateName := normalizeSpace(strings.TrimSpace(a.Given + " " + a.Family))
			if candidateName != "" {
				name = candidateName
			}
			if len(a.Affiliation) > 0 {
				aff := normalizeSpace(a.Affiliation[0].Name)
				if aff != "" {
					affiliation = aff
				}
			}
		}

		if strings.TrimSpace(item.DOI) != "" {
			externalIDs["doi"] = item.DOI
		}
	}

	return &AuthorInfo{
		Name:         name,
		Affiliation:  affiliation,
		Interests:    []string{},
		CitedBy:      citedBy,
		Publications: publications,
		Source:       "crossref",
		ExternalIDs:  externalIDs,
	}, nil
}

func getAuthorInfoFromORCID(ctx context.Context, requester *Requester, authorName string) (*AuthorInfo, *ToolError) {
	q := fmt.Sprintf("given-and-family-names:(\"%s\")", authorName)
	fields := "orcid,given-and-family-names,current-institution-affiliation-name"
	searchURL := "https://pub.orcid.org/v3.0/csv-search/?q=" + url.QueryEscape(q) + "&fl=" + url.QueryEscape(fields)
	body, status, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("orcid author search failed: %v", err)}
	}
	if status != http.StatusOK {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("orcid author search failed with status %d", status)}
	}

	reader := csv.NewReader(bytes.NewReader(body))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, &ToolError{Code: "parse_failed", Message: fmt.Sprintf("orcid csv parse failed: %v", err)}
	}
	if len(records) < 2 {
		return nil, &ToolError{Code: "no_results", Message: "orcid author not found"}
	}

	header := records[0]
	row := records[1]
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}

	get := func(key string) string {
		i, ok := idx[key]
		if !ok || i >= len(row) {
			return ""
		}
		return normalizeSpace(row[i])
	}

	orcidID := get("orcid")
	name := get("given-and-family-names")
	if name == "" {
		name = authorName
	}
	affiliation := get("current-institution-affiliation-name")
	if affiliation == "" {
		affiliation = "N/A"
	}

	externalIDs := map[string]string{}
	if orcidID != "" {
		externalIDs["orcid"] = orcidID
	}

	return &AuthorInfo{
		Name:         name,
		Affiliation:  affiliation,
		Interests:    []string{},
		CitedBy:      0,
		Publications: []Publication{},
		Source:       "orcid",
		ExternalIDs:  externalIDs,
	}, nil
}

func getAuthorInfoFromScholar(ctx context.Context, requester *Requester, authorName string) (*AuthorInfo, *ToolError) {
	searchURL := "https://scholar.google.com/citations?view_op=search_authors&mauthors=" + url.QueryEscape(strings.TrimSpace(authorName))
	body, status, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("author search failed: %v", err)}
	}
	if status != http.StatusOK {
		if status == http.StatusForbidden || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			return nil, BuildBlockedError(status)
		}
		return nil, &ToolError{Code: "upstream_error", Message: fmt.Sprintf("author search failed with status %d", status)}
	}

	author, profileURL, parseErr := parseAuthorSearchResult(body)
	if parseErr != nil {
		return nil, &ToolError{Code: "parse_failed", Message: parseErr.Error()}
	}
	if author == nil {
		if looksLikeBlocked(body) {
			return nil, &ToolError{Code: "blocked", Message: "Google Scholar appears to have blocked this automated request"}
		}
		return nil, &ToolError{Code: "no_results", Message: "author not found"}
	}

	if profileURL == "" {
		return author, nil
	}

	profileBody, profileStatus, profileErr := requester.Get(ctx, profileURL)
	if profileErr != nil {
		return author, nil
	}
	if profileStatus != http.StatusOK {
		return author, nil
	}

	fillAuthorFromProfile(author, profileBody)
	return author, nil
}

func parseAuthorSearchResult(html []byte) (*AuthorInfo, string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, "", err
	}

	first := doc.Find("div.gsc_1usr").First()
	if first.Length() == 0 {
		return nil, "", nil
	}

	name := normalizeSpace(first.Find("h3.gs_ai_name a").First().Text())
	if name == "" {
		name = "N/A"
	}
	affiliation := normalizeSpace(first.Find("div.gs_ai_aff").First().Text())
	if affiliation == "" {
		affiliation = "N/A"
	}

	interests := make([]string, 0)
	first.Find("a.gs_ai_one_int").Each(func(_ int, sel *goquery.Selection) {
		t := normalizeSpace(sel.Text())
		if t != "" {
			interests = append(interests, t)
		}
	})

	citedBy := 0
	citedText := normalizeSpace(first.Find("div.gs_ai_cby").First().Text())
	if idx := strings.LastIndex(citedText, " "); idx > -1 {
		v, err := strconv.Atoi(strings.TrimSpace(citedText[idx+1:]))
		if err == nil {
			citedBy = v
		}
	}

	href, _ := first.Find("h3.gs_ai_name a").First().Attr("href")
	profileURL := ""
	if strings.TrimSpace(href) != "" {
		profileURL = "https://scholar.google.com" + href
	}

	return &AuthorInfo{
		Name:         name,
		Affiliation:  affiliation,
		Interests:    interests,
		CitedBy:      citedBy,
		Publications: []Publication{},
	}, profileURL, nil
}

func fillAuthorFromProfile(author *AuthorInfo, html []byte) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return
	}

	if author.Name == "N/A" {
		profileName := normalizeSpace(doc.Find("#gsc_prf_in").First().Text())
		if profileName != "" {
			author.Name = profileName
		}
	}

	if author.Affiliation == "N/A" {
		aff := normalizeSpace(doc.Find(".gsc_prf_il").First().Text())
		if aff != "" {
			author.Affiliation = aff
		}
	}

	publications := make([]Publication, 0, 5)
	doc.Find("tr.gsc_a_tr").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if len(publications) >= 5 {
			return false
		}

		title := normalizeSpace(sel.Find("a.gsc_a_at").First().Text())
		if title == "" {
			title = "N/A"
		}

		citations := 0
		citText := normalizeSpace(sel.Find("a.gsc_a_ac").First().Text())
		if citText != "" {
			if c, err := strconv.Atoi(citText); err == nil {
				citations = c
			}
		}

		year := normalizeSpace(sel.Find("span.gsc_a_h").First().Text())
		if year == "" {
			year = normalizeSpace(sel.Find("span.gsc_a_y").First().Text())
		}
		if year == "" {
			year = "N/A"
		}

		publications = append(publications, Publication{
			Title:     title,
			Year:      year,
			Citations: citations,
		})
		return true
	})
	author.Publications = publications
}
