package config

import (
	"os"
	"strconv"
	"strings"
	"time"
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

	return Config{
		MinDelay:         minDelay,
		MaxDelay:         maxDelay,
		MaxRetries:       maxRetries,
		BackoffFactor:    backoff,
		RotateUserAgents: rotate,
		UserAgents:       userAgents,
		ProxyList:        proxyList,
		Timeout:          timeout,
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
