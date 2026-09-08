package scholar

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestParseSearchResultsHTML(t *testing.T) {
	html := []byte(`
<div class="gs_ri">
  <h3 class="gs_rt"><a href="https://example.org/p1">Paper One</a></h3>
  <div class="gs_a">A Author, B Author - 2024</div>
  <div class="gs_rs">This is an abstract snippet...</div>
</div>
<div class="gs_ri">
  <h3 class="gs_rt">Paper Two</h3>
  <div class="gs_a">C Author - 2023</div>
  <div class="gs_rs">Second abstract</div>
</div>
`)

	results, err := parseSearchResultsHTML(html, 5)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Title != "Paper One" {
		t.Fatalf("unexpected title: %s", results[0].Title)
	}
	if !results[0].SnippetTruncated {
		t.Fatalf("expected snippet_truncated=true for first record")
	}
	if results[1].URL != "No link available" {
		t.Fatalf("expected fallback URL for second record, got: %s", results[1].URL)
	}
}

func TestQuotedTitleBatchQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
		ok    bool
	}{
		{
			name:  "multiple quoted titles",
			query: `"Who Gets to Define Safety" "Rethinking Interdependence" "Leveraging Affordances as a Lens"`,
			want:  `"Who Gets to Define Safety" OR "Rethinking Interdependence" OR "Leveraging Affordances as a Lens"`,
			ok:    true,
		},
		{
			name:  "whitespace separated titles",
			query: " \t\"First Title\"\n \"Second Title\" ",
			want:  `"First Title" OR "Second Title"`,
			ok:    true,
		},
		{name: "single phrase", query: `"Only One"`},
		{name: "explicit boolean operator", query: `"First Title" OR "Second Title"`},
		{name: "mixed text", query: `prefix "First Title" "Second Title"`},
		{name: "trailing mixed text", query: `"First Title" "Second Title" suffix`},
		{name: "parentheses", query: `("First Title") "Second Title"`},
		{name: "missing closing quote", query: `"First Title" "Second Title`},
		{name: "empty phrase", query: `"First Title" ""`},
		{name: "missing separator", query: `"First Title""Second Title"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := quotedTitleBatchQuery(tt.query)
			if ok != tt.ok {
				t.Fatalf("quotedTitleBatchQuery(%q) ok = %t, want %t", tt.query, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("quotedTitleBatchQuery(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestSearchAdvanced_QuotedTitleBatchBypassesScholar(t *testing.T) {
	var scholarRequests, openAlexRequests int
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "scholar.google.com":
			scholarRequests++
			return httpResponse(http.StatusOK, ""), nil
		case "api.openalex.org":
			openAlexRequests++
			if got, want := req.URL.Query().Get("search"), `"First Title" OR "Second Title"`; got != want {
				t.Errorf("OpenAlex search = %q, want %q", got, want)
			}
			if got, want := req.URL.Query().Get("per-page"), "5"; got != want {
				t.Errorf("OpenAlex per-page = %q, want %q", got, want)
			}
			if got, want := req.URL.Query().Get("filter"), "authorships.author.display_name.search:Ada Lovelace,from_publication_date:2020-01-01,to_publication_date:2025-12-31"; got != want {
				t.Errorf("OpenAlex filter = %q, want %q", got, want)
			}
			return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/W1","display_name":"Batch result"}]}`), nil
		default:
			t.Fatalf("unexpected request host: %s", req.URL.Host)
			return nil, nil
		}
	}))

	results, toolErr := SearchAdvanced(context.Background(), requester, `"First Title" "Second Title"`, "Ada Lovelace", []int{2025, 2020}, 5)
	if toolErr != nil {
		t.Fatalf("SearchAdvanced returned tool error: %+v", toolErr)
	}
	if len(results) != 1 || results[0].Title != "Batch result" {
		t.Fatalf("unexpected results: %+v", results)
	}
	if scholarRequests != 0 {
		t.Fatalf("Scholar requests = %d, want 0", scholarRequests)
	}
	if openAlexRequests != 1 {
		t.Fatalf("OpenAlex requests = %d, want 1", openAlexRequests)
	}
}

func TestSearchByKeywords_BooleanQueryRemainsScholarSearch(t *testing.T) {
	query := `"First Title" OR "Second Title"`
	var scholarRequests, openAlexRequests int
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "scholar.google.com":
			scholarRequests++
			if got := req.URL.Query().Get("q"); got != query {
				t.Errorf("Scholar query = %q, want %q", got, query)
			}
			return httpResponse(http.StatusOK, `<div class="gs_ri"><h3 class="gs_rt"><a href="https://example.org/p1">Paper</a></h3><div class="gs_a">Author</div><div class="gs_rs">Abstract</div></div>`), nil
		case "api.openalex.org":
			openAlexRequests++
			return httpResponse(http.StatusOK, ""), nil
		default:
			t.Fatalf("unexpected request host: %s", req.URL.Host)
			return nil, nil
		}
	}))

	results, toolErr := SearchByKeywords(context.Background(), requester, query, 5)
	if toolErr != nil {
		t.Fatalf("SearchByKeywords returned tool error: %+v", toolErr)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if scholarRequests != 1 {
		t.Fatalf("Scholar requests = %d, want 1", scholarRequests)
	}
	if openAlexRequests != 0 {
		t.Fatalf("OpenAlex requests = %d, want 0", openAlexRequests)
	}
}

func TestSearchByKeywords_BlockedScholarFallsBackWithoutRetry(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
	}{
		{name: "forbidden", status: http.StatusForbidden},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var scholarRequests, openAlexRequests int
			requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Host {
				case "scholar.google.com":
					scholarRequests++
					return httpResponse(tt.status, "blocked"), nil
				case "api.openalex.org":
					openAlexRequests++
					return httpResponse(http.StatusOK, `{"results":[{"id":"https://openalex.org/W1","display_name":"Fallback result"}]}`), nil
				default:
					t.Fatalf("unexpected request host: %s", req.URL.Host)
					return nil, nil
				}
			}))
			requester.cfg.MaxRetries = 3
			requester.cfg.BackoffFactor = 0

			results, toolErr := SearchByKeywords(context.Background(), requester, "ordinary query", 5)
			if toolErr != nil {
				t.Fatalf("SearchByKeywords returned tool error: %+v", toolErr)
			}
			if len(results) != 1 || results[0].Title != "Fallback result" {
				t.Fatalf("unexpected results: %+v", results)
			}
			if scholarRequests != 1 {
				t.Fatalf("Scholar requests = %d, want 1", scholarRequests)
			}
			if openAlexRequests != 1 {
				t.Fatalf("OpenAlex requests = %d, want 1", openAlexRequests)
			}
		})
	}
}

func TestRequester_RetriesBlockedStatusForOtherHosts(t *testing.T) {
	requests := 0
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.openalex.org" {
			t.Fatalf("unexpected request host: %s", req.URL.Host)
		}
		requests++
		if requests == 1 {
			return httpResponse(http.StatusServiceUnavailable, "retry"), nil
		}
		return httpResponse(http.StatusOK, "ok"), nil
	}))
	requester.cfg.MaxRetries = 2
	requester.cfg.BackoffFactor = 0

	body, status, err := requester.Get(context.Background(), "https://api.openalex.org/works")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("Get = (%q, %d), want (ok, 200)", body, status)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestRequesterWaitsRespectCanceledContext(t *testing.T) {
	requester := newTestRequester(nil)
	requester.cfg.MinDelay = time.Second
	requester.cfg.MaxDelay = time.Second
	original := time.Now()
	requester.lastByHost["example.org"] = original

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := requester.sleepForRateLimit(ctx, "https://example.org/works"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sleepForRateLimit error = %v, want context.DeadlineExceeded", err)
	}
	if got := requester.lastByHost["example.org"]; !got.Equal(original) {
		t.Fatalf("last request time = %v, want cancellation rollback to %v", got, original)
	}
	if err := requester.sleepForBackoff(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sleepForBackoff error = %v, want context.DeadlineExceeded", err)
	}
}

func TestRequesterRateLimitQueueRespectsCanceledContext(t *testing.T) {
	requester := newTestRequester(nil)
	gate := requester.rateLimitGate("example.org")
	<-gate
	defer func() { gate <- struct{}{} }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := requester.sleepForRateLimit(ctx, "https://example.org/works"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sleepForRateLimit error = %v, want context.DeadlineExceeded", err)
	}
}
