package scholar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"googlescholar-mcp-go/internal/config"
)

// Host classes drive pacing and retry policy.
const (
	hostClassScholar  = "scholar"
	hostClassAPI      = "api"
	hostClassDocument = "document"

	apiUserAgent = "researcher-mcp (+https://github.com/hoshinoht/researcher-mcp)"

	maxRedirects    = 10
	maxBackoffDelay = 30 * time.Second
)

// apiHosts are structured metadata APIs. They get short pacing, an honest
// user agent instead of a rotated browser one, and their own credentials.
var apiHosts = map[string]bool{
	"api.openalex.org":  true,
	"api.crossref.org":  true,
	"pub.orcid.org":     true,
	"orcid.org":         true,
	"api.unpaywall.org": true,
}

type Requester struct {
	cfg    config.Config
	client *http.Client
	creds  *credentialInjector

	mu         sync.Mutex
	lastByHost map[string]time.Time
	notBefore  map[string]time.Time
	rateGates  map[string]chan struct{}
	rng        *rand.Rand
}

func NewRequester(cfg config.Config) *Requester {
	r := &Requester{}
	transport := &http.Transport{
		Proxy:                 r.proxyForRequest,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	r.init(cfg, transport)
	return r
}

// NewRequesterWithTransport builds a requester over a custom base transport
// (used by tests). Credentials are still injected per host above it.
func NewRequesterWithTransport(cfg config.Config, base http.RoundTripper) *Requester {
	r := &Requester{}
	r.init(cfg, base)
	return r
}

func (r *Requester) init(cfg config.Config, base http.RoundTripper) {
	r.cfg = cfg
	r.lastByHost = make(map[string]time.Time)
	r.notBefore = make(map[string]time.Time)
	r.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	// The token client talks to orcid.org/oauth/token without any injected
	// credentials of its own.
	tokenClient := &http.Client{Timeout: 20 * time.Second, Transport: base}
	r.creds = newCredentialInjector(cfg, tokenClient)
	r.client = &http.Client{
		Timeout:       cfg.Timeout,
		Transport:     &authTransport{base: base, creds: r.creds},
		CheckRedirect: checkRedirect,
	}
}

// checkRedirect bounds redirect chains and refuses non-HTTP targets.
// Credentials are attached per hop by authTransport, so a redirect to another
// host never inherits them.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s URL", req.URL.Scheme)
	}
	return nil
}

func (r *Requester) proxyForRequest(_ *http.Request) (*url.URL, error) {
	if len(r.cfg.ProxyList) == 0 {
		return nil, nil
	}
	proxyText := r.randomChoice(r.cfg.ProxyList)
	if proxyText == "" {
		return nil, nil
	}
	p, err := url.Parse(proxyText)
	if err != nil {
		return nil, nil
	}
	return p, nil
}

// FetchedDoc is a size-capped response plus the metadata needed to decide
// how to interpret it.
type FetchedDoc struct {
	Body        []byte
	ContentType string
	FinalURL    string
	Status      int
	// RetryAfter is the server-requested wait when the final response was
	// rate limited and the wait was too long to honour within the deadline.
	RetryAfter time.Duration
}

// ErrBodyTooLarge is returned when a response exceeds the configured size
// limit (SCHOLAR_MAX_FETCH_MB for documents, RESEARCHER_MAX_RESPONSE_MB for
// API and HTML responses).
var ErrBodyTooLarge = errors.New("response body exceeds the configured size limit")

// Get fetches an API or HTML response, bounded by MaxResponseBytes.
func (r *Requester) Get(ctx context.Context, rawURL string) ([]byte, int, error) {
	doc, err := r.fetch(ctx, rawURL, "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8", r.maxResponseBytes())
	if err != nil {
		return nil, 0, err
	}
	return doc.Body, doc.Status, nil
}

// GetJSON fetches a JSON API response, bounded by MaxResponseBytes.
func (r *Requester) GetJSON(ctx context.Context, rawURL string) ([]byte, int, error) {
	doc, err := r.fetch(ctx, rawURL, "application/json", r.maxResponseBytes())
	if err != nil {
		return nil, 0, err
	}
	return doc.Body, doc.Status, nil
}

// GetDocument fetches a document (PDF or HTML), bounded by MaxFetchBytes,
// and reports the final URL after redirects.
func (r *Requester) GetDocument(ctx context.Context, rawURL string) (*FetchedDoc, error) {
	maxBytes := r.cfg.MaxFetchBytes
	if maxBytes <= 0 {
		maxBytes = 30 * 1024 * 1024
	}
	return r.fetch(ctx, rawURL, "application/pdf,text/html;q=0.9,*/*;q=0.8", maxBytes)
}

func (r *Requester) maxResponseBytes() int64 {
	if r.cfg.MaxResponseBytes > 0 {
		return r.cfg.MaxResponseBytes
	}
	return 8 * 1024 * 1024
}

func (r *Requester) maxRetries() int {
	if r.cfg.MaxRetries < 1 {
		return 1
	}
	return r.cfg.MaxRetries
}

// fetch is the shared request loop: per-host pacing, bounded reads, retries
// for transient failures with Retry-After support, and deadline awareness.
func (r *Requester) fetch(ctx context.Context, rawURL, accept string, maxBytes int64) (*FetchedDoc, error) {
	host := hostFromURL(rawURL)
	class := hostClass(host)

	for attempt := 1; ; attempt++ {
		if err := r.sleepForRateLimit(ctx, rawURL); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, errors.New(RedactText(err.Error()))
		}
		if class == hostClassAPI {
			req.Header.Set("User-Agent", apiUserAgent)
		} else {
			req.Header.Set("User-Agent", r.pickUserAgent())
		}
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Accept", accept)

		resp, err := r.client.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			err = redactError(err)
			if attempt >= r.maxRetries() || !isRetryableNetError(err) {
				return nil, err
			}
			if !r.waitForRetry(ctx, r.backoffDelay(attempt)) {
				return nil, err
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, redactError(readErr)
		}
		if int64(len(body)) > maxBytes {
			return nil, fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, maxBytes)
		}

		if host == "pub.orcid.org" && resp.StatusCode == http.StatusUnauthorized {
			r.creds.invalidateORCIDToken()
		}

		finalURL := rawURL
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		doc := &FetchedDoc{
			Body:        body,
			ContentType: resp.Header.Get("Content-Type"),
			FinalURL:    finalURL,
			Status:      resp.StatusCode,
		}

		if !isRetryableStatus(class, resp.StatusCode) {
			return doc, nil
		}

		retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		wait := r.backoffDelay(attempt)
		if hasRetryAfter {
			wait = retryAfter
			r.deferHost(host, retryAfter)
		}
		if attempt >= r.maxRetries() || (hasRetryAfter && retryAfter > r.maxRetryAfter()) || !r.waitForRetry(ctx, wait) {
			if hasRetryAfter {
				doc.RetryAfter = retryAfter
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return doc, nil
		}
	}
}

func (r *Requester) maxRetryAfter() time.Duration {
	if r.cfg.MaxRetryAfter > 0 {
		return r.cfg.MaxRetryAfter
	}
	return 30 * time.Second
}

// waitForRetry sleeps d unless that would overrun the context deadline, in
// which case it returns false so the caller can fail fast instead.
func (r *Requester) waitForRetry(ctx context.Context, d time.Duration) bool {
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(d).After(deadline) {
		return false
	}
	return sleepWithContext(ctx, d) == nil
}

func (r *Requester) deferHost(host string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(r.notBefore[host]) {
		r.notBefore[host] = until
	}
}

// isRetryableStatus reports transient statuses. 403 is not transient (it is
// usually a block or login wall), and Scholar rate limits are blocks too:
// retrying them only deepens the block.
func isRetryableStatus(class string, status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return class != hostClassScholar
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRetryableNetError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && strings.Contains(urlErr.Err.Error(), "redirect") {
		return false
	}
	return true
}

// parseRetryAfter accepts both delta-seconds and HTTP-date forms.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// hostInterval is the minimum spacing for a host: the configured Scholar
// jitter window for Google Scholar, short pacing for metadata APIs, and a
// moderate default for everything else.
func (r *Requester) hostInterval(host string) time.Duration {
	switch hostClass(host) {
	case hostClassScholar:
		delay := r.cfg.MinDelay
		if r.cfg.MaxDelay > r.cfg.MinDelay {
			r.mu.Lock()
			delay += time.Duration(r.rng.Int63n(int64(r.cfg.MaxDelay - r.cfg.MinDelay)))
			r.mu.Unlock()
		}
		return delay
	case hostClassAPI:
		return r.cfg.APIMinInterval
	default:
		return r.cfg.DocumentMinInterval
	}
}

func hostClass(host string) string {
	switch {
	case host == "scholar.google.com":
		return hostClassScholar
	case apiHosts[host]:
		return hostClassAPI
	default:
		return hostClassDocument
	}
}

func (r *Requester) sleepForRateLimit(ctx context.Context, rawURL string) error {
	host := hostFromURL(rawURL)
	if err := ctx.Err(); err != nil {
		return err
	}

	gate := r.rateLimitGate(host)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
	}
	defer func() { gate <- struct{}{} }()

	delay := r.hostInterval(host)
	r.mu.Lock()
	target := r.lastByHost[host].Add(delay)
	if nb := r.notBefore[host]; nb.After(target) {
		target = nb
	}
	r.mu.Unlock()

	if wait := time.Until(target); wait > 0 {
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
			return fmt.Errorf("%s is rate limited for another %s, beyond the operation deadline: %w", host, wait.Round(time.Second), context.DeadlineExceeded)
		}
		if err := sleepWithContext(ctx, wait); err != nil {
			return err
		}
	}

	r.mu.Lock()
	r.lastByHost[host] = time.Now()
	r.mu.Unlock()
	return nil
}

func (r *Requester) rateLimitGate(host string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rateGates == nil {
		r.rateGates = make(map[string]chan struct{})
	}
	if gate := r.rateGates[host]; gate != nil {
		return gate
	}

	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	r.rateGates[host] = gate
	return gate
}

func (r *Requester) backoffDelay(attempt int) time.Duration {
	// A zero factor disables backoff (tests); Load clamps it to >= 1.
	if r.cfg.BackoffFactor <= 0 {
		return 0
	}
	base := math.Pow(r.cfg.BackoffFactor, float64(attempt)) * float64(time.Second)
	r.mu.Lock()
	jitter := r.rng.Float64() * math.Min(base/4, float64(time.Second))
	r.mu.Unlock()
	d := time.Duration(base + jitter)
	if d > maxBackoffDelay {
		d = maxBackoffDelay
	}
	return d
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Requester) pickUserAgent() string {
	if len(r.cfg.UserAgents) == 0 {
		return "Mozilla/5.0"
	}
	if !r.cfg.RotateUserAgents {
		return r.cfg.UserAgents[0]
	}
	return r.randomChoice(r.cfg.UserAgents)
}

func (r *Requester) randomChoice(values []string) string {
	if len(values) == 0 {
		return ""
	}
	r.mu.Lock()
	idx := r.rng.Intn(len(values))
	r.mu.Unlock()
	return values[idx]
}

// CredentialStatus reports which provider credentials are configured,
// without revealing them and without any network call.
func (r *Requester) CredentialStatus() CredentialStatus {
	return CredentialStatus{
		OpenAlexAPIKey:    r.cfg.OpenAlexAPIKey != "",
		CrossrefPlusToken: r.cfg.CrossrefPlusToken != "",
		ORCIDClient:       r.cfg.ORCIDClientID != "" && r.cfg.ORCIDClientSecret != "",
		ContactEmail:      r.cfg.ContactEmail != "",
	}
}

// ContactEmail is the configured contact address (used by Unpaywall, which
// requires it as a query parameter).
func (r *Requester) ContactEmail() string { return r.cfg.ContactEmail }

type CredentialStatus struct {
	OpenAlexAPIKey    bool `json:"openalex_api_key"`
	CrossrefPlusToken bool `json:"crossref_plus_token"`
	ORCIDClient       bool `json:"orcid_client_credentials"`
	ContactEmail      bool `json:"contact_email"`
}

func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func BuildBlockedError(status int) *ToolError {
	return &ToolError{
		Code:      "blocked",
		Message:   fmt.Sprintf("Google Scholar request returned status %d", status),
		Hint:      "Increase SCHOLAR_MIN_DELAY and SCHOLAR_MAX_DELAY, reduce request volume, or configure SCHOLAR_PROXY_LIST.",
		Retryable: true,
	}
}
