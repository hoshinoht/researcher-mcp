package scholar

import (
	"context"
	"errors"
	"strings"
)

type PaperResult struct {
	Title            string `json:"title"`
	Authors          string `json:"authors"`
	Abstract         string `json:"abstract"`
	URL              string `json:"url"`
	DOI              string `json:"doi,omitempty"`
	Year             int    `json:"year,omitempty"`
	PDFURL           string `json:"pdf_url,omitempty"`
	SnippetTruncated bool   `json:"snippet_truncated"`
	// Source is the provider that produced this record: openalex, crossref
	// or google_scholar.
	Source     string `json:"source"`
	OpenAlexID string `json:"openalex_id,omitempty"`
}

// Publication is one work in an author's publication sample. Citations is
// null when the provider did not report a count.
type Publication struct {
	Title     string `json:"title"`
	Year      string `json:"year"`
	Citations *int   `json:"citations"`
	DOI       string `json:"doi,omitempty"`
}

// AuthorMetrics are author-level totals reported by a provider's author
// profile. Fields are null when the provider does not report them; they are
// never derived from a sample of works.
type AuthorMetrics struct {
	CitedByCount *int   `json:"cited_by_count"`
	WorksCount   *int   `json:"works_count"`
	HIndex       *int   `json:"h_index"`
	I10Index     *int   `json:"i10_index"`
	Source       string `json:"source"`
	Scope        string `json:"scope"`
	Note         string `json:"note,omitempty"`
}

// PublicationSample describes how the publications list was selected, so
// per-work numbers are not mistaken for author totals.
type PublicationSample struct {
	Scope  string `json:"scope"`
	Source string `json:"source"`
	Count  int    `json:"count"`
	Note   string `json:"note,omitempty"`
}

// MatchInfo explains how an identity (author or paper) was resolved.
type MatchInfo struct {
	Method     string   `json:"method"`
	Confidence string   `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"`
}

type AuthorInfo struct {
	Name        string   `json:"name"`
	Affiliation string   `json:"affiliation"`
	Interests   []string `json:"interests"`
	// CitedBy is the author-level total citation count from Metrics, or
	// null when no author profile reported one. Kept for compatibility.
	CitedBy           *int               `json:"citedby"`
	Metrics           AuthorMetrics      `json:"metrics"`
	Publications      []Publication      `json:"publications"`
	PublicationSample *PublicationSample `json:"publication_sample,omitempty"`
	Source            string             `json:"source,omitempty"`
	ExternalIDs       map[string]string  `json:"external_ids,omitempty"`
	Match             *MatchInfo         `json:"match,omitempty"`
	Warnings          []string           `json:"warnings,omitempty"`
}

// Candidate is one possible identity returned with an "ambiguous" or
// "identifier_conflict" error.
type Candidate struct {
	ID           string   `json:"id,omitempty"`
	Label        string   `json:"label"`
	Year         int      `json:"year,omitempty"`
	DOI          string   `json:"doi,omitempty"`
	ArxivID      string   `json:"arxiv_id,omitempty"`
	ORCID        string   `json:"orcid,omitempty"`
	Authors      string   `json:"authors,omitempty"`
	Affiliation  string   `json:"affiliation,omitempty"`
	WorksCount   *int     `json:"works_count,omitempty"`
	CitedByCount *int     `json:"cited_by_count,omitempty"`
	Score        float64  `json:"score,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
	Source       string   `json:"source,omitempty"`
}

// Attempt records one provider or candidate step, for diagnosing fallbacks.
type Attempt struct {
	Provider string `json:"provider"`
	Stage    string `json:"stage"`
	URL      string `json:"url,omitempty"`
	Outcome  string `json:"outcome"`
	Status   int    `json:"status,omitempty"`
	Message  string `json:"message,omitempty"`
}

// MaxAttempts bounds attempt histories returned to clients.
const MaxAttempts = 20

// AppendAttempt appends a redacted attempt, keeping the history bounded.
func AppendAttempt(attempts []Attempt, a Attempt) []Attempt {
	a.URL = RedactURL(a.URL)
	a.Message = RedactText(a.Message)
	if len(attempts) >= MaxAttempts {
		return attempts
	}
	return append(attempts, a)
}

type ToolError struct {
	Code              string      `json:"code"`
	Message           string      `json:"message"`
	Hint              string      `json:"hint,omitempty"`
	Retryable         bool        `json:"retryable,omitempty"`
	RetryAfterSeconds int         `json:"retry_after_seconds,omitempty"`
	Candidates        []Candidate `json:"candidates,omitempty"`
	Attempts          []Attempt   `json:"attempts,omitempty"`
}

// Error codes beyond the original set.
const (
	CodeInvalidInput       = "invalid_input"
	CodeBlocked            = "blocked"
	CodeNoResults          = "no_results"
	CodeParseFailed        = "parse_failed"
	CodeUpstreamError      = "upstream_error"
	CodeAmbiguous          = "ambiguous"
	CodeIdentifierConflict = "identifier_conflict"
	CodeAccessRestricted   = "access_restricted"
	CodeTimeout            = "timeout"
	CodeCancelled          = "cancelled"
)

// errorRank orders failures by how informative they are to a caller: a
// specific identity problem beats an access wall, which beats a transport
// failure, which beats "nothing found".
var errorRank = map[string]int{
	CodeIdentifierConflict: 90,
	CodeAmbiguous:          85,
	CodeInvalidInput:       80,
	CodeAccessRestricted:   70,
	CodeBlocked:            60,
	CodeParseFailed:        50,
	CodeTimeout:            45,
	CodeUpstreamError:      40,
	CodeCancelled:          30,
	CodeNoResults:          10,
}

// MoreInformative reports whether a should replace b as the error to report.
func MoreInformative(a, b *ToolError) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return errorRank[a.Code] > errorRank[b.Code]
}

// ContextError converts a context failure into a structured error, or
// returns nil when err is not a context error.
func ContextError(err error, what string) *ToolError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &ToolError{Code: CodeTimeout, Message: strings.TrimSpace(what + " exceeded the operation deadline"), Hint: "Retry, or raise RESEARCHER_OPERATION_TIMEOUT_SECONDS.", Retryable: true}
	case errors.Is(err, context.Canceled):
		return &ToolError{Code: CodeCancelled, Message: strings.TrimSpace(what + " was cancelled")}
	default:
		return nil
	}
}

// IntPtr returns a pointer to v.
func IntPtr(v int) *int { return &v }
