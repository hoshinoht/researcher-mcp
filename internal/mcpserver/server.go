package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/scholar"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	sdkServer *mcp.Server
	requester *scholar.Requester
	implName  string
	implVer   string
}

type SearchKeywordsInput struct {
	Query      string `json:"query" jsonschema:"scholarly search query text"`
	NumResults int    `json:"num_results,omitempty" jsonschema:"number of results (default 5, max 20)"`
}

type SearchAdvancedInput struct {
	Query      string `json:"query" jsonschema:"scholarly search query text"`
	Author     string `json:"author,omitempty" jsonschema:"author filter"`
	YearRange  []int  `json:"year_range,omitempty" jsonschema:"[start_year, end_year]"`
	NumResults int    `json:"num_results,omitempty" jsonschema:"number of results (default 5, max 20)"`
}

type GetAuthorInput struct {
	AuthorName string `json:"author_name" jsonschema:"author display name"`
}

type SearchResponse struct {
	Results []scholar.PaperResult `json:"results"`
	Error   *scholar.ToolError    `json:"error,omitempty"`
}

type AuthorResponse struct {
	Author *scholar.AuthorInfo `json:"author,omitempty"`
	Error  *scholar.ToolError  `json:"error,omitempty"`
}

type HealthResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func New(cfg config.Config, version string) *Server {
	s := &Server{
		implName:  "researcher-mcp",
		implVer:   version,
		requester: scholar.NewRequester(cfg),
	}

	s.sdkServer = mcp.NewServer(&mcp.Implementation{Name: s.implName, Version: s.implVer}, nil)

	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "search_google_scholar_key_words", Description: "Search scholarly articles (Google Scholar first, OpenAlex fallback) using keyword queries"}, s.searchKeywords)
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "search_google_scholar_advanced", Description: "Search scholarly articles (Google Scholar first, OpenAlex fallback) with author/year filters"}, s.searchAdvanced)
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "get_author_info", Description: "Get researcher metadata via OpenAlex/Crossref/ORCID with Scholar fallback"}, s.getAuthorInfo)
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "google_scholar_healthcheck", Description: "Check Researcher MCP process health and runtime mode"}, s.healthcheck)

	// Researcher MCP aliases (preferred names).
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "search_research_articles", Description: "Search scholarly articles (Google Scholar first, OpenAlex fallback) using keyword queries"}, s.searchKeywords)
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "search_research_articles_advanced", Description: "Search scholarly articles (Google Scholar first, OpenAlex fallback) with author/year filters"}, s.searchAdvanced)
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "get_researcher_info", Description: "Get researcher metadata via OpenAlex/Crossref/ORCID with Scholar fallback"}, s.getAuthorInfo)
	mcp.AddTool(s.sdkServer, &mcp.Tool{Name: "researcher_mcp_healthcheck", Description: "Check Researcher MCP process health and runtime mode"}, s.healthcheck)

	return s
}

func (s *Server) Run(ctx context.Context) error {
	return s.sdkServer.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) searchKeywords(ctx context.Context, _ *mcp.CallToolRequest, input SearchKeywordsInput) (*mcp.CallToolResult, SearchResponse, error) {
	if strings.TrimSpace(input.Query) == "" {
		return nil, SearchResponse{Error: &scholar.ToolError{Code: "invalid_input", Message: "query is required"}}, nil
	}
	limit := sanitizeNumResults(input.NumResults)

	results, toolErr := scholar.SearchByKeywords(ctx, s.requester, input.Query, limit)
	if toolErr != nil {
		return nil, SearchResponse{Results: []scholar.PaperResult{}, Error: toolErr}, nil
	}

	return nil, SearchResponse{Results: results}, nil
}

func (s *Server) searchAdvanced(ctx context.Context, _ *mcp.CallToolRequest, input SearchAdvancedInput) (*mcp.CallToolResult, SearchResponse, error) {
	if strings.TrimSpace(input.Query) == "" {
		return nil, SearchResponse{Error: &scholar.ToolError{Code: "invalid_input", Message: "query is required"}}, nil
	}
	if len(input.YearRange) != 0 && len(input.YearRange) != 2 {
		return nil, SearchResponse{Error: &scholar.ToolError{Code: "invalid_input", Message: "year_range must be [start,end]"}}, nil
	}
	limit := sanitizeNumResults(input.NumResults)

	results, toolErr := scholar.SearchAdvanced(ctx, s.requester, input.Query, input.Author, input.YearRange, limit)
	if toolErr != nil {
		return nil, SearchResponse{Results: []scholar.PaperResult{}, Error: toolErr}, nil
	}

	return nil, SearchResponse{Results: results}, nil
}

func (s *Server) getAuthorInfo(ctx context.Context, _ *mcp.CallToolRequest, input GetAuthorInput) (*mcp.CallToolResult, AuthorResponse, error) {
	if strings.TrimSpace(input.AuthorName) == "" {
		return nil, AuthorResponse{Error: &scholar.ToolError{Code: "invalid_input", Message: "author_name is required"}}, nil
	}

	author, toolErr := scholar.GetAuthorInfo(ctx, s.requester, input.AuthorName)
	if toolErr != nil {
		return nil, AuthorResponse{Error: toolErr}, nil
	}

	return nil, AuthorResponse{Author: author}, nil
}

func (s *Server) healthcheck(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, HealthResponse, error) {
	return nil, HealthResponse{
		Status:  "ok",
		Message: fmt.Sprintf("%s %s is running and waiting for MCP client traffic on stdio", s.implName, s.implVer),
	}, nil
}

func sanitizeNumResults(v int) int {
	if v <= 0 {
		return 5
	}
	if v > 20 {
		return 20
	}
	return v
}
