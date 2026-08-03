//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/fulltext"
	"googlescholar-mcp-go/internal/scholar"
)

func newIntegrationFulltextService() *fulltext.Service {
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
	return fulltext.NewService(scholar.NewRequester(cfg), cfg)
}

func TestGetPaperContentArxivLive(t *testing.T) {
	requireLiveScholar(t)
	svc := newIntegrationFulltextService()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	content, toolErr := svc.GetPaperContent(ctx, fulltext.Request{ArxivID: "1706.03762"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if !strings.Contains(strings.ToLower(content.Markdown), "attention") {
		t.Fatalf("markdown does not mention attention; source=%s converter=%s", content.SourceURL, content.Converter)
	}
	t.Logf("source=%s type=%s converter=%s total_chars=%d", content.SourceURL, content.SourceType, content.Converter, content.TotalChars)
}

func TestGetPaperContentPaginationRoundTripLive(t *testing.T) {
	requireLiveScholar(t)
	svc := newIntegrationFulltextService()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	req := fulltext.Request{ArxivID: "1706.03762"}
	page1, toolErr := svc.GetPaperContent(ctx, req, 5000, 0)
	if toolErr != nil {
		t.Fatalf("page1 error: %+v", toolErr)
	}
	if !page1.Truncated || page1.NextOffset == 0 {
		t.Fatalf("expected truncation on a full paper: %+v", page1.TotalChars)
	}

	page2, toolErr := svc.GetPaperContent(ctx, req, 5000, page1.NextOffset)
	if toolErr != nil {
		t.Fatalf("page2 error: %+v", toolErr)
	}
	if page2.Offset != page1.NextOffset {
		t.Fatalf("page2 offset = %d, want %d", page2.Offset, page1.NextOffset)
	}
}

func TestGetPaperContentDOILive(t *testing.T) {
	requireLiveScholar(t)
	svc := newIntegrationFulltextService()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// PLOS ONE is reliably open access.
	content, toolErr := svc.GetPaperContent(ctx, fulltext.Request{DOI: "10.1371/journal.pone.0266462"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if content.TotalChars < 2000 {
		t.Fatalf("suspiciously short content: %d chars from %s", content.TotalChars, content.SourceURL)
	}
}

func TestGetPaperContentTitleLive(t *testing.T) {
	requireLiveScholar(t)
	svc := newIntegrationFulltextService()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	content, toolErr := svc.GetPaperContent(ctx, fulltext.Request{Title: "Attention Is All You Need"}, 0, 0)
	if toolErr != nil {
		t.Fatalf("GetPaperContent error: %+v", toolErr)
	}
	if !strings.Contains(strings.ToLower(content.Markdown), "attention") {
		t.Fatalf("markdown does not mention attention; source=%s", content.SourceURL)
	}
}
