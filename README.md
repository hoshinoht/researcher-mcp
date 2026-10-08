# Researcher MCP Server (Go)

Go-based MCP server for scholarly article search and researcher metadata retrieval.

This project evolved from a Google Scholar-focused port into a provider-first Researcher MCP that reduces reliance on fragile scraping and prefers free metadata sources.

## What This Server Provides

- Configurable article search across OpenAlex, Crossref and Google Scholar. Every result reports its `source`, and each response reports the `provider` that answered (plus `attempts` when earlier providers failed).
- A query made only of two or more double-quoted titles separated by whitespace (for example, `"Title One" "Title Two"`) is searched as an OpenAlex title batch when OpenAlex is enabled; Boolean operators and mixed text remain ordinary queries.
- Researcher lookup that prefers exact identifiers (ORCID, OpenAlex author ID) and returns an `ambiguous` error with candidates instead of guessing between same-name people.
- Paper full text with explicit identity resolution, content classification (`full_text`, `unverified`, `partial`, `abstract_only`, `landing_page`), independent fallbacks and page/section provenance.
- Structured errors with explicit codes, also flagged as MCP tool errors (`isError: true`).
- Bounded networking: response size limits, per-provider pacing, Retry-After handling, retries only for transient failures, and a per-call deadline.
- A healthcheck that separates process health from configured capabilities without making network calls.
- Preserves existing tool names for compatibility with existing MCP clients; the duplicate aliases can be switched off.
- Docker packaging.

## MCP Tools

Every tool exists under a legacy name and a preferred alias (see [Tool catalog](#tool-catalog)).

- `search_google_scholar_key_words` / `search_research_articles`
	- Input: `query`, `num_results`
	- Output: `results` (each with `source`), `provider`, optional `attempts`, optional `error`
- `search_google_scholar_advanced` / `search_research_articles_advanced`
	- Input: `query`, `author`, `year_range`, `num_results`
	- Output: as above
- `get_author_info` / `get_researcher_info`
	- Input: `author_name` (one person; "Surname, Given" is accepted), or exact `orcid` / `openalex_id`; optional evidence `affiliation` and `known_paper` (DOI or exact title)
	- Output: `author` object or `error`
	- Exact identifiers are looked up directly; a supplied ORCID that disagrees with the OpenAlex profile is an `identifier_conflict`.
	- Name lookups resolve only when evidence singles out one OpenAlex profile (a known paper's authorship, a matching affiliation, or a single name-compatible profile). Otherwise the error is `ambiguous` with `candidates` (ID, ORCID, affiliation, counts). Candidate order is not identity evidence; citation counts are never used to choose.
	- Fallbacks when OpenAlex has no match or fails: ORCID, then Google Scholar, then Crossref.
	- `match` explains the method, confidence (`high`/`medium`/`low`) and evidence; `warnings` lists caveats.
- `get_paper_fulltext` / `read_research_paper`
	- Input: `url`, `doi`, `arxiv_id`, `openalex_id`, or `title` (at least one); optional `author`, `year` (evidence for titles); `max_chars`, `offset` for pagination
	- Output: `content` or `error` (see [Full-text behaviour](#full-text-behaviour))
- `google_scholar_healthcheck` / `researcher_mcp_healthcheck`
	- Output: `status`, `process` (version, uptime), `capabilities` (search order, provider credential modes, extractors, limits, tool set). No network calls are made, so capabilities describe configuration, not reachability.

## Full-text behaviour

**Identity.** Exact identifiers win over titles. A title is matched against up to 10 OpenAlex candidates by normalized title, with optional author and year evidence. Records with the same title and overlapping authors are treated as versions of one paper (their locations are all tried). Different papers sharing a title, or no sufficiently close title, produce an `ambiguous` error with candidates; supplied identifiers that disagree (DOI vs arXiv ID, DOI vs title, URL vs DOI) produce `identifier_conflict`. If OpenAlex is unavailable, Crossref is used to identify the DOI. `content.identity` returns the resolved DOI / OpenAlex / arXiv / PMCID identifiers and the match evidence.

**Sources.** Candidates are de-duplicated and tried by priority: direct URL, arXiv (HTML, ar5iv, PDF), PubMed Central, open-access PDFs (published > accepted > submitted versions), open-access landing pages, then the DOI landing page. Unpaywall (requires `SCHOLAR_CONTACT_EMAIL`) is consulted independently once only landing pages remain or every candidate has failed, including when OpenAlex itself is down. Full-text pointers on pages (`citation_pdf_url`, `bepress_citation_pdf_url`, `eprints.document_url`, `link rel="alternate" type="application/pdf"`, `citation_fulltext_html_url`) are followed whenever a page is not already full text, however long it is. At most 8 documents are downloaded per call.

**Classification.** Text is returned as `full_text` only with structural evidence (body sections, a reference list with sections, a LaTeXML article, or a multi-page PDF). Login/purchase walls (`access_restricted`) and bot challenges (`blocked`) are errors, never content. If no full text is found, the best partial result is returned with `content_status` (`unverified`, `partial`, `abstract_only`, `landing_page`), a `Content status` line in the markdown header and `warnings`; as a last resort the OpenAlex abstract is returned as `abstract_only`. When everything fails, the most informative error is returned with a bounded `attempts` history.

**Output.** `document_id` is a stable scholarly identifier (`doi:`, `arxiv:`, `openalex:`, `pmcid:` or `url:`); `content_id` hashes the extracted text. `provenance` gives the page range of the returned window for PDFs (pages are also marked `<!-- page N -->`), the section in effect at `offset`, and headings inside the window with their offsets. HTML conversion keeps tables as markdown tables and math as `$...$` / `$$...$$` TeX when the source provides it; `limitations` lists what could not be preserved (math without TeX, figures, merged table cells, PDF equations/tables). PDFs use poppler's `pdftotext` when available, otherwise a pure-Go extractor (`converter` reports which).

## Error Codes

Errors are returned in the payload's `error` object and the MCP result has `isError: true`. Codes:

- `invalid_input`
- `ambiguous` — several identities fit; see `candidates`
- `identifier_conflict` — supplied identifiers disagree; see `candidates`
- `access_restricted` — paywall or login page
- `blocked` — rate limited or bot-challenged (`retryable`, optional `retry_after_seconds`)
- `no_results`
- `parse_failed`
- `upstream_error`
- `timeout` — the operation deadline (`RESEARCHER_OPERATION_TIMEOUT_SECONDS`) expired
- `cancelled`

## Quick Start

### Prerequisites

- Go 1.25+ (matches go.mod)
- Optional: poppler `pdftotext` for better PDF extraction

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

## Configuration

Copy `.env.example` values into your runtime environment as needed.

Pacing, retries and limits:

- `SCHOLAR_MIN_DELAY`, `SCHOLAR_MAX_DELAY` (seconds): jittered spacing for Google Scholar only
- `RESEARCHER_API_MIN_INTERVAL_MS` (default 150): spacing for OpenAlex, Crossref, ORCID and Unpaywall
- `RESEARCHER_DOCUMENT_MIN_INTERVAL_MS` (default 1000): spacing for every other host
- `SCHOLAR_MAX_RETRIES`, `SCHOLAR_BACKOFF_FACTOR`: retries for transient failures only (network errors, 429/502/503/504; Scholar rate limits are never retried)
- `RESEARCHER_MAX_RETRY_AFTER_SECONDS` (default 30): a longer `Retry-After` fails fast with `retry_after_seconds`
- `RESEARCHER_OPERATION_TIMEOUT_SECONDS` (default 120): deadline for one tool call; retries and waits never exceed it
- `SCHOLAR_TIMEOUT_SECONDS`: per-request timeout
- `SCHOLAR_ROTATE_USER_AGENTS`, `SCHOLAR_USER_AGENTS`, `SCHOLAR_PROXY_LIST`: browser user agents and proxies (metadata APIs get an honest `researcher-mcp` user agent)

Search and tools:

- `RESEARCHER_SEARCH_PROVIDERS`: comma-separated order from `openalex`, `crossref`, `scholar`. Default: `openalex,crossref,scholar` when `OPENALEX_API_KEY` is set, otherwise `scholar,openalex,crossref`.
- `RESEARCHER_TOOL_SET`: see [Tool catalog](#tool-catalog).

Full text:

- `SCHOLAR_CONTACT_EMAIL`: enables the Unpaywall fallback (Unpaywall requires an email) and is sent to Crossref as `mailto` (polite pool). It is not sent to OpenAlex, which retired its mailto polite pool.
- `SCHOLAR_MAX_FETCH_MB` (default 30): max document download size
- `RESEARCHER_MAX_RESPONSE_MB` (default 8): max metadata/API response size
- `RESEARCHER_MAX_EXTRACT_CHARS` (default 2,000,000): max characters kept per document
- `RESEARCHER_CACHE_MAX_MB` (default 64): memory budget for cached extractions (also max 16 entries; 15 min for full text, 2 min for partial results). Concurrent identical requests share one fetch.
- `SCHOLAR_PDFTOTEXT_PATH`: path to poppler's pdftotext; empty auto-detects on PATH, "off" forces pure-Go extraction

### Tool catalog

`RESEARCHER_TOOL_SET=all` (default) registers both the legacy names and the preferred aliases, as before. `preferred` registers only `search_research_articles`, `search_research_articles_advanced`, `get_researcher_info`, `read_research_paper`, `researcher_mcp_healthcheck`; `legacy` registers only the original names.

## Provider Credentials

Each credential is attached only to HTTPS requests for its own provider's API host. It is injected per request hop, so redirects to other hosts never carry it, and it is redacted from error messages and attempt histories. The healthcheck reports only whether a credential is configured.

| Variable | Provider | How it is used |
| --- | --- | --- |
| `OPENALEX_API_KEY` | OpenAlex | `api_key` query parameter. OpenAlex requires a (free) key for a usable daily budget; keyless calls are heavily limited. |
| `CROSSREF_API_KEY` | Crossref Metadata Plus (paid) | `Crossref-Plus-API-Token: Bearer <key>` header. Not needed for normal use. |
| `ORCID_CLIENT_ID`, `ORCID_CLIENT_SECRET` | ORCID Public API (free registration) | Exchanged once at `https://orcid.org/oauth/token` (client credentials, `/read-public` scope) for a bearer token sent to `pub.orcid.org`. Without them, ORCID is queried anonymously. |
| `SCHOLAR_CONTACT_EMAIL` | Unpaywall, Crossref | Unpaywall `email` parameter (required); Crossref `mailto` (polite pool). |

## Docker

Build image:

```bash
docker build -t researcher-mcp-go .
```

Run image:

```bash
docker run --rm -i researcher-mcp-go
```

## Development

Run tests:

```bash
go test ./...
```

Unit tests are deterministic and make no network calls (HTTP is faked). Run them with the race detector too:

```bash
go test -race ./...
```

Live integration tests are opt-in and call real providers (Google Scholar, OpenAlex, Crossref, ORCID, arXiv, publishers) with your environment's configuration:

```bash
SCHOLAR_LIVE_TESTS=1 go test -tags=integration ./integration -v
```

The integration suite uses citation-derived query fixtures based on IEEE-style references (healthcare, microservices, cloud native, and ML papers/books) to validate:

- keyword search returns records (and logs which provider answered)
- advanced search works with author and year filters
- author lookup resolves citation-linked authors, or reports same-name candidates
- full text from arXiv, a DOI and a title is classified as `full_text`

## Disclaimer

Use responsibly and comply with Google Scholar terms of service and applicable laws.
