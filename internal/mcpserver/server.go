package mcpserver

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"googlescholar-mcp-go/internal/config"
	"googlescholar-mcp-go/internal/fulltext"
	"googlescholar-mcp-go/internal/scholar"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	sdkServer *mcp.Server
	cfg       config.Config
	requester *scholar.Requester
	fulltext  *fulltext.Service
	implName  string
	implVer   string
	started   time.Time
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
	AuthorName  string `json:"author_name,omitempty" jsonschema:"researcher name, e.g. 'Ada Lovelace' or 'Lovelace, Ada' (one person)"`
	ORCID       string `json:"orcid,omitempty" jsonschema:"ORCID iD (exact identifier; takes precedence over author_name)"`
	OpenAlexID  string `json:"openalex_id,omitempty" jsonschema:"OpenAlex author ID such as A5023888391 (exact identifier)"`
	Affiliation string `json:"affiliation,omitempty" jsonschema:"optional institution used as evidence to tell same-name researchers apart"`
	KnownPaper  string `json:"known_paper,omitempty" jsonschema:"optional DOI or exact title of a paper by this researcher, used as identity evidence"`
}

type GetPaperContentInput struct {
	URL        string `json:"url,omitempty" jsonschema:"direct URL to the paper (landing page or PDF)"`
	DOI        string `json:"doi,omitempty" jsonschema:"DOI, e.g. 10.1038/nature14539"`
	ArxivID    string `json:"arxiv_id,omitempty" jsonschema:"arXiv identifier, e.g. 1706.03762"`
	OpenAlexID string `json:"openalex_id,omitempty" jsonschema:"OpenAlex work ID, e.g. W2194775991"`
	Title      string `json:"title,omitempty" jsonschema:"paper title; matched against several candidates and rejected as ambiguous when evidence is insufficient (prefer DOI/arXiv/URL)"`
	Author     string `json:"author,omitempty" jsonschema:"optional author name or surname used as evidence for title matching"`
	Year       int    `json:"year,omitempty" jsonschema:"optional publication year used as evidence for title matching"`
	MaxChars   int    `json:"max_chars,omitempty" jsonschema:"max markdown characters to return (default 40000, max 150000)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"character offset for pagination (use next_offset from the previous call)"`
}

type SearchResponse struct {
	Results []scholar.PaperResult `json:"results"`
	// Provider is the search provider that produced Results.
	Provider string             `json:"provider,omitempty"`
	Attempts []scholar.Attempt  `json:"attempts,omitempty"`
	Error    *scholar.ToolError `json:"error,omitempty"`
}

type AuthorResponse struct {
	Author *scholar.AuthorInfo `json:"author,omitempty"`
	Error  *scholar.ToolError  `json:"error,omitempty"`
}

type PaperContentResponse struct {
	Content *fulltext.PaperContent `json:"content,omitempty"`
	Error   *scholar.ToolError     `json:"error,omitempty"`
}

type HealthResponse struct {
	Status       string        `json:"status"`
	Message      string        `json:"message"`
	Process      ProcessHealth `json:"process"`
	Capabilities Capabilities  `json:"capabilities"`
}

// ProcessHealth describes the running process only.
type ProcessHealth struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	GoVersion     string `json:"go_version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// Capabilities describe configuration. They are derived locally and are not
// a statement that any provider is currently reachable.
type Capabilities struct {
	Note            string                        `json:"note"`
	SearchProviders []string                      `json:"search_providers"`
	Providers       map[string]ProviderCapability `json:"providers"`
	Extractors      ExtractorCapabilities         `json:"extractors"`
	Limits          Limits                        `json:"limits"`
	ToolSet         string                        `json:"tool_set"`
}

type ProviderCapability struct {
	Used        []string `json:"used_for"`
	Credentials string   `json:"credentials"`
	Note        string   `json:"note,omitempty"`
}

type ExtractorCapabilities struct {
	Pdftotext     bool   `json:"pdftotext"`
	PdftotextPath string `json:"pdftotext_path,omitempty"`
	GoPDF         bool   `json:"go_pdf"`
	HTML          bool   `json:"html"`
}

type Limits struct {
	OperationTimeoutSeconds int   `json:"operation_timeout_seconds"`
	MaxDocumentBytes        int64 `json:"max_document_bytes"`
	MaxResponseBytes        int64 `json:"max_response_bytes"`
	MaxExtractChars         int   `json:"max_extract_chars"`
	CacheMaxBytes           int64 `json:"cache_max_bytes"`
}

type toolDef struct {
	legacy, preferred string
	register          func(name string)
}

func New(cfg config.Config, version string) *Server {
	return newServer(cfg, version, scholar.NewRequester(cfg))
}

func newServer(cfg config.Config, version string, requester *scholar.Requester) *Server {
	s := &Server{
		cfg:       cfg,
		implName:  "researcher-mcp",
		implVer:   version,
		requester: requester,
		started:   time.Now(),
	}
	s.fulltext = fulltext.NewService(s.requester, cfg)
	s.sdkServer = mcp.NewServer(&mcp.Implementation{Name: s.implName, Version: s.implVer}, nil)

	order := strings.Join(displayProviders(cfg.SearchProviders), " → ")
	searchDesc := fmt.Sprintf("Search scholarly articles (providers in order: %s; each result reports its source) using keyword queries", order)
	advancedDesc := fmt.Sprintf("Search scholarly articles (providers in order: %s; each result reports its source) with author/year filters", order)
	authorDesc := "Get researcher metadata (OpenAlex, then ORCID, Google Scholar and Crossref fallbacks). Accepts exact orcid/openalex_id, plus optional affiliation/known_paper evidence; returns an 'ambiguous' error with candidates instead of guessing between same-name people"
	paperDesc := "Fetch a paper's text as markdown by URL, DOI, arXiv ID, OpenAlex ID, or title (open-access sources; paginated via max_chars/offset). content_status says whether it is full text, partial, or abstract only"
	healthDesc := "Report Researcher MCP process health and configured providers/extractors (no network calls)"

	defs := []toolDef{
		{"search_google_scholar_key_words", "search_research_articles", func(name string) {
			mcp.AddTool(s.sdkServer, &mcp.Tool{Name: name, Description: searchDesc}, s.searchKeywords)
		}},
		{"search_google_scholar_advanced", "search_research_articles_advanced", func(name string) {
			mcp.AddTool(s.sdkServer, &mcp.Tool{Name: name, Description: advancedDesc}, s.searchAdvanced)
		}},
		{"get_author_info", "get_researcher_info", func(name string) {
			mcp.AddTool(s.sdkServer, &mcp.Tool{Name: name, Description: authorDesc}, s.getAuthorInfo)
		}},
		{"google_scholar_healthcheck", "researcher_mcp_healthcheck", func(name string) {
			mcp.AddTool(s.sdkServer, &mcp.Tool{Name: name, Description: healthDesc}, s.healthcheck)
		}},
		{"get_paper_fulltext", "read_research_paper", func(name string) {
			mcp.AddTool(s.sdkServer, &mcp.Tool{Name: name, Description: paperDesc}, s.getPaperContent)
		}},
	}
	for _, name := range ToolNames(cfg.ToolSet) {
		for _, d := range defs {
			if d.legacy == name || d.preferred == name {
				d.register(name)
			}
		}
	}
	return s
}

// ToolNames lists the tool names registered for a tool set.
func ToolNames(toolSet string) []string {
	legacy := []string{"search_google_scholar_key_words", "search_google_scholar_advanced", "get_author_info", "google_scholar_healthcheck", "get_paper_fulltext"}
	preferred := []string{"search_research_articles", "search_research_articles_advanced", "get_researcher_info", "researcher_mcp_healthcheck", "read_research_paper"}
	switch toolSet {
	case config.ToolSetPreferred:
		return preferred
	case config.ToolSetLegacy:
		return legacy
	default:
		return append(legacy, preferred...)
	}
}

func displayProviders(providers []string) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		switch p {
		case config.ProviderOpenAlex:
			out = append(out, "OpenAlex")
		case config.ProviderCrossref:
			out = append(out, "Crossref")
		case config.ProviderScholar:
			out = append(out, "Google Scholar")
		}
	}
	return out
}

func (s *Server) Run(ctx context.Context) error {
	return s.sdkServer.Run(ctx, &mcp.StdioTransport{})
}

// MCPServer exposes the SDK server (tests connect over in-memory transports).
func (s *Server) MCPServer() *mcp.Server { return s.sdkServer }

// withDeadline bounds one tool call by the configured operation timeout.
func (s *Server) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.cfg.OperationTimeout > 0 {
		return context.WithTimeout(ctx, s.cfg.OperationTimeout)
	}
	return context.WithCancel(ctx)
}

// errorResult marks the MCP result as an error while keeping the structured
// error payload in structuredContent.
func errorResult() *mcp.CallToolResult { return &mcp.CallToolResult{IsError: true} }

func (s *Server) searchKeywords(ctx context.Context, _ *mcp.CallToolRequest, input SearchKeywordsInput) (*mcp.CallToolResult, SearchResponse, error) {
	if strings.TrimSpace(input.Query) == "" {
		return errorResult(), SearchResponse{Results: []scholar.PaperResult{}, Error: &scholar.ToolError{Code: scholar.CodeInvalidInput, Message: "query is required"}}, nil
	}
	ctx, cancel := s.withDeadline(ctx)
	defer cancel()
	return searchResponse(scholar.SearchByKeywords(ctx, s.requester, input.Query, sanitizeNumResults(input.NumResults)))
}

func (s *Server) searchAdvanced(ctx context.Context, _ *mcp.CallToolRequest, input SearchAdvancedInput) (*mcp.CallToolResult, SearchResponse, error) {
	if strings.TrimSpace(input.Query) == "" {
		return errorResult(), SearchResponse{Results: []scholar.PaperResult{}, Error: &scholar.ToolError{Code: scholar.CodeInvalidInput, Message: "query is required"}}, nil
	}
	if len(input.YearRange) != 0 && len(input.YearRange) != 2 {
		return errorResult(), SearchResponse{Results: []scholar.PaperResult{}, Error: &scholar.ToolError{Code: scholar.CodeInvalidInput, Message: "year_range must be [start,end]"}}, nil
	}
	ctx, cancel := s.withDeadline(ctx)
	defer cancel()
	return searchResponse(scholar.SearchAdvanced(ctx, s.requester, input.Query, input.Author, input.YearRange, sanitizeNumResults(input.NumResults)))
}

func searchResponse(outcome *scholar.SearchOutcome, toolErr *scholar.ToolError) (*mcp.CallToolResult, SearchResponse, error) {
	if toolErr != nil {
		attempts := toolErr.Attempts
		e := *toolErr
		e.Attempts = nil
		return errorResult(), SearchResponse{Results: []scholar.PaperResult{}, Attempts: attempts, Error: &e}, nil
	}
	resp := SearchResponse{Results: outcome.Results, Provider: outcome.Provider}
	if len(outcome.Attempts) > 1 {
		resp.Attempts = outcome.Attempts
	}
	return nil, resp, nil
}

func (s *Server) getAuthorInfo(ctx context.Context, _ *mcp.CallToolRequest, input GetAuthorInput) (*mcp.CallToolResult, AuthorResponse, error) {
	if strings.TrimSpace(input.AuthorName) == "" && strings.TrimSpace(input.ORCID) == "" && strings.TrimSpace(input.OpenAlexID) == "" {
		return errorResult(), AuthorResponse{Error: &scholar.ToolError{Code: scholar.CodeInvalidInput, Message: "author_name, orcid or openalex_id is required"}}, nil
	}
	ctx, cancel := s.withDeadline(ctx)
	defer cancel()

	author, toolErr := scholar.GetAuthorInfo(ctx, s.requester, scholar.AuthorQuery{
		Name:        input.AuthorName,
		ORCID:       input.ORCID,
		OpenAlexID:  input.OpenAlexID,
		Affiliation: input.Affiliation,
		KnownPaper:  input.KnownPaper,
	})
	if toolErr != nil {
		return errorResult(), AuthorResponse{Error: toolErr}, nil
	}
	return nil, AuthorResponse{Author: author}, nil
}

func (s *Server) getPaperContent(ctx context.Context, _ *mcp.CallToolRequest, input GetPaperContentInput) (*mcp.CallToolResult, PaperContentResponse, error) {
	req := fulltext.Request{
		URL:        input.URL,
		DOI:        input.DOI,
		ArxivID:    input.ArxivID,
		OpenAlexID: input.OpenAlexID,
		Title:      input.Title,
		Author:     input.Author,
		Year:       input.Year,
	}
	ctx, cancel := s.withDeadline(ctx)
	defer cancel()

	content, toolErr := s.fulltext.GetPaperContent(ctx, req, input.MaxChars, input.Offset)
	if toolErr != nil {
		return errorResult(), PaperContentResponse{Error: toolErr}, nil
	}
	return nil, PaperContentResponse{Content: content}, nil
}

func (s *Server) healthcheck(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, HealthResponse, error) {
	creds := s.requester.CredentialStatus()
	auth := func(configured bool, yes, no string) string {
		if configured {
			return yes
		}
		return no
	}
	pdftotextPath, pdftotextOK := fulltext.PdftotextAvailable(s.cfg)

	providers := map[string]ProviderCapability{
		"openalex": {
			Used:        []string{"search", "author_lookup", "paper_identity"},
			Credentials: auth(creds.OpenAlexAPIKey, "api_key", "none"),
			Note:        auth(creds.OpenAlexAPIKey, "", "OpenAlex requires a free API key for a usable daily budget; keyless use is heavily limited. Set OPENALEX_API_KEY."),
		},
		"crossref": {
			Used:        []string{"search", "author_fallback", "paper_identity_fallback"},
			Credentials: auth(creds.CrossrefPlusToken, "metadata_plus_token", auth(creds.ContactEmail, "polite_pool_mailto", "public_pool")),
		},
		"orcid": {
			Used:        []string{"author_lookup"},
			Credentials: auth(creds.ORCIDClient, "client_credentials", "anonymous"),
		},
		"unpaywall": {
			Used:        []string{"fulltext_fallback"},
			Credentials: auth(creds.ContactEmail, "email", "disabled"),
			Note:        auth(creds.ContactEmail, "", "Set SCHOLAR_CONTACT_EMAIL to enable the Unpaywall fallback."),
		},
		"google_scholar": {
			Used:        []string{"search", "author_fallback"},
			Credentials: "none (HTML scraping)",
			Note:        "Frequently blocked; paced by SCHOLAR_MIN_DELAY/SCHOLAR_MAX_DELAY.",
		},
	}

	return nil, HealthResponse{
		Status:  "ok",
		Message: fmt.Sprintf("%s %s is running and waiting for MCP client traffic on stdio", s.implName, s.implVer),
		Process: ProcessHealth{
			Name:          s.implName,
			Version:       s.implVer,
			GoVersion:     runtime.Version(),
			UptimeSeconds: int64(time.Since(s.started).Seconds()),
		},
		Capabilities: Capabilities{
			Note:            "Configuration only: no network calls are made, so providers may still be unreachable or rate limited.",
			SearchProviders: s.cfg.SearchProviders,
			Providers:       providers,
			Extractors: ExtractorCapabilities{
				Pdftotext:     pdftotextOK,
				PdftotextPath: pdftotextPath,
				GoPDF:         true,
				HTML:          true,
			},
			Limits: Limits{
				OperationTimeoutSeconds: int(s.cfg.OperationTimeout.Seconds()),
				MaxDocumentBytes:        s.cfg.MaxFetchBytes,
				MaxResponseBytes:        s.cfg.MaxResponseBytes,
				MaxExtractChars:         s.cfg.MaxExtractChars,
				CacheMaxBytes:           s.cfg.CacheMaxBytes,
			},
			ToolSet: s.cfg.ToolSet,
		},
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
