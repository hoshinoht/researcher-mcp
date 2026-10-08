package scholar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"googlescholar-mcp-go/internal/config"
)

type crossrefAuthor struct {
	Given       string `json:"given"`
	Family      string `json:"family"`
	Name        string `json:"name"`
	ORCID       string `json:"ORCID"`
	Affiliation []struct {
		Name string `json:"name"`
	} `json:"affiliation"`
}

func (a crossrefAuthor) FullName() string {
	if n := normalizeSpace(strings.TrimSpace(a.Given + " " + a.Family)); n != "" {
		return n
	}
	return normalizeSpace(a.Name)
}

type crossrefWork struct {
	DOI                 string           `json:"DOI"`
	URL                 string           `json:"URL"`
	Title               []string         `json:"title"`
	Abstract            string           `json:"abstract"`
	IsReferencedByCount *int             `json:"is-referenced-by-count"`
	Author              []crossrefAuthor `json:"author"`
	Issued              struct {
		DateParts [][]int `json:"date-parts"`
	} `json:"issued"`
}

func (w crossrefWork) year() int {
	if len(w.Issued.DateParts) > 0 && len(w.Issued.DateParts[0]) > 0 {
		return w.Issued.DateParts[0][0]
	}
	return 0
}

func (w crossrefWork) title() string {
	if len(w.Title) > 0 {
		return normalizeSpace(w.Title[0])
	}
	return ""
}

type crossrefWorksResponse struct {
	Message struct {
		Items []crossrefWork `json:"items"`
	} `json:"message"`
}

func buildCrossrefSearchURL(req SearchRequest) string {
	params := url.Values{}
	params.Set("query.bibliographic", strings.TrimSpace(req.Query))
	params.Set("rows", strconv.Itoa(req.NumResults))
	params.Set("select", "DOI,URL,title,abstract,author,issued,is-referenced-by-count")
	if a := strings.TrimSpace(req.Author); a != "" {
		params.Set("query.author", a)
	}
	if len(req.YearRange) == 2 {
		start, end := req.YearRange[0], req.YearRange[1]
		if start > end {
			start, end = end, start
		}
		params.Set("filter", fmt.Sprintf("from-pub-date:%d,until-pub-date:%d", start, end))
	}
	return "https://api.crossref.org/works?" + params.Encode()
}

func searchCrossref(ctx context.Context, requester *Requester, req SearchRequest) ([]PaperResult, *ToolError) {
	body, status, err := requester.Get(ctx, buildCrossrefSearchURL(req))
	if err != nil {
		return nil, requestError("crossref", err)
	}
	if status != http.StatusOK {
		return nil, statusError("crossref", status)
	}
	var resp crossrefWorksResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &ToolError{Code: CodeParseFailed, Message: fmt.Sprintf("crossref parse failed: %v", err)}
	}

	results := make([]PaperResult, 0, req.NumResults)
	for _, item := range resp.Message.Items {
		if len(results) >= req.NumResults {
			break
		}
		title := item.title()
		if title == "" {
			continue
		}
		names := make([]string, 0, len(item.Author))
		for _, a := range item.Author {
			if n := a.FullName(); n != "" {
				names = append(names, n)
			}
		}
		authors := strings.Join(names, ", ")
		if authors == "" {
			authors = "No authors available"
		}
		abstract := normalizeSpace(htmlTagPattern.ReplaceAllString(item.Abstract, " "))
		if abstract == "" {
			abstract = "No abstract available"
		}
		doi, _ := NormalizeDOI(item.DOI)
		link := strings.TrimSpace(item.URL)
		if doi != "" {
			link = "https://doi.org/" + doi
		}
		if link == "" {
			link = "No link available"
		}
		results = append(results, PaperResult{
			Title:    title,
			Authors:  authors,
			Abstract: abstract,
			URL:      link,
			DOI:      doi,
			Year:     item.year(),
			Source:   config.ProviderCrossref,
		})
	}
	if len(results) == 0 {
		return nil, &ToolError{Code: CodeNoResults, Message: "no results found"}
	}
	return results, nil
}
