package fulltext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"googlescholar-mcp-go/internal/scholar"
)

// Request identifies a paper. Exact identifiers (URL, arXiv ID, DOI,
// OpenAlex work ID) take precedence over a title; Author and Year are
// optional evidence for title matching and consistency checks.
type Request struct {
	URL        string
	DOI        string
	ArxivID    string
	OpenAlexID string
	Title      string
	Author     string
	Year       int
}

func (r Request) empty() bool {
	return strings.TrimSpace(r.URL) == "" && strings.TrimSpace(r.DOI) == "" && strings.TrimSpace(r.ArxivID) == "" &&
		strings.TrimSpace(r.OpenAlexID) == "" && strings.TrimSpace(r.Title) == ""
}

// PaperIdentity is what the request resolved to, with match evidence.
type PaperIdentity struct {
	DOI        string            `json:"doi,omitempty"`
	OpenAlexID string            `json:"openalex_id,omitempty"`
	ArxivID    string            `json:"arxiv_id,omitempty"`
	PMCID      string            `json:"pmcid,omitempty"`
	Title      string            `json:"title,omitempty"`
	Year       int               `json:"year,omitempty"`
	Authors    []string          `json:"authors,omitempty"`
	Match      scholar.MatchInfo `json:"match"`
}

// candidate is one full-text location to try.
type candidate struct {
	URL      string
	Source   string // user_url, arxiv, pmc, openalex, unpaywall, doi_landing, embedded
	Kind     string // pdf, html, landing, unknown
	Version  string // publishedVersion, acceptedVersion, submittedVersion
	Priority int
}

// resolution is the outcome of identity resolution: who the paper is and
// where its full text may be.
type resolution struct {
	Identity   PaperIdentity
	Candidates []candidate
	Abstract   string
	Attempts   []scholar.Attempt
	Warnings   []string
	// unpaywallDOI is consulted lazily once higher-priority candidates fail.
	unpaywallDOI string
}

const (
	priorityUserURL      = 100
	priorityArxivHTML    = 90
	priorityAr5iv        = 88
	priorityArxivPDF     = 86
	priorityPMC          = 82
	priorityOAPublished  = 76
	priorityOAAccepted   = 74
	priorityOASubmitted  = 72
	priorityOAURL        = 60
	priorityOALanding    = 50
	priorityDOILanding   = 20
	unpaywallBeforeBelow = 55

	titleSearchSize = 10
	maxListedTitles = 5
)

var (
	arxivNewIDPattern = regexp.MustCompile(`^\d{4}\.\d{4,5}(v\d+)?$`)
	arxivOldIDPattern = regexp.MustCompile(`^[a-z-]+(\.[A-Z]{2})?/\d{7}(v\d+)?$`)
	arxivVersion      = regexp.MustCompile(`v\d+$`)
)

func invalid(msg, hint string) *scholar.ToolError {
	return &scholar.ToolError{Code: scholar.CodeInvalidInput, Message: msg, Hint: hint}
}

func conflict(msg string, cands ...scholar.Candidate) *scholar.ToolError {
	return &scholar.ToolError{
		Code:       scholar.CodeIdentifierConflict,
		Message:    msg,
		Hint:       "The supplied identifiers point at different papers. Check them and supply the one you mean.",
		Candidates: cands,
	}
}

// normalized holds validated request identifiers.
type normalized struct {
	contentURL string
	doi        string
	arxiv      string
	openalex   string
	title      string
	author     string
	year       int
}

func normalizeRequest(req Request) (*normalized, *scholar.ToolError) {
	n := &normalized{title: strings.TrimSpace(req.Title), author: strings.TrimSpace(req.Author), year: req.Year}
	if v := strings.TrimSpace(req.DOI); v != "" {
		doi, ok := normalizeDOI(v)
		if !ok {
			return nil, invalid(fmt.Sprintf("invalid DOI: %q", req.DOI), "Expected a DOI like 10.1038/nature14539 (with or without a https://doi.org/ prefix).")
		}
		n.doi = doi
	}
	if v := strings.TrimSpace(req.ArxivID); v != "" {
		id, ok := normalizeArxivID(v)
		if !ok {
			return nil, invalid(fmt.Sprintf("invalid arXiv identifier: %q", req.ArxivID), "Expected forms like 1706.03762, 2401.00001v2, or cs/9901001.")
		}
		n.arxiv = id
	}
	if v := strings.TrimSpace(req.OpenAlexID); v != "" {
		id, ok := scholar.NormalizeOpenAlexID(v)
		if !ok || !strings.HasPrefix(id, "W") {
			return nil, invalid(fmt.Sprintf("invalid OpenAlex work ID: %q", req.OpenAlexID), "Expected W followed by digits, e.g. W2194775991.")
		}
		n.openalex = id
	}

	if raw := strings.TrimSpace(req.URL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, invalid(fmt.Sprintf("invalid URL: %q", raw), "Provide an absolute http(s) URL.")
		}
		host := strings.ToLower(u.Hostname())
		switch {
		case host == "doi.org" || host == "dx.doi.org":
			doi, ok := normalizeDOI(strings.TrimPrefix(u.Path, "/"))
			if !ok {
				return nil, invalid(fmt.Sprintf("URL does not contain a valid DOI: %q", raw), "")
			}
			if n.doi != "" && n.doi != doi {
				return nil, conflict(fmt.Sprintf("url names DOI %s but doi is %s", doi, n.doi),
					scholar.Candidate{ID: "doi:" + doi, Label: "DOI from url", DOI: doi, Source: "input"},
					scholar.Candidate{ID: "doi:" + n.doi, Label: "doi", DOI: n.doi, Source: "input"})
			}
			n.doi = doi
		case isArxivHost(host):
			if id, ok := arxivIDFromURLPath(u.Path); ok {
				if n.arxiv != "" && !sameArxiv(n.arxiv, id) {
					return nil, conflict(fmt.Sprintf("url names arXiv %s but arxiv_id is %s", id, n.arxiv),
						scholar.Candidate{ID: "arxiv:" + id, Label: "arXiv ID from url", ArxivID: id, Source: "input"},
						scholar.Candidate{ID: "arxiv:" + n.arxiv, Label: "arxiv_id", ArxivID: n.arxiv, Source: "input"})
				}
				n.arxiv = id
			} else {
				n.contentURL = raw
			}
		case host == "openalex.org" || host == "api.openalex.org":
			id, ok := scholar.NormalizeOpenAlexID(raw)
			if !ok || !strings.HasPrefix(id, "W") {
				return nil, invalid(fmt.Sprintf("URL is not an OpenAlex work: %q", raw), "")
			}
			if n.openalex != "" && n.openalex != id {
				return nil, conflict(fmt.Sprintf("url names OpenAlex %s but openalex_id is %s", id, n.openalex))
			}
			n.openalex = id
		default:
			n.contentURL = raw
		}
	}

	// arXiv-minted DOIs encode the arXiv ID.
	if strings.HasPrefix(n.doi, "10.48550/arxiv.") {
		implied := strings.TrimPrefix(n.doi, "10.48550/arxiv.")
		if n.arxiv != "" && !sameArxiv(n.arxiv, implied) {
			return nil, conflict(fmt.Sprintf("DOI %s is arXiv %s but arxiv_id is %s", n.doi, implied, n.arxiv),
				scholar.Candidate{ID: "doi:" + n.doi, Label: "doi", DOI: n.doi, ArxivID: implied, Source: "input"},
				scholar.Candidate{ID: "arxiv:" + n.arxiv, Label: "arxiv_id", ArxivID: n.arxiv, Source: "input"})
		}
		if n.arxiv == "" {
			if id, ok := normalizeArxivID(implied); ok {
				n.arxiv = id
			}
		}
	}
	return n, nil
}

func resolve(ctx context.Context, fetcher DocFetcher, contactEmail string, req Request) (*resolution, *scholar.ToolError) {
	if req.empty() {
		return nil, invalid("at least one of url, doi, arxiv_id, openalex_id, or title is required", "")
	}
	n, toolErr := normalizeRequest(req)
	if toolErr != nil {
		return nil, toolErr
	}

	res := &resolution{}
	id := &res.Identity
	id.DOI, id.ArxivID, id.OpenAlexID = n.doi, n.arxiv, n.openalex

	if n.contentURL != "" {
		res.add(candidate{URL: n.contentURL, Source: "user_url", Kind: "unknown", Priority: priorityUserURL})
	}
	if n.arxiv != "" {
		res.addArxiv(n.arxiv)
	}

	var openAlexErr *scholar.ToolError
	switch {
	case n.openalex != "" || n.doi != "":
		key := n.openalex
		if key == "" {
			key = "https://doi.org/" + n.doi
		}
		var work scholar.OpenAlexWork
		status, fetchErr := fetchJSON(ctx, fetcher, "https://api.openalex.org/works/"+key, &work)
		res.Attempts = scholar.AppendAttempt(res.Attempts, attemptFor("openalex", "resolve", "https://api.openalex.org/works/"+key, status, fetchErr))
		switch {
		case fetchErr == nil:
			if err := res.checkWorkConsistency(n, work); err != nil {
				return nil, err
			}
			res.applyWork(work, true)
		case isContextErr(fetchErr):
			return nil, fetchErr
		case status == http.StatusNotFound && n.openalex != "" && n.doi == "" && n.arxiv == "" && n.contentURL == "":
			return nil, &scholar.ToolError{Code: scholar.CodeNoResults, Message: "OpenAlex work not found: " + n.openalex}
		default:
			openAlexErr = fetchErr
			res.Warnings = append(res.Warnings, "OpenAlex lookup failed ("+fetchErr.Message+"); continuing with other sources")
		}
		if n.openalex != "" && id.DOI == "" && n.doi != "" {
			id.DOI = n.doi
		}
		method := "doi"
		if n.openalex != "" {
			method = "openalex_id"
		}
		if n.contentURL != "" {
			method = "url+" + method
		}
		id.Match = scholar.MatchInfo{Method: method, Confidence: "high", Evidence: []string{"exact identifier supplied"}}
		if id.Title == "" && n.title != "" {
			id.Title = n.title
		}
	case n.arxiv != "":
		id.Match = scholar.MatchInfo{Method: "arxiv_id", Confidence: "high", Evidence: []string{"exact identifier supplied"}}
		id.Title = n.title
	case n.contentURL != "":
		id.Match = scholar.MatchInfo{Method: "url", Confidence: "high", Evidence: []string{"direct URL supplied; paper identity not verified"}}
		id.Title = n.title
	default:
		if err := res.resolveTitle(ctx, fetcher, n); err != nil {
			return nil, err
		}
	}

	if id.DOI != "" {
		res.add(candidate{URL: "https://doi.org/" + id.DOI, Source: "doi_landing", Kind: "landing", Priority: priorityDOILanding})
		if contactEmail != "" {
			res.unpaywallDOI = id.DOI
		}
	}

	if len(res.Candidates) == 0 && res.unpaywallDOI == "" {
		if openAlexErr != nil {
			openAlexErr.Attempts = res.Attempts
			return nil, openAlexErr
		}
		return nil, &scholar.ToolError{
			Code:     scholar.CodeNoResults,
			Message:  "no open-access full-text location found",
			Hint:     "The paper may be paywalled. Provide a direct PDF URL if you have access, or set SCHOLAR_CONTACT_EMAIL to enable the Unpaywall fallback.",
			Attempts: res.Attempts,
		}
	}
	return res, nil
}

func isContextErr(e *scholar.ToolError) bool {
	return e != nil && (e.Code == scholar.CodeTimeout || e.Code == scholar.CodeCancelled)
}

// checkWorkConsistency flags supplied identifiers that disagree with the
// OpenAlex record fetched by the strongest identifier.
func (res *resolution) checkWorkConsistency(n *normalized, work scholar.OpenAlexWork) *scholar.ToolError {
	workDOI, _ := normalizeDOI(work.DOI)
	workID, _ := scholar.NormalizeOpenAlexID(work.ID)
	workCand := scholar.Candidate{ID: workID, Label: work.DisplayTitle(), Year: work.PublicationYear, DOI: workDOI, Source: "openalex"}

	if n.openalex != "" && n.doi != "" && workDOI != "" && workDOI != n.doi {
		return conflict(fmt.Sprintf("OpenAlex work %s has DOI %s, but doi %s was supplied", n.openalex, workDOI, n.doi),
			workCand, scholar.Candidate{ID: "doi:" + n.doi, Label: "doi", DOI: n.doi, Source: "input"})
	}
	if n.arxiv != "" {
		workArxiv := arxivIDsFromWork(work)
		if len(workArxiv) > 0 {
			matched := false
			for _, a := range workArxiv {
				if sameArxiv(a, n.arxiv) {
					matched = true
				}
			}
			if !matched {
				workCand.ArxivID = workArxiv[0]
				return conflict(fmt.Sprintf("the work identified by %s has arXiv version %s, but arxiv_id %s was supplied", firstNonEmpty(n.openalex, n.doi), workArxiv[0], n.arxiv),
					workCand, scholar.Candidate{ID: "arxiv:" + n.arxiv, Label: "arxiv_id", ArxivID: n.arxiv, Source: "input"})
			}
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not verify that arXiv %s is the same paper as %s (no arXiv location recorded)", n.arxiv, firstNonEmpty(n.doi, n.openalex)))
		}
	}
	if n.title != "" {
		if sim := scholar.TitleSimilarity(n.title, work.DisplayTitle()); sim < 0.5 {
			return conflict(fmt.Sprintf("title %q does not match %q identified by %s (similarity %.2f)", n.title, work.DisplayTitle(), firstNonEmpty(n.openalex, n.doi), sim),
				workCand, scholar.Candidate{Label: n.title, Source: "input"})
		}
	}
	if n.author != "" && len(work.AuthorNames()) > 0 && !authorsInclude(work.AuthorNames(), n.author) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("author %q is not among the recorded authors of %q", n.author, work.DisplayTitle()))
	}
	if n.year != 0 && work.PublicationYear != 0 && abs(work.PublicationYear-n.year) > 1 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("year %d differs from the recorded publication year %d", n.year, work.PublicationYear))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func authorsInclude(names []string, requested string) bool {
	for _, name := range names {
		if scholar.CompareNames(requested, name) != scholar.NameMismatch {
			return true
		}
	}
	// A bare surname ("Vaswani") is common evidence.
	want := scholar.NormalizeTitle(requested)
	for _, name := range names {
		if scholar.FamilyName(name) == want {
			return true
		}
	}
	return false
}

// applyWork records identity fields and full-text locations from a work.
// primary works fill identity; version works only contribute locations.
func (res *resolution) applyWork(work scholar.OpenAlexWork, primary bool) {
	id := &res.Identity
	if primary {
		if doi, ok := normalizeDOI(work.DOI); ok && id.DOI == "" {
			id.DOI = doi
		}
		if oa, ok := scholar.NormalizeOpenAlexID(work.ID); ok && id.OpenAlexID == "" {
			id.OpenAlexID = oa
		}
		if id.Title == "" {
			id.Title = work.DisplayTitle()
		}
		if id.Year == 0 {
			id.Year = work.PublicationYear
		}
		if len(id.Authors) == 0 {
			id.Authors = firstN(work.AuthorNames(), 10)
		}
		if res.Abstract == "" {
			res.Abstract = abstractFromInvertedIndex(work.AbstractInvertedIndex)
		}
	}
	if pmcid := strings.TrimSpace(work.IDs.PMCID); pmcid != "" {
		pmcid = pmcid[strings.LastIndex(pmcid, "/")+1:]
		if !strings.HasPrefix(strings.ToUpper(pmcid), "PMC") {
			pmcid = "PMC" + pmcid
		}
		if id.PMCID == "" && primary {
			id.PMCID = pmcid
		}
		res.add(candidate{URL: "https://pmc.ncbi.nlm.nih.gov/articles/" + pmcid + "/", Source: "pmc", Kind: "html", Priority: priorityPMC, Version: "publishedVersion"})
	}
	for _, a := range arxivIDsFromWork(work) {
		if id.ArxivID == "" && primary {
			id.ArxivID = a
		}
		res.addArxiv(a)
	}

	for i, loc := range work.AllLocations() {
		isBestOA := i == 0 && work.BestOALocation != nil
		if !loc.IsOA && !isBestOA {
			continue
		}
		if isArxivURL(loc.PDFURL) || isArxivURL(loc.LandingPageURL) {
			continue
		}
		if loc.PDFURL != "" {
			res.add(candidate{URL: loc.PDFURL, Source: "openalex", Kind: docKindPDF, Version: loc.Version, Priority: versionPriority(loc.Version)})
		}
		if loc.LandingPageURL != "" && !isDOIURL(loc.LandingPageURL) {
			res.add(candidate{URL: loc.LandingPageURL, Source: "openalex", Kind: "landing", Version: loc.Version, Priority: priorityOALanding})
		}
	}
	if u := strings.TrimSpace(work.OpenAccess.OAURL); u != "" && !isArxivURL(u) && !isDOIURL(u) {
		res.add(candidate{URL: u, Source: "openalex", Kind: "unknown", Priority: priorityOAURL})
	}
}

func firstN(values []string, n int) []string {
	if len(values) > n {
		return values[:n]
	}
	return values
}

func versionPriority(version string) int {
	switch version {
	case "publishedVersion":
		return priorityOAPublished
	case "acceptedVersion":
		return priorityOAAccepted
	default:
		return priorityOASubmitted
	}
}

func isDOIURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "doi.org" || h == "dx.doi.org"
}

func isArxivURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && isArxivHost(strings.ToLower(u.Hostname()))
}

func arxivIDsFromWork(work scholar.OpenAlexWork) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(id string) {
		base := arxivVersion.ReplaceAllString(id, "")
		if !seen[base] {
			seen[base] = true
			out = append(out, id)
		}
	}
	for _, loc := range work.AllLocations() {
		for _, raw := range []string{loc.PDFURL, loc.LandingPageURL} {
			u, err := url.Parse(raw)
			if err != nil || raw == "" || !isArxivHost(strings.ToLower(u.Hostname())) {
				continue
			}
			if id, ok := arxivIDFromURLPath(u.Path); ok {
				add(id)
			}
		}
	}
	if doi, ok := normalizeDOI(work.DOI); ok && strings.HasPrefix(doi, "10.48550/arxiv.") {
		if id, ok := normalizeArxivID(strings.TrimPrefix(doi, "10.48550/arxiv.")); ok {
			add(id)
		}
	}
	return out
}

func sameArxiv(a, b string) bool {
	return strings.EqualFold(arxivVersion.ReplaceAllString(a, ""), arxivVersion.ReplaceAllString(b, ""))
}

// add inserts a candidate unless an equivalent URL is already present, in
// which case the higher priority wins.
func (res *resolution) add(c candidate) {
	c.URL = strings.TrimSpace(c.URL)
	if c.URL == "" {
		return
	}
	key := canonicalURLKey(c.URL)
	for i := range res.Candidates {
		if canonicalURLKey(res.Candidates[i].URL) == key {
			if c.Priority > res.Candidates[i].Priority {
				res.Candidates[i] = c
			}
			return
		}
	}
	res.Candidates = append(res.Candidates, c)
}

func (res *resolution) addArxiv(id string) {
	res.add(candidate{URL: "https://arxiv.org/html/" + id, Source: "arxiv", Kind: docKindHTML, Version: "submittedVersion", Priority: priorityArxivHTML})
	res.add(candidate{URL: "https://ar5iv.labs.arxiv.org/html/" + id, Source: "arxiv", Kind: docKindHTML, Version: "submittedVersion", Priority: priorityAr5iv})
	res.add(candidate{URL: "https://arxiv.org/pdf/" + id, Source: "arxiv", Kind: docKindPDF, Version: "submittedVersion", Priority: priorityArxivPDF})
}

// sortedCandidates orders candidates by priority, stable on insertion order.
func (res *resolution) sortedCandidates() []candidate {
	out := append([]candidate{}, res.Candidates...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// canonicalURLKey normalizes a URL for de-duplication.
func canonicalURLKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if isArxivHost(host) {
		path = strings.TrimSuffix(path, ".pdf")
	}
	key := host + path
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

func isArxivHost(host string) bool {
	return host == "arxiv.org" || host == "www.arxiv.org" || host == "export.arxiv.org" || host == "ar5iv.labs.arxiv.org" || host == "ar5iv.org"
}

// arxivIDFromURLPath extracts an arXiv ID from paths like /abs/1706.03762,
// /pdf/1706.03762v5.pdf, or /html/2401.00001.
func arxivIDFromURLPath(path string) (string, bool) {
	path = strings.Trim(path, "/")
	for _, prefix := range []string{"abs/", "pdf/", "html/"} {
		if strings.HasPrefix(path, prefix) {
			id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".pdf")
			return normalizeArxivID(id)
		}
	}
	return "", false
}

func normalizeArxivID(raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	for _, prefix := range []string{"arXiv:", "arxiv:", "ARXIV:"} {
		id = strings.TrimPrefix(id, prefix)
	}
	if arxivNewIDPattern.MatchString(id) || arxivOldIDPattern.MatchString(id) {
		return id, true
	}
	return "", false
}

// arxivCandidates orders sources HTML-first: native arXiv HTML, ar5iv
// rendering, then the PDF.
func arxivCandidates(id string) []string {
	return []string{
		"https://arxiv.org/html/" + id,
		"https://ar5iv.labs.arxiv.org/html/" + id,
		"https://arxiv.org/pdf/" + id,
	}
}

func normalizeDOI(raw string) (string, bool) { return scholar.NormalizeDOI(raw) }

// ---- title resolution ----------------------------------------------------

// titleCandidate is one work returned by a title search, scored against the
// request.
type titleCandidate struct {
	Title      string
	Year       int
	Authors    []string
	DOI        string
	OpenAlexID string
	// Cited is the record's citation count, when the provider reports one.
	Cited    *int
	Provider string
	work     *scholar.OpenAlexWork

	sim      float64
	authorEv int // +1 match, -1 mismatch, 0 unknown
	yearEv   int
}

func (c *titleCandidate) negative() bool { return c.authorEv < 0 || c.yearEv < 0 }
func (c *titleCandidate) positives() int {
	n := 0
	if c.authorEv > 0 {
		n++
	}
	if c.yearEv > 0 {
		n++
	}
	return n
}

func (c *titleCandidate) evidence() []string {
	ev := []string{fmt.Sprintf("title similarity %.2f", c.sim)}
	switch c.authorEv {
	case 1:
		ev = append(ev, "author matches")
	case -1:
		ev = append(ev, "author does not match")
	}
	switch c.yearEv {
	case 1:
		ev = append(ev, "year matches")
	case -1:
		ev = append(ev, "year does not match")
	}
	return ev
}

func (c *titleCandidate) toCandidate() scholar.Candidate {
	return scholar.Candidate{
		ID:           firstNonEmpty(c.OpenAlexID, prefixed("doi:", c.DOI)),
		Label:        c.Title,
		Year:         c.Year,
		DOI:          c.DOI,
		Authors:      strings.Join(firstN(c.Authors, 4), ", "),
		CitedByCount: c.Cited,
		Score:        float64(int(c.sim*100+0.5)) / 100,
		Evidence:     c.evidence(),
		Source:       c.Provider,
	}
}

func prefixed(prefix, v string) string {
	if v == "" {
		return ""
	}
	return prefix + v
}

func scoreTitleCandidates(n *normalized, cands []*titleCandidate) {
	for _, c := range cands {
		c.sim = scholar.TitleSimilarity(n.title, c.Title)
		if n.author != "" && len(c.Authors) > 0 {
			if authorsInclude(c.Authors, n.author) {
				c.authorEv = 1
			} else {
				c.authorEv = -1
			}
		}
		if n.year != 0 && c.Year != 0 {
			if abs(c.Year-n.year) <= 1 {
				c.yearEv = 1
			} else {
				c.yearEv = -1
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].negative() != cands[j].negative() {
			return !cands[i].negative()
		}
		if cands[i].positives() != cands[j].positives() {
			return cands[i].positives() > cands[j].positives()
		}
		return cands[i].sim > cands[j].sim
	})
}

// sameWorkVersions reports whether two title hits are plausibly versions of
// one paper (preprint and published record): near-identical titles,
// overlapping author surnames and close years. Hits without authors are never
// merged, since nothing ties them to the same people.
func sameWorkVersions(a, b *titleCandidate) bool {
	if scholar.TitleSimilarity(a.Title, b.Title) < scholar.StrongTitleSimilarity {
		return false
	}
	if a.Year != 0 && b.Year != 0 && abs(a.Year-b.Year) > 3 {
		return false
	}
	families := map[string]bool{}
	for _, name := range a.Authors {
		families[scholar.FamilyName(name)] = true
	}
	for _, name := range b.Authors {
		if families[scholar.FamilyName(name)] {
			return true
		}
	}
	return false
}

// decideTitle picks the matching work, its versions, and the match
// evidence, or explains why the title is ambiguous or unmatched. Warnings
// are returned when a paper was selected by citation dominance.
func decideTitle(n *normalized, cands []*titleCandidate) ([]*titleCandidate, scholar.MatchInfo, []string, *scholar.ToolError) {
	scoreTitleCandidates(n, cands)

	strong := []*titleCandidate{}
	for _, c := range cands {
		if c.sim >= scholar.StrongTitleSimilarity && !c.negative() {
			strong = append(strong, c)
		}
	}

	listed := func(pool []*titleCandidate) []scholar.Candidate {
		out := []scholar.Candidate{}
		for _, c := range pool {
			if len(out) >= maxListedTitles {
				break
			}
			out = append(out, c.toCandidate())
		}
		return out
	}
	evidenceHint := "Call again with doi, arxiv_id or openalex_id from a candidate, or add author / year evidence."

	if len(strong) == 0 {
		fuzzy := []*titleCandidate{}
		for _, c := range cands {
			if c.sim >= 0.75 && !c.negative() {
				fuzzy = append(fuzzy, c)
			}
		}
		if len(fuzzy) == 1 && fuzzy[0].positives() > 0 {
			return fuzzy, scholar.MatchInfo{Method: "title_fuzzy", Confidence: "medium", Evidence: fuzzy[0].evidence()}, nil, nil
		}
		near := []*titleCandidate{}
		for _, c := range cands {
			if c.sim >= 0.5 {
				near = append(near, c)
			}
		}
		if len(near) > 0 {
			return nil, scholar.MatchInfo{}, nil, &scholar.ToolError{
				Code:       scholar.CodeAmbiguous,
				Message:    fmt.Sprintf("no search result matches the title %q closely enough (with the supplied evidence) to identify the paper", n.title),
				Hint:       evidenceHint,
				Candidates: listed(near),
			}
		}
		return nil, scholar.MatchInfo{}, nil, &scholar.ToolError{
			Code:       scholar.CodeNoResults,
			Message:    fmt.Sprintf("no paper found matching title %q", n.title),
			Hint:       "Check the title, or use a DOI, arXiv ID, or direct URL instead.",
			Candidates: listed(cands),
		}
	}

	groups := versionGroups(strong)
	best, versions := strong[0], groups[0]
	others := flatten(groups[1:])
	dominant := false
	if len(others) > 0 {
		separated := best.positives() > 0
		for _, o := range others {
			if o.positives() >= best.positives() {
				separated = false
			}
		}
		if !separated {
			top, ok := dominantGroup(groups, best.positives())
			if !ok {
				return nil, scholar.MatchInfo{}, nil, &scholar.ToolError{
					Code:       scholar.CodeAmbiguous,
					Message:    fmt.Sprintf("%d different papers match the title %q", len(groups), n.title),
					Hint:       evidenceHint,
					Candidates: listed(strong),
				}
			}
			dominant = true
			versions = groups[top]
			best = versions[0]
			others = flatten(append(append([][]*titleCandidate{}, groups[:top]...), groups[top+1:]...))
		}
	}

	evidence := best.evidence()
	if len(versions) > 1 {
		evidence = append(evidence, fmt.Sprintf("%d records treated as versions of the same paper", len(versions)))
	}
	switch {
	case dominant:
		evidence = append(evidence, fmt.Sprintf("cited by %d, at least %dx each of %d other same-title paper(s)", *best.Cited, scholar.DominanceRatio, len(groups)-1))
	case len(others) > 0:
		evidence = append(evidence, fmt.Sprintf("author/year evidence excluded %d same-title paper(s)", len(others)))
	}
	method, confidence := "title_exact", "medium"
	if best.sim < 0.97 {
		method = "title_near_exact"
	}
	if best.sim >= 0.97 && best.positives() > 0 {
		confidence = "high"
	}
	if best.sim < 0.97 && best.positives() == 0 {
		confidence = "low"
	}
	match := scholar.MatchInfo{Method: method, Confidence: confidence, Evidence: evidence}
	if !dominant {
		return versions, match, nil, nil
	}
	match.Confidence = "medium"
	match.Alternatives = listed(others)
	warning := fmt.Sprintf("%d other paper(s) share this title (see identity.match.alternatives); selected %s because it has at least %dx the citations of each. Call again with doi or openalex_id to read another.",
		len(groups)-1, firstNonEmpty(best.OpenAlexID, prefixed("doi:", best.DOI), best.Title), scholar.DominanceRatio)
	return versions, match, []string{warning}, nil
}

// minDominantPaperCitations keeps the dominance tie-break from choosing
// between barely cited records, where the counts say little.
const minDominantPaperCitations = 50

// versionGroups clusters strong title hits into papers: each hit joins the
// first group whose leader it is a version of, so groups[0] is led by the
// best-ranked hit.
func versionGroups(strong []*titleCandidate) [][]*titleCandidate {
	var groups [][]*titleCandidate
next:
	for _, c := range strong {
		for i, g := range groups {
			if sameWorkVersions(g[0], c) {
				groups[i] = append(g, c)
				continue next
			}
		}
		groups = append(groups, []*titleCandidate{c})
	}
	return groups
}

func flatten(groups [][]*titleCandidate) []*titleCandidate {
	out := []*titleCandidate{}
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// dominantGroup returns the paper whose citation count (its most-cited
// version) is at least minDominantPaperCitations and DominanceRatio times
// every other paper's, reordering that group so its most-cited record comes
// first. It fails when any paper's count is unknown, or when the dominant
// paper has weaker author/year evidence (fewer than minPositives) than the
// best-ranked hit: citations never override identifying evidence.
func dominantGroup(groups [][]*titleCandidate, minPositives int) (int, bool) {
	cites := make([]int, len(groups))
	top := -1
	for i, g := range groups {
		sort.SliceStable(g, func(a, b int) bool { return citedOrMinus(g[a]) > citedOrMinus(g[b]) })
		if g[0].Cited == nil {
			return -1, false
		}
		cites[i] = *g[0].Cited
		if top < 0 || cites[i] > cites[top] {
			top = i
		}
	}
	if cites[top] < minDominantPaperCitations {
		return -1, false
	}
	for i, c := range cites {
		if i != top && !scholar.Dominates(cites[top], c) {
			return -1, false
		}
	}
	for _, c := range groups[top] {
		if c.positives() >= minPositives {
			return top, true
		}
	}
	return -1, false
}

func citedOrMinus(c *titleCandidate) int {
	if c.Cited == nil {
		return -1
	}
	return *c.Cited
}

func (res *resolution) resolveTitle(ctx context.Context, fetcher DocFetcher, n *normalized) *scholar.ToolError {
	params := url.Values{}
	params.Set("search", n.title)
	params.Set("per-page", strconv.Itoa(titleSearchSize))
	apiURL := "https://api.openalex.org/works?" + params.Encode()

	var resp struct {
		Results []scholar.OpenAlexWork `json:"results"`
	}
	status, oaErr := fetchJSON(ctx, fetcher, apiURL, &resp)
	res.Attempts = scholar.AppendAttempt(res.Attempts, attemptFor("openalex", "resolve", apiURL, status, oaErr))
	if isContextErr(oaErr) {
		return oaErr
	}

	var cands []*titleCandidate
	if oaErr == nil {
		for i := range resp.Results {
			w := resp.Results[i]
			doi, _ := normalizeDOI(w.DOI)
			oa, _ := scholar.NormalizeOpenAlexID(w.ID)
			cands = append(cands, &titleCandidate{Title: w.DisplayTitle(), Year: w.PublicationYear, Authors: w.AuthorNames(), DOI: doi, OpenAlexID: oa, Cited: w.CitedByCount, Provider: "openalex", work: &w})
		}
	} else {
		// OpenAlex unavailable: Crossref can still identify the DOI.
		res.Warnings = append(res.Warnings, "OpenAlex title search failed ("+oaErr.Message+"); identified via Crossref")
		var crossErr *scholar.ToolError
		cands, crossErr = crossrefTitleCandidates(ctx, fetcher, n.title, res)
		if crossErr != nil {
			if isContextErr(crossErr) || scholar.MoreInformative(crossErr, oaErr) {
				crossErr.Attempts = res.Attempts
				return crossErr
			}
			oaErr.Attempts = res.Attempts
			return oaErr
		}
	}

	chosen, match, warnings, err := decideTitle(n, cands)
	if err != nil {
		err.Attempts = res.Attempts
		return err
	}
	res.Identity.Match = match
	res.Warnings = append(res.Warnings, warnings...)
	primary := chosen[0]
	if primary.work != nil {
		res.applyWork(*primary.work, true)
		for _, v := range chosen[1:] {
			if v.work != nil {
				res.applyWork(*v.work, false)
			}
		}
	} else {
		res.Identity.DOI = primary.DOI
		res.Identity.Title = primary.Title
		res.Identity.Year = primary.Year
		res.Identity.Authors = firstN(primary.Authors, 10)
	}
	return nil
}

func crossrefTitleCandidates(ctx context.Context, fetcher DocFetcher, title string, res *resolution) ([]*titleCandidate, *scholar.ToolError) {
	params := url.Values{}
	params.Set("query.bibliographic", title)
	params.Set("rows", "5")
	params.Set("select", "DOI,title,author,issued")
	apiURL := "https://api.crossref.org/works?" + params.Encode()
	var resp struct {
		Message struct {
			Items []struct {
				DOI    string   `json:"DOI"`
				Title  []string `json:"title"`
				Author []struct {
					Given  string `json:"given"`
					Family string `json:"family"`
				} `json:"author"`
				Issued struct {
					DateParts [][]int `json:"date-parts"`
				} `json:"issued"`
			} `json:"items"`
		} `json:"message"`
	}
	status, err := fetchJSON(ctx, fetcher, apiURL, &resp)
	res.Attempts = scholar.AppendAttempt(res.Attempts, attemptFor("crossref", "resolve", apiURL, status, err))
	if err != nil {
		return nil, err
	}
	out := []*titleCandidate{}
	for _, item := range resp.Message.Items {
		if len(item.Title) == 0 {
			continue
		}
		c := &titleCandidate{Title: strings.TrimSpace(item.Title[0]), Provider: "crossref"}
		c.DOI, _ = normalizeDOI(item.DOI)
		if len(item.Issued.DateParts) > 0 && len(item.Issued.DateParts[0]) > 0 {
			c.Year = item.Issued.DateParts[0][0]
		}
		for _, a := range item.Author {
			if name := strings.TrimSpace(a.Given + " " + a.Family); name != "" {
				c.Authors = append(c.Authors, name)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// ---- Unpaywall -------------------------------------------------------------

type unpaywallLocation struct {
	URLForPDF string `json:"url_for_pdf"`
	URL       string `json:"url"`
	HostType  string `json:"host_type"`
	Version   string `json:"version"`
}

// unpaywallCandidates returns all Unpaywall OA locations for a DOI.
func unpaywallCandidates(ctx context.Context, fetcher DocFetcher, contactEmail, doi string) ([]candidate, scholar.Attempt, *scholar.ToolError) {
	apiURL := "https://api.unpaywall.org/v2/" + url.PathEscape(doi) + "?email=" + url.QueryEscape(contactEmail)

	var resp struct {
		BestOALocation *unpaywallLocation  `json:"best_oa_location"`
		OALocations    []unpaywallLocation `json:"oa_locations"`
	}
	status, jsonErr := fetchJSON(ctx, fetcher, apiURL, &resp)
	attempt := attemptFor("unpaywall", "resolve", apiURL, status, jsonErr)
	if jsonErr != nil {
		return nil, attempt, jsonErr
	}
	locs := resp.OALocations
	if resp.BestOALocation != nil {
		locs = append([]unpaywallLocation{*resp.BestOALocation}, locs...)
	}
	out := []candidate{}
	for _, loc := range locs {
		if loc.URLForPDF != "" {
			out = append(out, candidate{URL: loc.URLForPDF, Source: "unpaywall", Kind: docKindPDF, Version: loc.Version, Priority: versionPriority(loc.Version) - 1})
		}
		if loc.URL != "" && loc.URL != loc.URLForPDF && !isDOIURL(loc.URL) {
			out = append(out, candidate{URL: loc.URL, Source: "unpaywall", Kind: "landing", Version: loc.Version, Priority: priorityOALanding - 1})
		}
	}
	if len(out) == 0 {
		attempt.Outcome = scholar.CodeNoResults
		attempt.Message = "no open-access locations"
	}
	return out, attempt, nil
}

// ---- helpers ---------------------------------------------------------------

func attemptFor(provider, stage, rawURL string, status int, err *scholar.ToolError) scholar.Attempt {
	a := scholar.Attempt{Provider: provider, Stage: stage, URL: rawURL, Status: status, Outcome: "ok"}
	if err != nil {
		a.Outcome = err.Code
		a.Message = err.Message
	}
	return a
}

// fetchJSON GETs a JSON API endpoint. The HTTP status is returned alongside
// the error so callers can special-case 404 (unknown DOI).
func fetchJSON(ctx context.Context, fetcher DocFetcher, apiURL string, out any) (int, *scholar.ToolError) {
	doc, err := fetcher.GetJSON(ctx, apiURL)
	if err != nil {
		if ctxErr := scholar.ContextError(err, "request to "+hostOf(apiURL)); ctxErr != nil {
			return 0, ctxErr
		}
		if errors.Is(err, scholar.ErrBodyTooLarge) {
			return 0, &scholar.ToolError{Code: scholar.CodeUpstreamError, Message: hostOf(apiURL) + " response exceeded the size limit", Hint: "Raise RESEARCHER_MAX_RESPONSE_MB if this is expected."}
		}
		return 0, &scholar.ToolError{Code: scholar.CodeUpstreamError, Message: fmt.Sprintf("request to %s failed: %s", hostOf(apiURL), scholar.RedactText(err.Error())), Retryable: true}
	}
	switch {
	case doc.Status == http.StatusOK:
	case doc.Status == http.StatusNotFound:
		return doc.Status, &scholar.ToolError{Code: scholar.CodeNoResults, Message: fmt.Sprintf("%s has no record (status 404)", hostOf(apiURL))}
	case doc.Status == http.StatusTooManyRequests:
		e := &scholar.ToolError{Code: scholar.CodeBlocked, Message: fmt.Sprintf("%s rate limited the request (status 429)", hostOf(apiURL)), Retryable: true, Hint: "Configure the provider's credentials (see README) or retry later."}
		if doc.RetryAfter > 0 {
			e.RetryAfterSeconds = int(doc.RetryAfter.Seconds() + 0.5)
		}
		return doc.Status, e
	default:
		return doc.Status, &scholar.ToolError{Code: scholar.CodeUpstreamError, Message: fmt.Sprintf("%s returned status %d", hostOf(apiURL), doc.Status), Retryable: doc.Status >= 500}
	}
	if err := json.Unmarshal(doc.Body, out); err != nil {
		return doc.Status, &scholar.ToolError{Code: scholar.CodeParseFailed, Message: fmt.Sprintf("%s response parse failed: %v", hostOf(apiURL), err)}
	}
	return doc.Status, nil
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

// abstractFromInvertedIndex rebuilds an OpenAlex abstract.
func abstractFromInvertedIndex(inverted map[string][]int) string {
	maxPos := -1
	for _, positions := range inverted {
		for _, p := range positions {
			if p > maxPos {
				maxPos = p
			}
		}
	}
	if maxPos < 0 || maxPos > 5000 {
		return ""
	}
	words := make([]string, maxPos+1)
	for token, positions := range inverted {
		for _, p := range positions {
			if p >= 0 && p <= maxPos && words[p] == "" {
				words[p] = token
			}
		}
	}
	return strings.Join(strings.Fields(strings.Join(words, " ")), " ")
}
