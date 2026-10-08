package scholar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"googlescholar-mcp-go/internal/config"
)

const orcidTokenURL = "https://orcid.org/oauth/token"

// credentialInjector attaches each provider's credential only to requests
// whose host is exactly that provider's API host. Injection happens per hop
// inside the transport, so redirects to other hosts never carry credentials
// and the URLs seen by callers (and their error messages) never contain them.
type credentialInjector struct {
	cfg         config.Config
	tokenClient *http.Client

	mu          sync.Mutex
	orcidToken  string
	orcidFailed time.Time
}

func newCredentialInjector(cfg config.Config, tokenClient *http.Client) *credentialInjector {
	return &credentialInjector{cfg: cfg, tokenClient: tokenClient}
}

func (c *credentialInjector) apply(req *http.Request) {
	if req.URL.Scheme != "https" {
		return
	}
	switch strings.ToLower(req.URL.Hostname()) {
	case "api.openalex.org":
		// OpenAlex: free API key as the documented api_key query parameter.
		// The retired mailto polite pool is no longer sent.
		if c.cfg.OpenAlexAPIKey != "" {
			setQueryParam(req, "api_key", c.cfg.OpenAlexAPIKey)
		}
	case "api.crossref.org":
		// Crossref: Metadata Plus token header; mailto selects the polite pool.
		if c.cfg.CrossrefPlusToken != "" {
			req.Header.Set("Crossref-Plus-API-Token", "Bearer "+c.cfg.CrossrefPlusToken)
		}
		if c.cfg.ContactEmail != "" && req.URL.Query().Get("mailto") == "" {
			setQueryParam(req, "mailto", c.cfg.ContactEmail)
		}
	case "pub.orcid.org":
		// ORCID public API: /read-public bearer token from client credentials.
		if token := c.orcidAccessToken(req.Context()); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
}

func setQueryParam(req *http.Request, key, value string) {
	q := req.URL.Query()
	q.Set(key, value)
	req.URL.RawQuery = q.Encode()
}

// orcidAccessToken lazily obtains a long-lived /read-public token. Failures
// fall back to anonymous access and are not retried for a minute.
func (c *credentialInjector) orcidAccessToken(ctx context.Context) string {
	if c.cfg.ORCIDClientID == "" || c.cfg.ORCIDClientSecret == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.orcidToken != "" {
		return c.orcidToken
	}
	if !c.orcidFailed.IsZero() && time.Since(c.orcidFailed) < time.Minute {
		return ""
	}
	token, err := c.fetchORCIDToken(ctx)
	if err != nil {
		c.orcidFailed = time.Now()
		return ""
	}
	c.orcidToken = token
	return token
}

func (c *credentialInjector) invalidateORCIDToken() {
	c.mu.Lock()
	c.orcidToken = ""
	c.mu.Unlock()
}

func (c *credentialInjector) fetchORCIDToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("client_id", c.cfg.ORCIDClientID)
	form.Set("client_secret", c.cfg.ORCIDClientSecret)
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "/read-public")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orcidTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", apiUserAgent)

	resp, err := c.tokenClient.Do(req)
	if err != nil {
		return "", redactError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("orcid token request returned status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("orcid token response missing access_token")
	}
	return tok.AccessToken, nil
}

// authTransport clones each outgoing request and attaches host-scoped
// credentials before delegating to the base transport.
type authTransport struct {
	base  http.RoundTripper
	creds *credentialInjector
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// Never forward credential-bearing headers a caller may have set for a
	// different host.
	clone.Header.Del("Crossref-Plus-API-Token")
	if !strings.EqualFold(clone.URL.Hostname(), "pub.orcid.org") {
		clone.Header.Del("Authorization")
	}
	t.creds.apply(clone)
	return t.base.RoundTrip(clone)
}

var (
	secretQueryPattern = regexp.MustCompile(`(?i)((?:api_key|apikey|access_token|token|client_secret|email|mailto)=)[^&\s"']+`)
	bearerPattern      = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

// RedactText removes credential- and contact-bearing query values and bearer
// tokens from text destined for errors, attempt histories or logs.
func RedactText(s string) string {
	s = secretQueryPattern.ReplaceAllString(s, "${1}REDACTED")
	return bearerPattern.ReplaceAllString(s, "${1}REDACTED")
}

// RedactURL is RedactText for a single URL.
func RedactURL(rawURL string) string { return RedactText(rawURL) }

func redactError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &url.Error{Op: urlErr.Op, URL: RedactURL(urlErr.URL), Err: errors.New(RedactText(urlErr.Err.Error()))}
	}
	return errors.New(RedactText(err.Error()))
}
