package scholar

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
)

type recordedRequest struct {
	url     string
	headers http.Header
}

type recorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{url: req.URL.String(), headers: req.Header.Clone()})
}

func credentialConfig() config.Config {
	return config.Config{
		MaxRetries:        1,
		UserAgents:        []string{"test-agent"},
		Timeout:           2 * time.Second,
		OpenAlexAPIKey:    "oa-secret-key",
		CrossrefPlusToken: "cr-secret-token",
		ORCIDClientID:     "orcid-client",
		ORCIDClientSecret: "orcid-secret",
		ContactEmail:      "me@example.org",
	}
}

func TestCredentialsAreScopedToTheirProvider(t *testing.T) {
	rec := &recorder{}
	tokenRequests := 0
	requester := NewRequesterWithTransport(credentialConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "orcid.org" && req.URL.Path == "/oauth/token" {
			tokenRequests++
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if req.PostForm.Get("scope") != "/read-public" || req.PostForm.Get("grant_type") != "client_credentials" {
				t.Errorf("token form = %v", req.PostForm)
			}
			return httpResponse(http.StatusOK, `{"access_token":"orcid-access-token","token_type":"bearer"}`), nil
		}
		rec.add(req)
		return httpResponse(http.StatusOK, `{}`), nil
	}))

	ctx := context.Background()
	for _, u := range []string{
		"https://api.openalex.org/works?search=x",
		"https://api.crossref.org/works?query=x",
		"https://pub.orcid.org/v3.0/0000-0002-1825-0097/person",
		"https://pub.orcid.org/v3.0/csv-search/?q=x",
		"https://publisher.example.org/paper",
		"http://api.openalex.org/works", // plain HTTP never carries credentials
	} {
		if _, _, err := requester.Get(ctx, u); err != nil {
			t.Fatalf("Get(%s): %v", u, err)
		}
	}
	if tokenRequests != 1 {
		t.Fatalf("ORCID token requests = %d, want 1 (cached)", tokenRequests)
	}

	byHost := map[string][]recordedRequest{}
	for _, r := range rec.reqs {
		u, _ := url.Parse(r.url)
		byHost[u.Scheme+"://"+u.Host] = append(byHost[u.Scheme+"://"+u.Host], r)
	}
	oa := byHost["https://api.openalex.org"][0]
	if !strings.Contains(oa.url, "api_key=oa-secret-key") || oa.headers.Get("Crossref-Plus-API-Token") != "" || oa.headers.Get("Authorization") != "" {
		t.Fatalf("openalex request = %+v", oa)
	}
	if strings.Contains(oa.url, "mailto") {
		t.Fatalf("openalex no longer uses mailto; contact email leaked: %s", oa.url)
	}
	cr := byHost["https://api.crossref.org"][0]
	if cr.headers.Get("Crossref-Plus-API-Token") != "Bearer cr-secret-token" || strings.Contains(cr.url, "api_key") || !strings.Contains(cr.url, "mailto=me%40example.org") {
		t.Fatalf("crossref request = %+v", cr)
	}
	for _, r := range byHost["https://pub.orcid.org"] {
		if r.headers.Get("Authorization") != "Bearer orcid-access-token" {
			t.Fatalf("orcid request missing bearer token: %+v", r)
		}
	}
	for _, host := range []string{"https://publisher.example.org", "http://api.openalex.org"} {
		r := byHost[host][0]
		if strings.Contains(r.url, "secret") || strings.Contains(r.url, "api_key") || r.headers.Get("Authorization") != "" || r.headers.Get("Crossref-Plus-API-Token") != "" {
			t.Fatalf("%s received credentials: %+v", host, r)
		}
	}
}

func TestCredentialsDoNotFollowCrossHostRedirects(t *testing.T) {
	rec := &recorder{}
	requester := NewRequesterWithTransport(credentialConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		rec.add(req)
		if req.URL.Host == "api.crossref.org" {
			resp := httpResponse(http.StatusFound, "")
			resp.Header.Set("Location", "https://evil.example.net/collect")
			return resp, nil
		}
		return httpResponse(http.StatusOK, "ok"), nil
	}))
	if _, _, err := requester.Get(context.Background(), "https://api.crossref.org/works"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(rec.reqs))
	}
	evil := rec.reqs[1]
	if !strings.HasPrefix(evil.url, "https://evil.example.net/") {
		t.Fatalf("second hop = %s", evil.url)
	}
	if evil.headers.Get("Crossref-Plus-API-Token") != "" || strings.Contains(evil.url, "secret") || strings.Contains(evil.url, "mailto") {
		t.Fatalf("credentials leaked across redirect: %+v", evil)
	}
}

func TestTransportErrorsAreRedacted(t *testing.T) {
	requester := NewRequesterWithTransport(credentialConfig(), roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + req.URL.String())
	}))
	_, _, err := requester.Get(context.Background(), "https://api.unpaywall.org/v2/10.1/x?email=me@example.org")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "me@example.org") || strings.Contains(err.Error(), "oa-secret-key") {
		t.Fatalf("error leaks contact or credential: %v", err)
	}

	_, _, err = requester.Get(context.Background(), "https://api.openalex.org/works")
	if err == nil || strings.Contains(err.Error(), "oa-secret-key") {
		t.Fatalf("openalex error leaks api key: %v", err)
	}
	if toolErr := requestError("openalex", err); strings.Contains(toolErr.Message, "oa-secret-key") {
		t.Fatalf("tool error leaks api key: %s", toolErr.Message)
	}
}

func TestGetEnforcesResponseSizeLimit(t *testing.T) {
	cfg := credentialConfig()
	cfg.MaxResponseBytes = 1024
	requester := NewRequesterWithTransport(cfg, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, strings.Repeat("x", 4096)), nil
	}))
	_, _, err := requester.Get(context.Background(), "https://api.openalex.org/works")
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}

	requester.cfg.SearchProviders = []string{config.ProviderOpenAlex}
	_, toolErr := SearchByKeywords(context.Background(), requester, "x", 5)
	if toolErr == nil || toolErr.Code != CodeUpstreamError || !strings.Contains(toolErr.Message, "size limit") {
		t.Fatalf("toolErr = %+v", toolErr)
	}
}

func TestRetryAfterIsHonouredOrFailsFast(t *testing.T) {
	calls := 0
	requester := newTestRequester(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			resp := httpResponse(http.StatusTooManyRequests, "slow down")
			resp.Header.Set("Retry-After", "1")
			return resp, nil
		}
		return httpResponse(http.StatusOK, "ok"), nil
	}))
	requester.cfg.MaxRetries = 2
	start := time.Now()
	_, status, err := requester.Get(context.Background(), "https://api.crossref.org/works")
	if err != nil || status != http.StatusOK {
		t.Fatalf("Get = %d, %v", status, err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retried after %v, want >= Retry-After 1s", elapsed)
	}

	// A Retry-After beyond the configured maximum fails fast with the hint.
	calls = 0
	requester = newTestRequester(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		resp := httpResponse(http.StatusTooManyRequests, "")
		resp.Header.Set("Retry-After", "600")
		return resp, nil
	}))
	requester.cfg.MaxRetries = 3
	start = time.Now()
	doc, err := requester.GetDocument(context.Background(), "https://api.crossref.org/works")
	if err != nil || doc.Status != http.StatusTooManyRequests || doc.RetryAfter != 600*time.Second {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	if calls != 1 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("calls = %d elapsed = %v; want one fast attempt", calls, time.Since(start))
	}
}

func TestRetryStopsAtDeadline(t *testing.T) {
	calls := 0
	requester := newTestRequester(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return httpResponse(http.StatusServiceUnavailable, ""), nil
	}))
	requester.cfg.MaxRetries = 5
	requester.cfg.BackoffFactor = 2 // 2s+ backoff, beyond the deadline below
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, status, err := requester.Get(ctx, "https://api.openalex.org/works")
	if err != nil || status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d err = %v", status, err)
	}
	if calls != 1 || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("calls = %d elapsed = %v; backoff should not be attempted past the deadline", calls, time.Since(start))
	}
}

func TestHostPacingIsProviderSpecific(t *testing.T) {
	requester := newTestRequester(nil)
	requester.cfg.MinDelay = 5 * time.Second
	requester.cfg.MaxDelay = 5 * time.Second
	requester.cfg.APIMinInterval = 100 * time.Millisecond
	requester.cfg.DocumentMinInterval = time.Second
	if d := requester.hostInterval("scholar.google.com"); d != 5*time.Second {
		t.Fatalf("scholar interval = %v", d)
	}
	if d := requester.hostInterval("api.openalex.org"); d != 100*time.Millisecond {
		t.Fatalf("openalex interval = %v", d)
	}
	if d := requester.hostInterval("www.example.org"); d != time.Second {
		t.Fatalf("document interval = %v", d)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d, ok := parseRetryAfter("7", now); !ok || d != 7*time.Second {
		t.Fatalf("seconds form = %v %v", d, ok)
	}
	if d, ok := parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); !ok || d != 30*time.Second {
		t.Fatalf("date form = %v %v", d, ok)
	}
	if _, ok := parseRetryAfter("soon", now); ok {
		t.Fatal("garbage accepted")
	}
}

func TestSearchProviderOrderAndCrossref(t *testing.T) {
	var hosts []string
	requester := newTestRequester(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		switch req.URL.Host {
		case "api.openalex.org":
			return httpResponse(http.StatusInternalServerError, "outage"), nil
		case "api.crossref.org":
			if req.URL.Query().Get("query.author") != "Ada Lovelace" || req.URL.Query().Get("filter") != "from-pub-date:2020,until-pub-date:2021" {
				t.Errorf("crossref query = %s", req.URL.RawQuery)
			}
			return httpResponse(http.StatusOK, `{"message":{"items":[{"DOI":"10.5555/ABC","title":["Crossref Result"],"abstract":"<jats:p>An abstract.</jats:p>","author":[{"given":"Ada","family":"Lovelace"}],"issued":{"date-parts":[[2021,3]]}}]}}`), nil
		}
		t.Errorf("unexpected host %s", req.URL.Host)
		return httpResponse(http.StatusNotFound, ""), nil
	}))
	outcome, toolErr := Search(context.Background(), requester, SearchRequest{Query: "engines", Author: "Ada Lovelace", YearRange: []int{2021, 2020}, NumResults: 3},
		[]string{config.ProviderOpenAlex, config.ProviderCrossref, config.ProviderScholar})
	if toolErr != nil {
		t.Fatalf("toolErr = %+v", toolErr)
	}
	if outcome.Provider != "crossref" || len(hosts) != 2 {
		t.Fatalf("provider = %s hosts = %v", outcome.Provider, hosts)
	}
	r := outcome.Results[0]
	if r.Source != "crossref" || r.DOI != "10.5555/abc" || r.Year != 2021 || r.Abstract != "An abstract." || r.URL != "https://doi.org/10.5555/abc" {
		t.Fatalf("result = %+v", r)
	}
	if outcome.Attempts[0].Provider != "openalex" || outcome.Attempts[0].Outcome != CodeUpstreamError {
		t.Fatalf("attempts = %+v", outcome.Attempts)
	}
}

func TestDefaultSearchProvidersPreferOpenAlexWithKey(t *testing.T) {
	if got := config.ParseSearchProviders("", true); got[0] != config.ProviderOpenAlex {
		t.Fatalf("with key = %v", got)
	}
	if got := config.ParseSearchProviders("", false); got[0] != config.ProviderScholar {
		t.Fatalf("without key = %v", got)
	}
	if got := config.ParseSearchProviders("crossref, bogus, google_scholar, crossref", false); len(got) != 2 || got[0] != "crossref" || got[1] != "scholar" {
		t.Fatalf("explicit = %v", got)
	}
}
