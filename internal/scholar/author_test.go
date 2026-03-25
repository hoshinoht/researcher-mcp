package scholar

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestRequester(rt http.RoundTripper) *Requester {
	cfg := config.Config{
		MinDelay:         0,
		MaxDelay:         0,
		MaxRetries:       1,
		BackoffFactor:    1,
		RotateUserAgents: false,
		UserAgents:       []string{"unit-test-agent"},
		Timeout:          2 * time.Second,
	}

	return &Requester{
		cfg:        cfg,
		client:     &http.Client{Transport: rt, Timeout: cfg.Timeout},
		lastByHost: make(map[string]time.Time),
		rng:        rand.New(rand.NewSource(1)),
	}
}

func httpResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGetAuthorInfo_SplitsCommaSeparatedNames(t *testing.T) {
	searched := make([]string, 0, 2)
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.openalex.org" {
			return httpResponse(http.StatusOK, `{"message":{"items":[]}}`), nil
		}

		query := req.URL.Query().Get("search")
		searched = append(searched, query)

		switch query {
		case "Ying Li":
			return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/A123","display_name":"Ying Li","works_count":12,"cited_by_count":109,"works_api_url":"","affiliations":[{"institution":{"display_name":"Rice University"}}],"x_concepts":[{"display_name":"Machine learning","score":0.8}]}]}`), nil
		case "Ying Li, Lei Wu":
			return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/A999","display_name":"Xuedong Zhou","works_count":3,"cited_by_count":15,"works_api_url":""}]}`), nil
		default:
			return httpResponse(http.StatusOK, `{"results":[]}`), nil
		}
	}))

	author, toolErr := GetAuthorInfo(context.Background(), requester, "Ying Li, Lei Wu")
	if toolErr != nil {
		t.Fatalf("unexpected tool error: %+v", toolErr)
	}
	if author == nil {
		t.Fatalf("expected author result")
	}
	if author.Name != "Ying Li" {
		t.Fatalf("expected Ying Li, got %q", author.Name)
	}
	if len(searched) == 0 || searched[0] != "Ying Li" {
		t.Fatalf("expected first OpenAlex query to be split candidate, got %v", searched)
	}
	for _, q := range searched {
		if q == "Ying Li, Lei Wu" {
			t.Fatalf("did not expect combined multi-name query to be used after split")
		}
	}
}

func TestGetAuthorInfo_OpenAlexChoosesBestNameMatch(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.openalex.org" {
			return httpResponse(http.StatusOK, `{"message":{"items":[]}}`), nil
		}

		return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/A1","display_name":"Xuedong Zhou","works_count":90,"cited_by_count":300,"works_api_url":""},{"id":"https://openalex.org/A2","display_name":"Andrew Y. Ng","works_count":200,"cited_by_count":25000,"works_api_url":""}]}`), nil
	}))

	author, toolErr := GetAuthorInfo(context.Background(), requester, "Andrew Ng")
	if toolErr != nil {
		t.Fatalf("unexpected tool error: %+v", toolErr)
	}
	if author == nil {
		t.Fatalf("expected author result")
	}
	if author.Name != "Andrew Y. Ng" {
		t.Fatalf("expected best-matched OpenAlex author, got %q", author.Name)
	}
}

func TestSplitAuthorNameCandidates_SpecialCharactersAndEmptyParts(t *testing.T) {
	got := splitAuthorNameCandidates("  Jos\u00e9 Garc\u00eda , Fran\u00e7ois Dupont , ,  ")
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d (%v)", len(got), got)
	}
	if got[0] != "Jos\u00e9 Garc\u00eda" {
		t.Fatalf("unexpected first candidate: %q", got[0])
	}
	if got[1] != "Fran\u00e7ois Dupont" {
		t.Fatalf("unexpected second candidate: %q", got[1])
	}
}

func TestGetAuthorInfo_EmptyNameReturnsInvalidInput(t *testing.T) {
	author, toolErr := GetAuthorInfo(context.Background(), nil, "   ")
	if toolErr == nil {
		t.Fatalf("expected invalid_input error")
	}
	if toolErr.Code != "invalid_input" {
		t.Fatalf("expected invalid_input code, got %q", toolErr.Code)
	}
	if author != nil {
		t.Fatalf("expected nil author for invalid input")
	}
}

func TestGetAuthorInfo_PreservesParseFailureWhenFallbacksHaveNoResults(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "api.openalex.org":
			return httpResponse(http.StatusOK, `{"results":`), nil
		case "api.crossref.org":
			return httpResponse(http.StatusOK, `{"message":{"items":[]}}`), nil
		case "pub.orcid.org":
			return httpResponse(http.StatusOK, "orcid,given-and-family-names,current-institution-affiliation-name\n"), nil
		case "scholar.google.com":
			return httpResponse(http.StatusOK, "<html><body>no author blocks</body></html>"), nil
		default:
			return httpResponse(http.StatusNotFound, ""), nil
		}
	}))

	author, toolErr := GetAuthorInfo(context.Background(), requester, "Any Name")
	if author != nil {
		t.Fatalf("expected nil author when providers fail")
	}
	if toolErr == nil {
		t.Fatalf("expected parse_failed error")
	}
	if toolErr.Code != "parse_failed" {
		t.Fatalf("expected parse_failed code, got %q", toolErr.Code)
	}
}

func TestGetAuthorInfo_ReturnsUpstreamErrorWhenProvidersUnreachable(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial timeout")
	}))

	author, toolErr := GetAuthorInfo(context.Background(), requester, "Andrew Ng")
	if author != nil {
		t.Fatalf("expected nil author when upstream is unreachable")
	}
	if toolErr == nil {
		t.Fatalf("expected upstream_error")
	}
	if toolErr.Code != "upstream_error" {
		t.Fatalf("expected upstream_error code, got %q", toolErr.Code)
	}
}
