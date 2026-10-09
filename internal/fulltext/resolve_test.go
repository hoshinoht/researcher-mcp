package fulltext

import (
	"context"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"
)

// fakeFetcher serves canned responses keyed by URL substring; the longest
// matching key wins so overlapping keys are deterministic.
type fakeFetcher struct {
	mu        sync.Mutex
	responses map[string]*scholar.FetchedDoc
	errs      map[string]error
	calls     []string
	// delay is applied to every fetch (honouring context cancellation).
	delay time.Duration
}

func longestMatch[T any](m map[string]T, rawURL string) (T, bool) {
	var (
		best    T
		bestLen = -1
	)
	for key, v := range m {
		if strings.Contains(rawURL, key) && len(key) > bestLen {
			best, bestLen = v, len(key)
		}
	}
	return best, bestLen >= 0
}

func (f *fakeFetcher) GetDocument(ctx context.Context, rawURL string) (*scholar.FetchedDoc, error) {
	f.mu.Lock()
	f.calls = append(f.calls, rawURL)
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err, ok := longestMatch(f.errs, rawURL); ok {
		return nil, err
	}
	if doc, ok := longestMatch(f.responses, rawURL); ok {
		out := *doc
		if out.FinalURL == "" {
			out.FinalURL = rawURL
		}
		return &out, nil
	}
	return &scholar.FetchedDoc{Status: 404, Body: []byte("not found"), FinalURL: rawURL}, nil
}

// GetJSON serves the same canned responses; the size-cap split between the
// two methods is covered against a real Requester.
func (f *fakeFetcher) GetJSON(ctx context.Context, rawURL string) (*scholar.FetchedDoc, error) {
	return f.GetDocument(ctx, rawURL)
}

func (f *fakeFetcher) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeFetcher) called(substr string) bool {
	for _, c := range f.callsSnapshot() {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func candidateURLs(res *resolution) []string {
	out := []string{}
	for _, c := range res.sortedCandidates() {
		out = append(out, c.URL)
	}
	return out
}

func TestNormalizeArxivID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1706.03762", "1706.03762", true},
		{"2401.00001v2", "2401.00001v2", true},
		{"arXiv:1706.03762", "1706.03762", true},
		{"cs/9901001", "cs/9901001", true},
		{"math.GT/0309136", "math.GT/0309136", true},
		{"not-an-id", "", false},
		{"10.1038/nature14539", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeArxivID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeArxivID(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeDOI(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"10.1038/nature14539", "10.1038/nature14539", true},
		{"https://doi.org/10.1038/nature14539", "10.1038/nature14539", true},
		{"doi:10.1109/CVPR.2016.90", "10.1109/cvpr.2016.90", true},
		{"nature14539", "", false},
		{"10.1038/", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeDOI(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeDOI(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestArxivIDFromURLPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/abs/1706.03762", "1706.03762", true},
		{"/pdf/1706.03762v5.pdf", "1706.03762v5", true},
		{"/html/2401.00001", "2401.00001", true},
		{"/list/cs.AI/recent", "", false},
	}
	for _, c := range cases {
		got, ok := arxivIDFromURLPath(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("arxivIDFromURLPath(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveArxivIDCandidates(t *testing.T) {
	f := &fakeFetcher{}
	res, toolErr := resolve(context.Background(), f, "", Request{ArxivID: "1706.03762"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	got := candidateURLs(res)
	want := arxivCandidates("1706.03762")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if len(f.callsSnapshot()) != 0 {
		t.Fatalf("arXiv-only requests should not spend an OpenAlex call: %v", f.callsSnapshot())
	}
	if res.Identity.Match.Method != "arxiv_id" {
		t.Fatalf("match = %+v", res.Identity.Match)
	}
}

func TestResolveDOIUsesOpenAlexLocations(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work.json")},
	}}

	res, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.1109/CVPR.2016.90"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Identity.Title != "Deep Residual Learning for Image Recognition" || res.Identity.DOI != "10.1109/cvpr.2016.90" {
		t.Fatalf("identity = %+v", res.Identity)
	}
	want := []string{
		"https://publisher.example.org/article/123.pdf",
		"https://repo.example.org/oa/123.pdf",
		"https://publisher.example.org/article/123",
		"https://doi.org/10.1109/cvpr.2016.90",
	}
	if got := candidateURLs(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestResolveDOIKeepsAlternativesBesideArxiv(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work_arxiv_and_repo.json")},
	}}
	res, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.1000/robust"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	got := candidateURLs(res)
	want := []string{
		"https://arxiv.org/html/2001.00001",
		"https://ar5iv.labs.arxiv.org/html/2001.00001",
		"https://arxiv.org/pdf/2001.00001",
		"https://repository.example.edu/bitstream/30/robust.pdf",
		"https://repository.example.edu/item/30",
		"https://doi.org/10.1000/robust",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates = %v\nwant %v", got, want)
	}
	if res.Identity.ArxivID != "2001.00001" || res.Identity.OpenAlexID != "W30" {
		t.Fatalf("identity = %+v", res.Identity)
	}
}

func TestResolveDOIUnknownStillTriesDOILanding(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 404, Body: []byte(`{"error":"not found"}`)},
	}}
	res, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.9999/does.not.exist"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if got := candidateURLs(res); len(got) != 1 || got[0] != "https://doi.org/10.9999/does.not.exist" {
		t.Fatalf("candidates = %v", got)
	}
}

func TestResolveDOIURLIsTreatedAsDOI(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work.json")},
	}}

	res, toolErr := resolve(context.Background(), fetcher, "", Request{URL: "https://doi.org/10.1109/cvpr.2016.90"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if got := candidateURLs(res); len(got) == 0 || !strings.HasSuffix(got[0], "123.pdf") {
		t.Fatalf("candidates = %v", got)
	}
}

func TestResolveTitleRejectsWrongFirstResult(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works?": {Status: 200, Body: loadFixture(t, "openalex_title_wrong_first.json")},
	}}
	res, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Attention Is All You Need"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Identity.OpenAlexID != "W11" || res.Identity.ArxivID != "1706.03762" {
		t.Fatalf("identity = %+v; the first (wrong) search hit must not win", res.Identity)
	}
	if res.Identity.Match.Method != "title_exact" || res.Identity.Match.Confidence != "medium" {
		t.Fatalf("match = %+v", res.Identity.Match)
	}
	urls := candidateURLs(res)
	for _, u := range urls {
		if strings.Contains(u, "wrong.example.org") {
			t.Fatalf("candidates include the wrong paper: %v", urls)
		}
	}
	// The second version record (same title, same author) contributes its
	// location as an alternative.
	if !strings.Contains(strings.Join(urls, ","), "papers.example.org/nips2017.pdf") {
		t.Fatalf("version location missing: %v", urls)
	}
}

func TestResolveTitleAmbiguousWithoutEvidence(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works?": {Status: 200, Body: loadFixture(t, "openalex_title_two_papers.json")},
	}}
	_, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Deep learning"})
	if toolErr == nil || toolErr.Code != scholar.CodeAmbiguous {
		t.Fatalf("toolErr = %+v, want ambiguous", toolErr)
	}
	if len(toolErr.Candidates) != 2 || toolErr.Candidates[0].DOI == "" {
		t.Fatalf("candidates = %+v", toolErr.Candidates)
	}

	res, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Deep learning", Author: "Goodfellow"})
	if toolErr != nil {
		t.Fatalf("with author evidence: %+v", toolErr)
	}
	if res.Identity.DOI != "10.5555/goodfellow" || res.Identity.Match.Confidence != "high" {
		t.Fatalf("identity = %+v", res.Identity)
	}

	res, toolErr = resolve(context.Background(), fetcher, "", Request{Title: "Deep learning", Year: 2015})
	if toolErr != nil || res.Identity.DOI != "10.1038/nature14539" {
		t.Fatalf("with year evidence: %+v %+v", res, toolErr)
	}
}

func TestResolveTitleNoCloseMatch(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works?": {Status: 200, Body: loadFixture(t, "openalex_title_two_papers.json")},
	}}
	_, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Quantum chromodynamics on the lattice"})
	if toolErr == nil || toolErr.Code != scholar.CodeNoResults {
		t.Fatalf("toolErr = %+v, want no_results", toolErr)
	}
}

func TestResolveTitleFallsBackToCrossrefDuringOpenAlexOutage(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works?": {Status: 503, Body: []byte("down")},
		"api.crossref.org/works?": {Status: 200, Body: []byte(`{"message":{"items":[{"DOI":"10.1000/XYZ","title":["Resilient Systems"],"author":[{"given":"Ann","family":"Smith"}],"issued":{"date-parts":[[2019]]}}]}}`)},
	}}
	res, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Resilient systems"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Identity.DOI != "10.1000/xyz" || len(res.Warnings) == 0 {
		t.Fatalf("identity = %+v warnings = %v", res.Identity, res.Warnings)
	}
}

func TestResolveDetectsConflictingIdentifiers(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work_arxiv_and_repo.json")},
	}}
	cases := []struct {
		name string
		req  Request
	}{
		{"doi vs arxiv location", Request{DOI: "10.1000/robust", ArxivID: "1706.03762"}},
		{"arxiv doi vs arxiv id", Request{DOI: "10.48550/arXiv.2001.00001", ArxivID: "1706.03762"}},
		{"doi url vs doi", Request{URL: "https://doi.org/10.1000/a", DOI: "10.1000/b"}},
		{"title vs doi", Request{DOI: "10.1000/robust", Title: "An entirely unrelated title about birds"}},
	}
	for _, c := range cases {
		_, toolErr := resolve(context.Background(), fetcher, "", c.req)
		if toolErr == nil || toolErr.Code != scholar.CodeIdentifierConflict {
			t.Errorf("%s: toolErr = %+v, want identifier_conflict", c.name, toolErr)
		}
	}
	// Matching identifiers are fine.
	if _, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.1000/robust", ArxivID: "2001.00001v2", Title: "Robust paper"}); toolErr != nil {
		t.Fatalf("consistent identifiers rejected: %+v", toolErr)
	}
}

func TestResolveInvalidInputs(t *testing.T) {
	for _, req := range []Request{
		{URL: "not a url"},
		{URL: "ftp://example.org/paper.pdf"},
		{DOI: "nope"},
		{ArxivID: "nope"},
		{OpenAlexID: "A123"},
		{},
	} {
		_, toolErr := resolve(context.Background(), &fakeFetcher{}, "", req)
		if toolErr == nil || toolErr.Code != scholar.CodeInvalidInput {
			t.Errorf("resolve(%+v) toolErr = %+v, want invalid_input", req, toolErr)
		}
	}
}

func titleSearchFetcher(body []byte) *fakeFetcher {
	return &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works?": {Status: 200, Body: body},
	}}
}

// Live OpenAlex lists the real "Attention Is All You Need" beside unrelated
// records with the exact title; the paper cited 10x more than every other
// same-title record is selected, with the others offered as alternatives.
func TestResolveTitleSelectsDominantPaper(t *testing.T) {
	fetcher := titleSearchFetcher(loadFixture(t, "openalex_title_attention_dominant.json"))
	res, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Attention Is All You Need"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	id := res.Identity
	if id.OpenAlexID != "W2626778328" || id.ArxivID != "1706.03762" {
		t.Fatalf("identity = %+v, want the dominant paper", id)
	}
	if id.Match.Confidence != "medium" || len(id.Match.Alternatives) != 2 {
		t.Fatalf("match = %+v", id.Match)
	}
	for _, alt := range id.Match.Alternatives {
		if alt.ID == "" || alt.Label == "" || alt.Year == 0 {
			t.Fatalf("alternative lacks id/title/year: %+v", alt)
		}
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "alternatives") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	for _, u := range candidateURLs(res) {
		if strings.Contains(u, "books.example.org") {
			t.Fatalf("candidates include an alternative paper's location: %v", candidateURLs(res))
		}
	}
}

// The CVPR and arXiv records of ResNet are versions of one paper. Neither
// record is 10x the other, so only after merging them does the paper dominate
// an unrelated same-title paper. The most-cited version supplies the identity
// and the arXiv version contributes its location.
func TestResolveTitleDominanceMergesVersionsFirst(t *testing.T) {
	fetcher := titleSearchFetcher(loadFixture(t, "openalex_title_resnet_versions.json"))
	res, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Deep Residual Learning for Image Recognition"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Identity.DOI != "10.1109/cvpr.2016.90" || res.Identity.OpenAlexID != "W2194775991" {
		t.Fatalf("identity = %+v", res.Identity)
	}
	if urls := strings.Join(candidateURLs(res), " "); !strings.Contains(urls, "arxiv.org/pdf/1512.03385") || strings.Contains(urls, "other.example.org") {
		t.Fatalf("candidates = %v, want the arXiv version and not the unrelated paper", candidateURLs(res))
	}
	if res.Identity.Match.Confidence != "medium" || len(res.Identity.Match.Alternatives) != 1 || res.Identity.Match.Alternatives[0].DOI != "10.9999/other.resnet" {
		t.Fatalf("match = %+v", res.Identity.Match)
	}

	// Identifying evidence still wins outright, without a dominance warning.
	res, toolErr = resolve(context.Background(), fetcher, "", Request{Title: "Deep Residual Learning for Image Recognition", Author: "Kaiming He", Year: 2016})
	if toolErr != nil || res.Identity.Match.Confidence != "high" || len(res.Identity.Match.Alternatives) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("with evidence: %+v %+v", res, toolErr)
	}
}

func TestResolveTitleComparablePapersStayAmbiguous(t *testing.T) {
	fetcher := titleSearchFetcher([]byte(`{"results":[
	 {"id":"https://openalex.org/W1","display_name":"Graph Methods","publication_year":2010,"cited_by_count":900,"authorships":[{"author":{"display_name":"Ann Lee"}}]},
	 {"id":"https://openalex.org/W2","display_name":"Graph Methods","publication_year":2019,"cited_by_count":200,"authorships":[{"author":{"display_name":"Bo Chen"}}]}]}`))
	_, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Graph Methods"})
	if toolErr == nil || toolErr.Code != scholar.CodeAmbiguous || len(toolErr.Candidates) != 2 {
		t.Fatalf("toolErr = %+v, want ambiguous with both papers", toolErr)
	}
}

// Citation counts near math.MaxInt must not overflow the dominance check
// and let one of two tied papers be selected.
func TestResolveTitleMaxIntTiedPapersStayAmbiguous(t *testing.T) {
	maxCites := strconv.Itoa(math.MaxInt)
	fetcher := titleSearchFetcher([]byte(`{"results":[
	 {"id":"https://openalex.org/W1","display_name":"Graph Methods","publication_year":2010,"cited_by_count":` + maxCites + `,"authorships":[{"author":{"display_name":"Ann Lee"}}]},
	 {"id":"https://openalex.org/W2","display_name":"Graph Methods","publication_year":2019,"cited_by_count":` + maxCites + `,"authorships":[{"author":{"display_name":"Bo Chen"}}]}]}`))
	_, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Graph Methods"})
	if toolErr == nil || toolErr.Code != scholar.CodeAmbiguous || len(toolErr.Candidates) != 2 {
		t.Fatalf("toolErr = %+v, want ambiguous with both papers", toolErr)
	}
}

// A same-title record without authors has nothing tying it to the other
// record, so the two must not be merged into one paper.
func TestResolveTitleDoesNotMergeAuthorlessRecords(t *testing.T) {
	fetcher := titleSearchFetcher([]byte(`{"results":[
	 {"id":"https://openalex.org/W1","display_name":"Graph Methods","publication_year":2018,"authorships":[{"author":{"display_name":"Ann Lee"}}]},
	 {"id":"https://openalex.org/W2","display_name":"Graph Methods","publication_year":2019,"authorships":[]}]}`))
	_, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Graph Methods"})
	if toolErr == nil || toolErr.Code != scholar.CodeAmbiguous {
		t.Fatalf("toolErr = %+v, want ambiguous", toolErr)
	}
}

// Metadata API responses are bounded by RESEARCHER_MAX_RESPONSE_MB, not the
// much larger document cap meant for PDFs and HTML pages.
func TestMetadataLookupUsesResponseSizeCap(t *testing.T) {
	work := `{"id":"https://openalex.org/W30","display_name":"Robust Paper"}` + strings.Repeat(" ", 4096)
	requester := scholar.NewRequesterWithTransport(config.Config{
		MaxRetries:       1,
		UserAgents:       []string{"test"},
		MaxResponseBytes: 1024,
		MaxFetchBytes:    1 << 20,
	}, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(work)), Request: req}, nil
	}))
	res, toolErr := resolve(context.Background(), requester, "", Request{DOI: "10.1000/robust"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if len(res.Attempts) == 0 || res.Attempts[0].Outcome != scholar.CodeUpstreamError || !strings.Contains(res.Attempts[0].Message, "size limit") {
		t.Fatalf("attempts = %+v, want the OpenAlex lookup rejected by the response size cap", res.Attempts)
	}
	if res.Identity.Title != "" {
		t.Fatalf("identity title = %q; the oversized metadata must not be parsed", res.Identity.Title)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
