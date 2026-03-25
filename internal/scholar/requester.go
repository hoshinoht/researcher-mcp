package scholar

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"googlescholar-mcp-go/internal/config"
)

type Requester struct {
	cfg    config.Config
	client *http.Client

	mu         sync.Mutex
	lastByHost map[string]time.Time
	rng        *rand.Rand
}

func NewRequester(cfg config.Config) *Requester {
	r := &Requester{
		cfg:        cfg,
		lastByHost: make(map[string]time.Time),
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	transport := &http.Transport{
		Proxy: r.proxyForRequest,
	}

	r.client = &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
	}

	return r
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

func (r *Requester) Get(ctx context.Context, rawURL string) ([]byte, int, error) {
	attempt := 0

	for {
		attempt++
		if err := r.sleepForRateLimit(rawURL); err != nil {
			return nil, 0, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, 0, err
		}

		req.Header.Set("User-Agent", r.pickUserAgent())
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Connection", "keep-alive")

		resp, err := r.client.Do(req)
		if err != nil {
			if attempt >= r.cfg.MaxRetries {
				return nil, 0, err
			}
			if sleepErr := r.sleepForBackoff(attempt); sleepErr != nil {
				return nil, 0, sleepErr
			}
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, resp.StatusCode, readErr
		}
		if closeErr != nil {
			return nil, resp.StatusCode, closeErr
		}

		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusServiceUnavailable) && attempt < r.cfg.MaxRetries {
			if sleepErr := r.sleepForBackoff(attempt); sleepErr != nil {
				return nil, resp.StatusCode, sleepErr
			}
			continue
		}

		return body, resp.StatusCode, nil
	}
}

func (r *Requester) sleepForRateLimit(rawURL string) error {
	host := hostFromURL(rawURL)
	now := time.Now()

	r.mu.Lock()
	last := r.lastByHost[host]
	delay := r.cfg.MinDelay
	if r.cfg.MaxDelay > r.cfg.MinDelay {
		delta := r.cfg.MaxDelay - r.cfg.MinDelay
		jitter := time.Duration(r.rng.Int63n(int64(delta)))
		delay = r.cfg.MinDelay + jitter
	}
	target := last.Add(delay)
	if target.Before(now) {
		r.lastByHost[host] = now
		r.mu.Unlock()
		return nil
	}
	wait := target.Sub(now)
	r.lastByHost[host] = target
	r.mu.Unlock()

	time.Sleep(wait)
	return nil
}

func (r *Requester) sleepForBackoff(attempt int) error {
	base := math.Pow(r.cfg.BackoffFactor, float64(attempt))
	jitter := float64(r.cfg.MinDelay) * r.rng.Float64()
	d := time.Duration(base*float64(time.Second) + jitter)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	time.Sleep(d)
	return nil
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

func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func BuildBlockedError(status int) *ToolError {
	return &ToolError{
		Code:    "blocked",
		Message: fmt.Sprintf("Google Scholar request returned status %d", status),
		Hint:    "Increase SCHOLAR_MIN_DELAY and SCHOLAR_MAX_DELAY, reduce request volume, or configure SCHOLAR_PROXY_LIST.",
	}
}
