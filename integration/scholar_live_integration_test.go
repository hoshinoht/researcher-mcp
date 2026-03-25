//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"
)

func requireLiveScholar(t *testing.T) {
	t.Helper()
	if os.Getenv("SCHOLAR_LIVE_TESTS") != "1" {
		t.Skip("set SCHOLAR_LIVE_TESTS=1 to run live integration tests")
	}
}

func newIntegrationRequester() *scholar.Requester {
	cfg := config.Load()
	if cfg.MinDelay < time.Second {
		cfg.MinDelay = time.Second
	}
	if cfg.MaxDelay < cfg.MinDelay {
		cfg.MaxDelay = cfg.MinDelay
	}
	if cfg.Timeout < 20*time.Second {
		cfg.Timeout = 20 * time.Second
	}
	return scholar.NewRequester(cfg)
}

func TestSearchByKeywordsWithIEEECitationQueries(t *testing.T) {
	requireLiveScholar(t)
	requester := newIntegrationRequester()

	queries := citationQueries
	if len(queries) > 5 {
		queries = queries[:5]
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()

			results, toolErr := scholar.SearchByKeywords(ctx, requester, query, 3)
			if toolErr != nil {
				if toolErr.Code == "blocked" {
					t.Skipf("live request was blocked by Scholar: %s", toolErr.Message)
				}
				t.Fatalf("unexpected tool error: %+v", toolErr)
			}

			if len(results) == 0 {
				t.Fatalf("expected at least one result for query %q", query)
			}
		})
	}
}

func TestAdvancedSearchUsingCitationContext(t *testing.T) {
	requireLiveScholar(t)
	requester := newIntegrationRequester()

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	results, toolErr := scholar.SearchAdvanced(
		ctx,
		requester,
		"Digital health interventions to support family caregivers",
		"Zhai",
		[]int{2020, 2025},
		3,
	)
	if toolErr != nil {
		if toolErr.Code == "blocked" {
			t.Skipf("live request was blocked by Scholar: %s", toolErr.Message)
		}
		t.Fatalf("unexpected tool error: %+v", toolErr)
	}

	if len(results) == 0 {
		t.Fatalf("expected results for advanced citation-driven query")
	}
	logJSON(t, "advanced_results", results)
}

func TestGetAuthorInfoUsingCitationAuthor(t *testing.T) {
	requireLiveScholar(t)
	requester := newIntegrationRequester()

	// Use multiple citation-linked names because Scholar's search page can be
	// unstable for specific names depending on region, anti-bot state, and time.
	candidates := []string{"Tianqi Chen", "Serena Zhai", "Geoffrey Hinton"}

	var lastNoResultsErr *scholar.ToolError
	for _, candidate := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		author, toolErr := scholar.GetAuthorInfo(ctx, requester, candidate)
		cancel()

		if toolErr != nil {
			if toolErr.Code == "blocked" {
				t.Skipf("live request was blocked by Scholar while querying %q: %s", candidate, toolErr.Message)
			}
			if toolErr.Code == "no_results" {
				lastNoResultsErr = toolErr
				t.Logf("no author result for %q, trying next candidate", candidate)
				continue
			}
			t.Fatalf("unexpected tool error for %q: %+v", candidate, toolErr)
		}

		if author == nil {
			t.Logf("nil author response for %q, trying next candidate", candidate)
			continue
		}
		if author.Name == "" || author.Name == "N/A" {
			t.Logf("empty author name for %q, trying next candidate", candidate)
			continue
		}

		logJSON(t, "author_info", author)
		return
	}

	if lastNoResultsErr != nil {
		t.Fatalf("all author candidates returned no_results; last error: %+v", lastNoResultsErr)
	}
	t.Fatalf("failed to resolve any author candidate")
}

func logJSON(t *testing.T, label string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Logf("%s marshal error: %v", label, err)
		return
	}
	t.Logf("%s:\n%s", label, string(b))
}
