package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Search providers accepted by RESEARCHER_SEARCH_PROVIDERS.
const (
	ProviderOpenAlex = "openalex"
	ProviderCrossref = "crossref"
	ProviderScholar  = "scholar"
)

// Tool catalog modes accepted by RESEARCHER_TOOL_SET.
const (
	ToolSetAll       = "all"       // legacy names and preferred aliases (default, backwards compatible)
	ToolSetPreferred = "preferred" // researcher_* / *_research_* names only
	ToolSetLegacy    = "legacy"    // original google_scholar-era names only
)

type Config struct {
	MinDelay         time.Duration
	MaxDelay         time.Duration
	MaxRetries       int
	BackoffFactor    float64
	RotateUserAgents bool
	UserAgents       []string
	ProxyList        []string
	Timeout          time.Duration
	ContactEmail     string
	MaxFetchBytes    int64
	PdftotextPath    string

	// MaxResponseBytes caps API/HTML responses read through Requester.Get.
	MaxResponseBytes int64
	// OperationTimeout bounds one whole tool call (all providers, retries and
	// extraction). Zero disables the bound.
	OperationTimeout time.Duration
	// APIMinInterval is the minimum spacing between requests to structured
	// metadata APIs (OpenAlex, Crossref, ORCID, Unpaywall).
	APIMinInterval time.Duration
	// DocumentMinInterval is the minimum spacing between requests to any other
	// host (publisher pages, repositories, arXiv).
	DocumentMinInterval time.Duration
	// MaxRetryAfter is the longest Retry-After the requester is willing to
	// honour by sleeping; longer waits fail fast with a retryable error.
	MaxRetryAfter time.Duration

	// MaxExtractChars bounds the characters kept from one extracted document.
	MaxExtractChars int
	// CacheMaxBytes bounds the total size of cached extracted documents.
	CacheMaxBytes int64

	// Provider credentials. Each is only ever attached to its own provider.
	OpenAlexAPIKey    string
	CrossrefPlusToken string
	ORCIDClientID     string
	ORCIDClientSecret string

	// SearchProviders is the ordered article-search provider list.
	SearchProviders []string
	// ToolSet selects which tool names are registered.
	ToolSet string
}

var defaultUserAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
}

func Load() Config {
	minDelay := getEnvDurationSeconds("SCHOLAR_MIN_DELAY", 3)
	maxDelay := getEnvDurationSeconds("SCHOLAR_MAX_DELAY", 8)
	if maxDelay < minDelay {
		maxDelay = minDelay
	}

	maxRetries := getEnvInt("SCHOLAR_MAX_RETRIES", 5)
	if maxRetries < 1 {
		maxRetries = 1
	}

	backoff := getEnvFloat("SCHOLAR_BACKOFF_FACTOR", 2.0)
	if backoff < 1.0 {
		backoff = 1.0
	}

	rotate := getEnvBool("SCHOLAR_ROTATE_USER_AGENTS", true)
	userAgents := getEnvCSV("SCHOLAR_USER_AGENTS")
	if len(userAgents) == 0 {
		userAgents = append([]string{}, defaultUserAgents...)
	}

	proxyList := getEnvCSV("SCHOLAR_PROXY_LIST")
	timeout := getEnvDurationSeconds("SCHOLAR_TIMEOUT_SECONDS", 25)

	maxFetchMB := getEnvInt("SCHOLAR_MAX_FETCH_MB", 30)
	if maxFetchMB < 1 {
		maxFetchMB = 30
	}
	maxResponseMB := getEnvInt("RESEARCHER_MAX_RESPONSE_MB", 8)
	if maxResponseMB < 1 {
		maxResponseMB = 8
	}
	cacheMB := getEnvInt("RESEARCHER_CACHE_MAX_MB", 64)
	if cacheMB < 1 {
		cacheMB = 64
	}
	maxExtract := getEnvInt("RESEARCHER_MAX_EXTRACT_CHARS", 2_000_000)
	if maxExtract < 10_000 {
		maxExtract = 2_000_000
	}

	openAlexKey := strings.TrimSpace(os.Getenv("OPENALEX_API_KEY"))

	return Config{
		MinDelay:            minDelay,
		MaxDelay:            maxDelay,
		MaxRetries:          maxRetries,
		BackoffFactor:       backoff,
		RotateUserAgents:    rotate,
		UserAgents:          userAgents,
		ProxyList:           proxyList,
		Timeout:             timeout,
		ContactEmail:        strings.TrimSpace(os.Getenv("SCHOLAR_CONTACT_EMAIL")),
		MaxFetchBytes:       int64(maxFetchMB) * 1024 * 1024,
		PdftotextPath:       strings.TrimSpace(os.Getenv("SCHOLAR_PDFTOTEXT_PATH")),
		MaxResponseBytes:    int64(maxResponseMB) * 1024 * 1024,
		OperationTimeout:    getEnvDurationSeconds("RESEARCHER_OPERATION_TIMEOUT_SECONDS", 120),
		APIMinInterval:      getEnvDurationMillis("RESEARCHER_API_MIN_INTERVAL_MS", 150),
		DocumentMinInterval: getEnvDurationMillis("RESEARCHER_DOCUMENT_MIN_INTERVAL_MS", 1000),
		MaxRetryAfter:       getEnvDurationSeconds("RESEARCHER_MAX_RETRY_AFTER_SECONDS", 30),
		MaxExtractChars:     maxExtract,
		CacheMaxBytes:       int64(cacheMB) * 1024 * 1024,
		OpenAlexAPIKey:      openAlexKey,
		CrossrefPlusToken:   strings.TrimSpace(os.Getenv("CROSSREF_API_KEY")),
		ORCIDClientID:       strings.TrimSpace(os.Getenv("ORCID_CLIENT_ID")),
		ORCIDClientSecret:   strings.TrimSpace(os.Getenv("ORCID_CLIENT_SECRET")),
		SearchProviders:     ParseSearchProviders(os.Getenv("RESEARCHER_SEARCH_PROVIDERS"), openAlexKey != ""),
		ToolSet:             parseToolSet(os.Getenv("RESEARCHER_TOOL_SET")),
	}
}

// DefaultSearchProviders prefers the structured OpenAlex API once it is
// authenticated (OpenAlex requires an API key for a usable daily budget);
// without a key, Google Scholar stays first as before.
func DefaultSearchProviders(openAlexKeyConfigured bool) []string {
	if openAlexKeyConfigured {
		return []string{ProviderOpenAlex, ProviderCrossref, ProviderScholar}
	}
	return []string{ProviderScholar, ProviderOpenAlex, ProviderCrossref}
}

// ParseSearchProviders parses a comma-separated provider list, dropping
// unknown and duplicate names. An empty or fully invalid list yields the
// default order.
func ParseSearchProviders(raw string, openAlexKeyConfigured bool) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "google_scholar" {
			name = ProviderScholar
		}
		switch name {
		case ProviderOpenAlex, ProviderCrossref, ProviderScholar:
		default:
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return DefaultSearchProviders(openAlexKeyConfigured)
	}
	return out
}

func parseToolSet(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ToolSetPreferred:
		return ToolSetPreferred
	case ToolSetLegacy:
		return ToolSetLegacy
	default:
		return ToolSetAll
	}
}

func getEnvCSV(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func getEnvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvDurationSeconds(key string, fallbackSeconds int) time.Duration {
	v := getEnvInt(key, fallbackSeconds)
	if v < 0 {
		v = fallbackSeconds
	}
	return time.Duration(v) * time.Second
}

func getEnvDurationMillis(key string, fallbackMillis int) time.Duration {
	v := getEnvInt(key, fallbackMillis)
	if v < 0 {
		v = fallbackMillis
	}
	return time.Duration(v) * time.Millisecond
}
