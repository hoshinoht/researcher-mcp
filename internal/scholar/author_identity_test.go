package scholar

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Two different, comparably established people named Wei Wang: neither
// dominates the other, so name alone must stay ambiguous.
const twoWeiWangs = `{"results":[
 {"id":"https://openalex.org/A1","orcid":"https://orcid.org/0000-0002-1825-0097","display_name":"Wei Wang","works_count":900,"cited_by_count":50000,
  "summary_stats":{"h_index":90,"i10_index":400},
  "last_known_institutions":[{"id":"https://openalex.org/I1","display_name":"University of California, Los Angeles"}]},
 {"id":"https://openalex.org/A2","display_name":"Wei Wang","works_count":300,"cited_by_count":9000,
  "last_known_institutions":[{"id":"https://openalex.org/I2","display_name":"Tsinghua University"}]},
 {"id":"https://openalex.org/A3","display_name":"Wei Zhang","works_count":10,"cited_by_count":5}
]}`

func openAlexAuthorTransport(t *testing.T, extra func(req *http.Request) (*http.Response, bool)) roundTripperFunc {
	return func(req *http.Request) (*http.Response, error) {
		if extra != nil {
			if resp, ok := extra(req); ok {
				return resp, nil
			}
		}
		switch {
		case req.URL.Host == "api.openalex.org" && req.URL.Path == "/authors":
			return httpResponse(http.StatusOK, twoWeiWangs), nil
		case req.URL.Host == "api.openalex.org" && req.URL.Path == "/works":
			return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/W9","display_name":"A cited work","publication_year":2020,"cited_by_count":1234}]}`), nil
		default:
			t.Errorf("unexpected request %s", req.URL)
			return httpResponse(http.StatusNotFound, ""), nil
		}
	}
}

func TestGetAuthorInfo_SameNamePeopleAreAmbiguous(t *testing.T) {
	requester := newTestRequester(openAlexAuthorTransport(t, nil))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Wei Wang"})
	if author != nil {
		t.Fatalf("resolved %q by weak evidence; want ambiguity", author.Name)
	}
	if toolErr == nil || toolErr.Code != CodeAmbiguous {
		t.Fatalf("toolErr = %+v, want ambiguous", toolErr)
	}
	if len(toolErr.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want the two Wei Wang profiles only", toolErr.Candidates)
	}
	if toolErr.Candidates[0].ID != "A1" || toolErr.Candidates[0].ORCID != "0000-0002-1825-0097" || toolErr.Candidates[0].Affiliation == "" {
		t.Fatalf("candidate details missing: %+v", toolErr.Candidates[0])
	}
}

func TestGetAuthorInfo_AffiliationEvidenceResolves(t *testing.T) {
	requester := newTestRequester(openAlexAuthorTransport(t, nil))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Wei Wang", Affiliation: "Tsinghua University"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.ExternalIDs["openalex"] != "https://openalex.org/A2" {
		t.Fatalf("resolved %v, want A2 (Tsinghua) in URL form", author.ExternalIDs)
	}
	if author.Match == nil || author.Match.Method != "name+affiliation" {
		t.Fatalf("match = %+v", author.Match)
	}
}

func TestGetAuthorInfo_KnownPaperEvidenceResolves(t *testing.T) {
	requester := newTestRequester(openAlexAuthorTransport(t, func(req *http.Request) (*http.Response, bool) {
		if req.URL.Host == "api.openalex.org" && strings.HasPrefix(req.URL.Path, "/works/https://doi.org/") {
			return httpResponse(http.StatusOK, `{"id":"https://openalex.org/W5","display_name":"Known Paper","authorships":[{"author":{"id":"https://openalex.org/A1","display_name":"Wei Wang"}}]}`), true
		}
		return nil, false
	}))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Wei Wang", KnownPaper: "10.1000/known"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.ExternalIDs["openalex"] != "https://openalex.org/A1" || author.ExternalIDs["orcid"] != "https://orcid.org/0000-0002-1825-0097" || author.Match.Confidence != "high" {
		t.Fatalf("author = %v match = %+v", author.ExternalIDs, author.Match)
	}
}

func TestGetAuthorInfo_ExactORCIDUsesIdentifierLookup(t *testing.T) {
	var paths []string
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch {
		case req.URL.Path == "/authors/orcid:0000-0002-1825-0097":
			return httpResponse(http.StatusOK, `{"id":"https://openalex.org/A1","orcid":"https://orcid.org/0000-0002-1825-0097","display_name":"Wei Wang","works_count":900,"cited_by_count":50000}`), nil
		case req.URL.Path == "/works":
			return httpResponse(http.StatusOK, `{"results":[]}`), nil
		}
		t.Errorf("unexpected request %s", req.URL)
		return httpResponse(http.StatusNotFound, ""), nil
	}))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{ORCID: "https://orcid.org/0000-0002-1825-0097"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.Match.Method != "orcid" || author.Match.Confidence != "high" {
		t.Fatalf("match = %+v", author.Match)
	}
	for _, p := range paths {
		if p == "/authors" {
			t.Fatal("name search should not run when an exact identifier is supplied")
		}
	}
}

func TestGetAuthorInfo_ConflictingIdentifiers(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/authors/A2" {
			return httpResponse(http.StatusOK, `{"id":"https://openalex.org/A2","orcid":"https://orcid.org/0000-0001-5109-3700","display_name":"Wei Wang"}`), nil
		}
		return httpResponse(http.StatusNotFound, ""), nil
	}))
	_, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{OpenAlexID: "A2", ORCID: "0000-0002-1825-0097"})
	if toolErr == nil || toolErr.Code != CodeIdentifierConflict {
		t.Fatalf("toolErr = %+v, want identifier_conflict", toolErr)
	}
}

func TestGetAuthorInfo_InvertedNameIsOnePerson(t *testing.T) {
	var searched []string
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/authors" {
			searched = append(searched, req.URL.Query().Get("search"))
			return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/A7","display_name":"Ying Li","works_count":3,"cited_by_count":9}]}`), nil
		}
		return httpResponse(http.StatusOK, `{"results":[]}`), nil
	}))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Li, Ying"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if len(searched) != 1 || searched[0] != "Ying Li" {
		t.Fatalf("searched %q, want one search for %q", searched, "Ying Li")
	}
	if author.Name != "Ying Li" || len(author.Warnings) != 0 {
		t.Fatalf("author = %q warnings = %v", author.Name, author.Warnings)
	}
}

func TestGetAuthorInfo_OpenAlexMetricsAreAuthorLevel(t *testing.T) {
	requester := newTestRequester(openAlexAuthorTransport(t, nil))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Wei Wang", Affiliation: "UCLA, University of California, Los Angeles"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.CitedBy == nil || *author.CitedBy != 50000 {
		t.Fatalf("citedby = %v, want the profile total 50000 (not the sampled work's 1234)", author.CitedBy)
	}
	if author.Metrics.Scope != "author_profile" || author.Metrics.HIndex == nil || *author.Metrics.HIndex != 90 {
		t.Fatalf("metrics = %+v", author.Metrics)
	}
	if author.PublicationSample == nil || author.PublicationSample.Scope != "top_cited_works" {
		t.Fatalf("publication sample = %+v", author.PublicationSample)
	}
	if len(author.Publications) != 1 || author.Publications[0].Citations != 1234 {
		t.Fatalf("publications = %+v", author.Publications)
	}
}

func TestGetAuthorInfo_CrossrefFallbackHasNoAuthorMetrics(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "api.openalex.org":
			return httpResponse(http.StatusOK, `{"results":[]}`), nil
		case "pub.orcid.org":
			return httpResponse(http.StatusOK, "orcid,given-names,family-name,current-institution-affiliation-name\n"), nil
		case "scholar.google.com":
			return httpResponse(http.StatusOK, "<html><body>no profiles</body></html>"), nil
		case "api.crossref.org":
			return httpResponse(http.StatusOK, `{"message":{"items":[
			  {"DOI":"10.1/a","title":["Highly cited"],"is-referenced-by-count":9000,"author":[{"given":"Rare","family":"Name"}],"issued":{"date-parts":[[2019]]}},
			  {"DOI":"10.1/b","title":["Other person"],"is-referenced-by-count":50,"author":[{"given":"Someone","family":"Else"}]}
			]}}`), nil
		}
		return httpResponse(http.StatusNotFound, ""), nil
	}))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Rare Name"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.Source != "crossref" {
		t.Fatalf("source = %q", author.Source)
	}
	if author.CitedBy != nil || author.Metrics.CitedByCount != nil {
		t.Fatalf("crossref fallback must not report author citations; got citedby=%v metrics=%+v", author.CitedBy, author.Metrics)
	}
	if len(author.Publications) != 1 || author.Publications[0].Title != "Highly cited" {
		t.Fatalf("publications = %+v, want only the name-matched work", author.Publications)
	}
	if author.PublicationSample == nil || author.PublicationSample.Scope != "name_matched_works_unverified" || author.Match.Confidence != "low" {
		t.Fatalf("sample = %+v match = %+v", author.PublicationSample, author.Match)
	}

	raw, err := json.Marshal(author)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"citedby"`) {
		t.Fatalf("unknown citedby should be omitted: %s", raw)
	}
}

// Every author path reports external_ids in the URL form main returned.
func TestGetAuthorInfo_ORCIDSearchExternalIDsAreURLs(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "api.openalex.org":
			return httpResponse(http.StatusOK, `{"results":[]}`), nil
		case "pub.orcid.org":
			return httpResponse(http.StatusOK, "orcid,given-names,family-name,current-institution-affiliation-name\n0000-0002-1825-0097,Rare,Name,Example University\n"), nil
		}
		t.Errorf("unexpected request %s", req.URL)
		return httpResponse(http.StatusNotFound, ""), nil
	}))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Rare Name"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.Source != "orcid" || author.ExternalIDs["orcid"] != "https://orcid.org/0000-0002-1825-0097" {
		t.Fatalf("source = %q external_ids = %v", author.Source, author.ExternalIDs)
	}
}

func TestGetAuthorInfo_ScholarFallbackExternalIDIsURL(t *testing.T) {
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "api.openalex.org":
			return httpResponse(http.StatusOK, `{"results":[]}`), nil
		case req.URL.Host == "pub.orcid.org":
			return httpResponse(http.StatusOK, "orcid,given-names,family-name,current-institution-affiliation-name\n"), nil
		case req.URL.Host == "scholar.google.com" && req.URL.Query().Get("view_op") == "search_authors":
			return httpResponse(http.StatusOK, `<html><body><div class="gsc_1usr"><h3 class="gs_ai_name"><a href="/citations?hl=en&amp;user=abc123XYZ">Rare Name</a></h3><div class="gs_ai_aff">Example University</div></div></body></html>`), nil
		case req.URL.Host == "scholar.google.com":
			return httpResponse(http.StatusNotFound, ""), nil
		}
		t.Errorf("unexpected request %s", req.URL)
		return httpResponse(http.StatusNotFound, ""), nil
	}))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Rare Name"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	raw, err := json.Marshal(author)
	if err != nil {
		t.Fatal(err)
	}
	if author.Source != "google_scholar" || !strings.Contains(string(raw), `"external_ids":{"google_scholar":"https://scholar.google.com/citations?user=abc123XYZ"}`) {
		t.Fatalf("source = %q, serialized = %s", author.Source, raw)
	}
}

func authorSearchTransport(t *testing.T, authors string) roundTripperFunc {
	return func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "api.openalex.org" && req.URL.Path == "/authors":
			return httpResponse(http.StatusOK, authors), nil
		case req.URL.Host == "api.openalex.org" && req.URL.Path == "/works":
			return httpResponse(http.StatusOK, `{"results":[]}`), nil
		}
		t.Errorf("unexpected request %s", req.URL)
		return httpResponse(http.StatusNotFound, ""), nil
	}
}

// Live OpenAlex has a stray exact-name "Yoshua Bengio" profile beside the
// real one; a profile with 10x the citations and works of every other match
// is selected, with the stray offered as an alternative.
func TestGetAuthorInfo_DominantProfileIsSelected(t *testing.T) {
	requester := newTestRequester(authorSearchTransport(t, `{"results":[
	 {"id":"https://openalex.org/A5000000001","display_name":"Yoshua Bengio","works_count":5,"cited_by_count":0,
	  "last_known_institutions":[{"display_name":"Example Hospital"}]},
	 {"id":"https://openalex.org/A5086198262","orcid":"https://orcid.org/0000-0002-1825-0097","display_name":"Yoshua Bengio","works_count":1379,"cited_by_count":486966,
	  "last_known_institutions":[{"display_name":"Université de Montréal"}]}]}`))
	author, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "Yoshua Bengio"})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if author.ExternalIDs["openalex"] != "https://openalex.org/A5086198262" {
		t.Fatalf("resolved %v, want the dominant profile", author.ExternalIDs)
	}
	if author.Match.Method != "name+dominance" || author.Match.Confidence != "medium" {
		t.Fatalf("match = %+v", author.Match)
	}
	if len(author.Match.Alternatives) != 1 || author.Match.Alternatives[0].ID != "A5000000001" || author.Match.Alternatives[0].Affiliation != "Example Hospital" {
		t.Fatalf("alternatives = %+v", author.Match.Alternatives)
	}
	if len(author.Warnings) != 1 || !strings.Contains(author.Warnings[0], "A5000000001") {
		t.Fatalf("warnings = %v", author.Warnings)
	}
}

// Several established same-name researchers ("John Smith") stay ambiguous.
func TestGetAuthorInfo_ComparableProfilesStayAmbiguous(t *testing.T) {
	requester := newTestRequester(authorSearchTransport(t, `{"results":[
	 {"id":"https://openalex.org/A1","display_name":"John Smith","works_count":900,"cited_by_count":20000},
	 {"id":"https://openalex.org/A2","display_name":"John Smith","works_count":750,"cited_by_count":15000},
	 {"id":"https://openalex.org/A3","display_name":"John Smith","works_count":600,"cited_by_count":1500}]}`))
	_, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "John Smith"})
	if toolErr == nil || toolErr.Code != CodeAmbiguous || len(toolErr.Candidates) != 3 {
		t.Fatalf("toolErr = %+v, want ambiguous with all three profiles", toolErr)
	}
}

// Totals near math.MaxInt must not overflow the dominance check and let one
// of two tied profiles be selected.
func TestGetAuthorInfo_MaxIntTiedProfilesStayAmbiguous(t *testing.T) {
	maxCount := strconv.Itoa(math.MaxInt)
	requester := newTestRequester(authorSearchTransport(t, `{"results":[
	 {"id":"https://openalex.org/A1","display_name":"John Smith","works_count":`+maxCount+`,"cited_by_count":`+maxCount+`},
	 {"id":"https://openalex.org/A2","display_name":"John Smith","works_count":`+maxCount+`,"cited_by_count":`+maxCount+`}]}`))
	_, toolErr := GetAuthorInfo(context.Background(), requester, AuthorQuery{Name: "John Smith"})
	if toolErr == nil || toolErr.Code != CodeAmbiguous || len(toolErr.Candidates) != 2 {
		t.Fatalf("toolErr = %+v, want ambiguous with both profiles", toolErr)
	}
}

// The 10x threshold is inclusive, holds against zero, and never overflows.
func TestDominates(t *testing.T) {
	for _, c := range []struct {
		top, other int
		want       bool
	}{
		{100, 10, true},
		{99, 10, false},
		{100, 11, false},
		{5, 0, true},
		{0, 0, true},
		{math.MaxInt, math.MaxInt, false},
		{math.MaxInt, math.MaxInt / DominanceRatio, true},
		{math.MaxInt, math.MaxInt/DominanceRatio + 1, false},
	} {
		if got := Dominates(c.top, c.other); got != c.want {
			t.Errorf("Dominates(%d, %d) = %v, want %v", c.top, c.other, got, c.want)
		}
	}
}
