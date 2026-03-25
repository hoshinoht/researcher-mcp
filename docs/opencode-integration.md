# OpenCode Integration

This guide shows how to wire Researcher MCP into OpenCode using stdio MCP.

## 1. Build The MCP Binary

Run from the project root:

```bash
go mod tidy
go build -o researcher-mcp ./cmd/google-scholar-mcp
```

## 2. Add The Server To OpenCode Config

In your OpenCode config, add an entry under mcpServers.

Example:

```json
{
  "mcpServers": {
    "researcher-mcp": {
      "type": "stdio",
      "command": "/absolute/path/to/researcher-mcp",
      "args": [],
      "env": {
        "OPENALEX_API_KEY": "",
        "CROSSREF_API_KEY": "",
        "ORCID_CLIENT_ID": "",
        "ORCID_CLIENT_SECRET": ""
      }
    }
  }
}
```

Notes:

- API keys are optional. The server works without them.
- If you need higher usage limits or better throughput, add keys from OpenAlex, Crossref, and ORCID.

## 3. Restart OpenCode

Restart OpenCode so it reloads MCP server definitions.

## 4. Verify Tools Are Available

After startup, Researcher MCP tools should be discoverable by the assistant.

Preferred tool aliases:

- search_research_articles
- search_research_articles_advanced
- get_researcher_info
- researcher_mcp_healthcheck

Compatibility tool names are also supported:

- search_google_scholar_key_words
- search_google_scholar_advanced
- get_author_info
- google_scholar_healthcheck
