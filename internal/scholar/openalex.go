package scholar

import (
	"regexp"
	"strings"
)

// OpenAlexLocation is one place a work is hosted.
type OpenAlexLocation struct {
	IsOA           bool   `json:"is_oa"`
	LandingPageURL string `json:"landing_page_url"`
	PDFURL         string `json:"pdf_url"`
	Version        string `json:"version"`
	License        string `json:"license"`
	Source         *struct {
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	} `json:"source"`
}

// OpenAlexAuthorship links a work to an author profile.
type OpenAlexAuthorship struct {
	Author struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		ORCID       string `json:"orcid"`
	} `json:"author"`
	RawAuthorName string `json:"raw_author_name"`
}

// OpenAlexWork is the subset of an OpenAlex work record used by search,
// paper identity resolution and author evidence.
type OpenAlexWork struct {
	ID              string `json:"id"`
	DOI             string `json:"doi"`
	DisplayName     string `json:"display_name"`
	Title           string `json:"title"`
	PublicationYear int    `json:"publication_year"`
	CitedByCount    *int   `json:"cited_by_count"`
	IDs             struct {
		OpenAlex string `json:"openalex"`
		DOI      string `json:"doi"`
		PMID     string `json:"pmid"`
		PMCID    string `json:"pmcid"`
	} `json:"ids"`
	Authorships           []OpenAlexAuthorship `json:"authorships"`
	AbstractInvertedIndex map[string][]int     `json:"abstract_inverted_index"`
	PrimaryLocation       *OpenAlexLocation    `json:"primary_location"`
	BestOALocation        *OpenAlexLocation    `json:"best_oa_location"`
	Locations             []OpenAlexLocation   `json:"locations"`
	OpenAccess            struct {
		IsOA     bool   `json:"is_oa"`
		OAStatus string `json:"oa_status"`
		OAURL    string `json:"oa_url"`
	} `json:"open_access"`
}

// DisplayTitle prefers display_name, falling back to title.
func (w OpenAlexWork) DisplayTitle() string {
	if t := normalizeSpace(w.DisplayName); t != "" {
		return t
	}
	return normalizeSpace(w.Title)
}

// AllLocations returns best OA, primary and listed locations in that order.
func (w OpenAlexWork) AllLocations() []OpenAlexLocation {
	out := make([]OpenAlexLocation, 0, len(w.Locations)+2)
	if w.BestOALocation != nil {
		out = append(out, *w.BestOALocation)
	}
	if w.PrimaryLocation != nil {
		out = append(out, *w.PrimaryLocation)
	}
	return append(out, w.Locations...)
}

// AuthorNames returns de-duplicated author display names in order.
func (w OpenAlexWork) AuthorNames() []string {
	names := make([]string, 0, len(w.Authorships))
	seen := map[string]bool{}
	for _, a := range w.Authorships {
		name := normalizeSpace(a.Author.DisplayName)
		if name == "" {
			name = normalizeSpace(a.RawAuthorName)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

var (
	openAlexIDPattern = regexp.MustCompile(`^(?i)(?:https?://(?:api\.)?openalex\.org/(?:works/|authors/)?)?([WA]\d+)$`)
	orcidPattern      = regexp.MustCompile(`(?i)^(?:https?://(?:www\.)?orcid\.org/)?(\d{4}-\d{4}-\d{4}-\d{3}[\dX])$`)
	doiPattern        = regexp.MustCompile(`^10\.\d{4,9}/\S+$`)
)

// NormalizeOpenAlexID returns the short form (W123 / A123) of an OpenAlex ID
// or URL, and whether it was valid.
func NormalizeOpenAlexID(raw string) (string, bool) {
	m := openAlexIDPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	return strings.ToUpper(m[1][:1]) + m[1][1:], true
}

// NormalizeORCID returns the bare 0000-0000-0000-000X form and validates the
// ISO 7064 Mod 11-2 checksum.
func NormalizeORCID(raw string) (string, bool) {
	m := orcidPattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(raw)))
	if m == nil {
		return "", false
	}
	id := m[1]
	digits := strings.ReplaceAll(id, "-", "")
	total := 0
	for _, r := range digits[:15] {
		total = (total + int(r-'0')) * 2
	}
	check := (12 - total%11) % 11
	want := byte('0' + check)
	if check == 10 {
		want = 'X'
	}
	if digits[15] != want {
		return "", false
	}
	return id, true
}

// NormalizeDOI strips resolver prefixes and lowercases a DOI (DOIs are case
// insensitive), returning whether it is well formed.
func NormalizeDOI(raw string) (string, bool) {
	doi := strings.TrimSpace(raw)
	lower := strings.ToLower(doi)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:"} {
		if strings.HasPrefix(lower, prefix) {
			doi = doi[len(prefix):]
			break
		}
	}
	doi = strings.ToLower(strings.TrimSpace(doi))
	if doiPattern.MatchString(doi) {
		return doi, true
	}
	return "", false
}
