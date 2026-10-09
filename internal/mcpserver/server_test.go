package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func testConfig() config.Config {
	return config.Config{
		MaxRetries:       1,
		UserAgents:       []string{"test"},
		Timeout:          2 * time.Second,
		OperationTimeout: 5 * time.Second,
		SearchProviders:  []string{config.ProviderOpenAlex},
		ToolSet:          config.ToolSetAll,
	}
}

func connect(t *testing.T, cfg config.Config, rt http.RoundTripper) *mcp.ClientSession {
	t.Helper()
	srv := newServer(cfg, "test", scholar.NewRequesterWithTransport(cfg, rt))
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCPServer().Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured content is not an object: %s", raw)
	}
	return res, out
}

func noNetwork(t *testing.T) roundTripperFunc {
	return func(req *http.Request) (*http.Response, error) {
		t.Errorf("unexpected network call: %s", req.URL)
		return jsonResponse(http.StatusNotFound, "{}"), nil
	}
}

func TestToolSetControlsCatalog(t *testing.T) {
	for _, c := range []struct {
		set  string
		want []string
	}{
		{config.ToolSetAll, ToolNames(config.ToolSetAll)},
		{config.ToolSetPreferred, []string{"get_researcher_info", "read_research_paper", "researcher_mcp_healthcheck", "search_research_articles", "search_research_articles_advanced"}},
		{config.ToolSetLegacy, []string{"get_author_info", "get_paper_fulltext", "google_scholar_healthcheck", "search_google_scholar_advanced", "search_google_scholar_key_words"}},
	} {
		cfg := testConfig()
		cfg.ToolSet = c.set
		session := connect(t, cfg, noNetwork(t))
		res, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, tool := range res.Tools {
			got = append(got, tool.Name)
		}
		sort.Strings(got)
		want := append([]string{}, c.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: tools = %v, want %v", c.set, got, want)
		}
	}
	if len(ToolNames(config.ToolSetAll)) != 10 {
		t.Fatal("default catalog must keep all 10 existing tool names")
	}
}

func TestHealthcheckMakesNoNetworkCalls(t *testing.T) {
	cfg := testConfig()
	cfg.OpenAlexAPIKey = "secret-key"
	session := connect(t, cfg, noNetwork(t))
	res, out := call(t, session, "researcher_mcp_healthcheck", nil)
	if res.IsError {
		t.Fatal("healthcheck reported an error")
	}
	if out["status"] != "ok" {
		t.Fatalf("status = %v", out["status"])
	}
	caps := out["capabilities"].(map[string]any)
	providers := caps["providers"].(map[string]any)
	if providers["openalex"].(map[string]any)["credentials"] != "api_key" || providers["unpaywall"].(map[string]any)["credentials"] != "disabled" {
		t.Fatalf("providers = %v", providers)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-key") {
		t.Fatal("healthcheck leaks a credential")
	}
	if _, ok := out["process"].(map[string]any)["uptime_seconds"]; !ok {
		t.Fatalf("process = %v", out["process"])
	}
}

func TestErrorsSetIsErrorAndKeepStructuredError(t *testing.T) {
	session := connect(t, testConfig(), noNetwork(t))
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"search_research_articles", map[string]any{"query": "  "}},
		{"get_researcher_info", map[string]any{}},
		{"read_research_paper", map[string]any{}},
		{"read_research_paper", map[string]any{"doi": "not-a-doi"}},
		{"get_author_info", map[string]any{"orcid": "0000-0002-1825-0098"}},
	} {
		res, out := call(t, session, c.tool, c.args)
		if !res.IsError {
			t.Errorf("%s %v: IsError = false", c.tool, c.args)
		}
		errObj, ok := out["error"].(map[string]any)
		if !ok || errObj["code"] != "invalid_input" {
			t.Errorf("%s %v: structured error = %v", c.tool, c.args, out)
		}
	}
}

func TestSearchReportsProvider(t *testing.T) {
	session := connect(t, testConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/W1","display_name":"A Paper","doi":"https://doi.org/10.1/x","publication_year":2020}]}`), nil
	}))
	res, out := call(t, session, "search_research_articles", map[string]any{"query": "a paper"})
	if res.IsError {
		t.Fatalf("unexpected error: %v", out)
	}
	if out["provider"] != "openalex" {
		t.Fatalf("provider = %v", out["provider"])
	}
	r := out["results"].([]any)[0].(map[string]any)
	if r["source"] != "openalex" || r["openalex_id"] != "W1" {
		t.Fatalf("result = %v", r)
	}
}

// Two comparably established profiles: neither dominates the other, so the
// name alone cannot pick one.
func TestAmbiguousAuthorIsAnErrorWithCandidates(t *testing.T) {
	session := connect(t, testConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"results":[
		  {"id":"https://openalex.org/A1","display_name":"Wei Wang","works_count":900,"cited_by_count":50000},
		  {"id":"https://openalex.org/A2","display_name":"Wei Wang","works_count":600,"cited_by_count":30000}]}`), nil
	}))
	res, out := call(t, session, "get_researcher_info", map[string]any{"author_name": "Wei Wang"})
	if !res.IsError {
		t.Fatal("ambiguity must be reported as an MCP error")
	}
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "ambiguous" || len(errObj["candidates"].([]any)) != 2 {
		t.Fatalf("error = %v", errObj)
	}
}

// A rate limit whose Retry-After is too long to wait out reaches the client
// as a structured error carrying the wait, for both search and author tools.
// Each tool gets a fresh server: the first 429 defers the host, so a second
// call on the same requester would fail on the deferral instead.
func TestRateLimitErrorsCarryRetryAfter(t *testing.T) {
	limited := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp := jsonResponse(http.StatusTooManyRequests, `{}`)
		resp.Header.Set("Retry-After", "120")
		return resp, nil
	})
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"search_research_articles", map[string]any{"query": "graph methods"}},
		{"get_researcher_info", map[string]any{"author_name": "Wei Wang"}},
	} {
		res, out := call(t, connect(t, testConfig(), limited), c.tool, c.args)
		if !res.IsError {
			t.Fatalf("%s: IsError = false: %v", c.tool, out)
		}
		errObj, _ := out["error"].(map[string]any)
		if errObj["code"] != "blocked" || errObj["retry_after_seconds"] != float64(120) || errObj["retryable"] != true {
			t.Fatalf("%s: error = %v", c.tool, errObj)
		}
	}
}

func TestPaperContentContract(t *testing.T) {
	body := `<html><body><article><h1>T</h1>` +
		strings.Repeat(`<h2>Introduction</h2><p>intro text that is long enough to count as content in tests.</p>`, 1) +
		`<h2>Methods</h2><p>` + strings.Repeat("method words ", 80) + `</p><h2>Results</h2><p>` + strings.Repeat("result words ", 80) + `</p><h2>References</h2><p>[1] x</p></article></body></html>`
	var fetches atomic.Int32
	session := connect(t, testConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		fetches.Add(1)
		resp := jsonResponse(http.StatusOK, body)
		resp.Header.Set("Content-Type", "text/html")
		return resp, nil
	}))
	res, out := call(t, session, "read_research_paper", map[string]any{"url": "https://example.org/paper", "max_chars": 500})
	if res.IsError {
		t.Fatalf("unexpected error: %v", out)
	}
	content := out["content"].(map[string]any)
	for _, key := range []string{"document_id", "content_id", "content_status", "markdown", "total_chars", "next_offset", "provenance", "identity"} {
		if _, ok := content[key]; !ok {
			t.Errorf("content missing %q: %v", key, content)
		}
	}
	if content["content_status"] != "full_text" || content["document_id"] != "url:example.org/paper" {
		t.Fatalf("content = %v", content)
	}
}

func TestAuthorWithUnknownMetricsPassesOutputSchema(t *testing.T) {
	session := connect(t, testConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/authors" {
			return jsonResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/A9","display_name":"Solo Researcher"}]}`), nil
		}
		return jsonResponse(http.StatusOK, `{"results":[]}`), nil
	}))
	res, out := call(t, session, "get_researcher_info", map[string]any{"author_name": "Solo Researcher"})
	if res.IsError {
		t.Fatalf("unexpected error: %v", out)
	}
	author := out["author"].(map[string]any)
	if v, ok := author["citedby"]; ok {
		t.Fatalf("citedby = %v, want it omitted when unknown", v)
	}
	metrics := author["metrics"].(map[string]any)
	if metrics["cited_by_count"] != nil || metrics["source"] != "openalex" {
		t.Fatalf("metrics = %v", metrics)
	}
}
