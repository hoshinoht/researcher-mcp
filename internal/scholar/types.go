package scholar

type PaperResult struct {
	Title            string `json:"title"`
	Authors          string `json:"authors"`
	Abstract         string `json:"abstract"`
	URL              string `json:"url"`
	DOI              string `json:"doi,omitempty"`
	Year             int    `json:"year,omitempty"`
	PDFURL           string `json:"pdf_url,omitempty"`
	SnippetTruncated bool   `json:"snippet_truncated"`
}

type Publication struct {
	Title     string `json:"title"`
	Year      string `json:"year"`
	Citations int    `json:"citations"`
}

type AuthorInfo struct {
	Name         string            `json:"name"`
	Affiliation  string            `json:"affiliation"`
	Interests    []string          `json:"interests"`
	CitedBy      int               `json:"citedby"`
	Publications []Publication     `json:"publications"`
	Source       string            `json:"source,omitempty"`
	ExternalIDs  map[string]string `json:"external_ids,omitempty"`
}

type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}
