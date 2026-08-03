package fulltext

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"googlescholar-mcp-go/internal/scholar"
)

// fakeFetcher serves canned responses keyed by URL substring.
type fakeFetcher struct {
	responses map[string]*scholar.FetchedDoc
	errs      map[string]error
	calls     []string
}

func (f *fakeFetcher) GetDocument(_ context.Context, rawURL string) (*scholar.FetchedDoc, error) {
	f.calls = append(f.calls, rawURL)
	for key, err := range f.errs {
		if strings.Contains(rawURL, key) {
			return nil, err
		}
	}
	for key, doc := range f.responses {
		if strings.Contains(rawURL, key) {
			out := *doc
			if out.FinalURL == "" {
				out.FinalURL = rawURL
			}
			return &out, nil
		}
	}
	return &scholar.FetchedDoc{Status: http.StatusNotFound, FinalURL: rawURL}, nil
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
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
		{"doi:10.1109/CVPR.2016.90", "10.1109/CVPR.2016.90", true},
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

func TestResolveArxivIDProducesHTMLFirstChain(t *testing.T) {
	res, toolErr := resolve(context.Background(), &fakeFetcher{}, "", Request{ArxivID: "1706.03762"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	want := []string{
		"https://arxiv.org/html/1706.03762",
		"https://ar5iv.labs.arxiv.org/html/1706.03762",
		"https://arxiv.org/pdf/1706.03762",
	}
	if len(res.Candidates) != len(want) {
		t.Fatalf("candidates = %v, want %v", res.Candidates, want)
	}
	for i := range want {
		if res.Candidates[i] != want[i] {
			t.Fatalf("candidates[%d] = %q, want %q", i, res.Candidates[i], want[i])
		}
	}
}

func TestResolveDOIOrdersOpenAlexLocations(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work.json")},
	}}

	res, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.1109/cvpr.2016.90"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Title != "Deep Residual Learning for Image Recognition" {
		t.Fatalf("title = %q", res.Title)
	}
	want := []string{
		"https://publisher.example.org/article/123.pdf",
		"https://repo.example.org/oa/123.pdf",
		"https://publisher.example.org/article/123",
	}
	if len(res.Candidates) != len(want) {
		t.Fatalf("candidates = %v, want %v", res.Candidates, want)
	}
	for i := range want {
		if res.Candidates[i] != want[i] {
			t.Fatalf("candidates[%d] = %q, want %q", i, res.Candidates[i], want[i])
		}
	}
}

func TestResolveDOIRoutesArxivWorksToArxivChain(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 200, Body: loadFixture(t, "openalex_work_arxiv.json")},
	}}

	res, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.48550/arxiv.1706.03762"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Candidates[0] != "https://arxiv.org/html/1706.03762" {
		t.Fatalf("candidates[0] = %q, want arXiv HTML first", res.Candidates[0])
	}
}

func TestResolveDOIUnknownReturnsNoResults(t *testing.T) {
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works/": {Status: 404, Body: []byte(`{"error":"not found"}`)},
	}}

	_, toolErr := resolve(context.Background(), fetcher, "", Request{DOI: "10.9999/does.not.exist"})
	if toolErr == nil || toolErr.Code != "no_results" {
		t.Fatalf("toolErr = %+v, want no_results", toolErr)
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
	if len(res.Candidates) == 0 || !strings.HasSuffix(res.Candidates[0], "123.pdf") {
		t.Fatalf("candidates = %v", res.Candidates)
	}
}

func TestResolveTitleUsesOpenAlexSearch(t *testing.T) {
	body := []byte(`{"results":[` + string(loadFixture(t, "openalex_work.json")) + `]}`)
	fetcher := &fakeFetcher{responses: map[string]*scholar.FetchedDoc{
		"api.openalex.org/works?": {Status: 200, Body: body},
	}}

	res, toolErr := resolve(context.Background(), fetcher, "", Request{Title: "Deep Residual Learning"})
	if toolErr != nil {
		t.Fatalf("resolve error: %+v", toolErr)
	}
	if res.Title != "Deep Residual Learning for Image Recognition" {
		t.Fatalf("title = %q", res.Title)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %v", res.Candidates)
	}
}

func TestResolveInvalidInputs(t *testing.T) {
	for _, req := range []Request{
		{},
		{ArxivID: "banana"},
		{DOI: "not-a-doi"},
		{URL: "ftp://example.org/x"},
	} {
		_, toolErr := resolve(context.Background(), &fakeFetcher{}, "", req)
		if toolErr == nil || toolErr.Code != "invalid_input" {
			t.Errorf("resolve(%+v) toolErr = %+v, want invalid_input", req, toolErr)
		}
	}
}
