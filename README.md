# Researcher MCP Server (Go)

Go-based MCP server for scholarly article search and researcher metadata retrieval.

This project evolved from a Google Scholar-focused port into a provider-first Researcher MCP that reduces reliance on fragile scraping and prefers free metadata sources.

## What This Server Provides

- Adds anti-blocking request behavior for Scholar access (delay jitter, retries, UA rotation, optional proxies).
- Uses generic article search fallback: Google Scholar first, then OpenAlex when Scholar is blocked or has no usable results.
- Returns structured tool errors with explicit error codes.
- Preserves existing tool names for compatibility with existing MCP clients.
- Adds a healthcheck tool so users can verify the server is alive.
- Adds Docker packaging for easier deployment.
- Uses provider-first researcher lookup: OpenAlex -> Crossref -> ORCID -> Scholar fallback.

## MCP Tools

- search_google_scholar_key_words
	- Input: query, num_results
	- Output: results array and optional error object
- search_google_scholar_advanced
	- Input: query, author, year_range, num_results
	- Output: results array and optional error object
- get_author_info
	- Input: author_name
	- Output: author object and optional error object
	- Source order: OpenAlex, Crossref, ORCID, then Google Scholar fallback
- google_scholar_healthcheck
	- Input: none
	- Output: server status message

## Error Codes

The tools return structured errors in the response payload:

- invalid_input
- blocked
- no_results
- parse_failed
- upstream_error

## Quick Start

### Prerequisites

- Go 1.23+

### Build

```bash
go mod tidy
go build -o researcher-mcp ./cmd/google-scholar-mcp
```

### Run (stdio MCP server)

```bash
./researcher-mcp
```

Note: no output is expected during idle operation. The process waits for MCP client traffic over stdio.

## MCP Client Configuration Example

```json
{
	"mcpServers": {
		"researcher-mcp": {
			"command": "/absolute/path/to/researcher-mcp",
			"args": []
		}
	}
}
```

## OpenCode Integration

If you are using OpenCode, see the integration guide:

- docs/opencode-integration.md

Quick OpenCode mcpServers example:

```json
{
	"mcpServers": {
		"researcher-mcp": {
			"type": "stdio",
			"command": "/absolute/path/to/researcher-mcp",
			"args": []
		}
	}
}
```

## Anti-Blocking Configuration

Copy .env.example values into your runtime environment as needed:

- SCHOLAR_MIN_DELAY
- SCHOLAR_MAX_DELAY
- SCHOLAR_MAX_RETRIES
- SCHOLAR_BACKOFF_FACTOR
- SCHOLAR_ROTATE_USER_AGENTS
- SCHOLAR_USER_AGENTS
- SCHOLAR_PROXY_LIST
- SCHOLAR_TIMEOUT_SECONDS

## API Keys And Rate Limits

Researcher MCP works without API credentials, but provider-side anonymous usage can be rate-limited.

For higher usage limits or better throughput, set optional credentials from each provider in your environment:

- OPENALEX_API_KEY
- CROSSREF_API_KEY
- ORCID_CLIENT_ID
- ORCID_CLIENT_SECRET

You can obtain these from the related provider developer portals:

- OpenAlex
- Crossref
- ORCID

## Docker

Build image:

```bash
docker build -t researcher-mcp-go .
```

Run image:

```bash
docker run --rm -i researcher-mcp-go
```

Preferred tool aliases:

- search_research_articles
- search_research_articles_advanced
- get_researcher_info
- researcher_mcp_healthcheck

## Development

Run tests:

```bash
go test ./...
```

Run live integration tests (uses Google Scholar network calls):

```bash
SCHOLAR_LIVE_TESTS=1 go test -tags=integration ./integration -v
```

The integration suite uses citation-derived query fixtures based on IEEE-style references (healthcare, microservices, cloud native, and ML papers/books) to validate:

- keyword search returns records
- advanced search works with author and year filters
- author lookup resolves citation-linked authors

## Disclaimer

Use responsibly and comply with Google Scholar terms of service and applicable laws.
