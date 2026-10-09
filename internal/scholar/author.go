package scholar

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// AuthorQuery identifies a researcher. Exact identifiers (ORCID, OpenAlex
// author ID) take precedence over names; affiliation and a known paper are
// optional evidence for choosing between same-name people.
type AuthorQuery struct {
	Name        string
	ORCID       string
	OpenAlexID  string
	Affiliation string
	// KnownPaper is a DOI or title of a paper the researcher authored.
	KnownPaper string
}

const (
	confidenceHigh   = "high"
	confidenceMedium = "medium"
	confidenceLow    = "low"

	maxAuthorCandidates = 8
	authorSampleSize    = 5
)

// GetAuthorInfo resolves a researcher and returns profile metadata. When the
// evidence cannot single out one person it returns an "ambiguous" error with
// candidates instead of guessing.
func GetAuthorInfo(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	q.Name = normalizeSpace(q.Name)
	q.Affiliation = normalizeSpace(q.Affiliation)
	q.KnownPaper = normalizeSpace(q.KnownPaper)

	var warnings []string
	if q.ORCID != "" {
		id, ok := NormalizeORCID(q.ORCID)
		if !ok {
			return nil, &ToolError{Code: CodeInvalidInput, Message: fmt.Sprintf("invalid ORCID iD: %q", q.ORCID), Hint: "Expected 0000-0000-0000-000X (checksum is validated)."}
		}
		q.ORCID = id
	}
	if q.OpenAlexID != "" {
		id, ok := NormalizeOpenAlexID(q.OpenAlexID)
		if !ok || !strings.HasPrefix(id, "A") {
			return nil, &ToolError{Code: CodeInvalidInput, Message: fmt.Sprintf("invalid OpenAlex author ID: %q", q.OpenAlexID), Hint: "Expected A followed by digits, e.g. A5023888391."}
		}
		q.OpenAlexID = id
	}
	if q.Name != "" {
		people := SplitPeople(q.Name)
		if len(people) == 0 {
			return nil, &ToolError{Code: CodeInvalidInput, Message: "author_name is required"}
		}
		if len(people) > 1 {
			warnings = append(warnings, fmt.Sprintf("author_name lists %d people; only the first (%q) was looked up. Query the others separately.", len(people), people[0]))
		}
		q.Name = DisplayOrder(people[0])
	}
	if q.Name == "" && q.ORCID == "" && q.OpenAlexID == "" {
		return nil, &ToolError{Code: CodeInvalidInput, Message: "author_name, orcid or openalex_id is required"}
	}

	var (
		author  *AuthorInfo
		toolErr *ToolError
	)
	if q.ORCID != "" || q.OpenAlexID != "" {
		author, toolErr = resolveAuthorByIdentifier(ctx, requester, q)
	} else {
		author, toolErr = resolveAuthorByName(ctx, requester, q)
	}
	if toolErr != nil {
		return nil, toolErr
	}
	author.Warnings = append(warnings, author.Warnings...)
	if author.Metrics.CitedByCount != nil {
		author.CitedBy = IntPtr(*author.Metrics.CitedByCount)
	}
	if author.ExternalIDs == nil {
		author.ExternalIDs = map[string]string{}
	}
	if author.Interests == nil {
		author.Interests = []string{}
	}
	if author.Publications == nil {
		author.Publications = []Publication{}
	}
	return author, nil
}

// ---- OpenAlex ------------------------------------------------------------

type openAlexInstitution struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type openAlexAuthorResult struct {
	ID                      string                `json:"id"`
	ORCID                   string                `json:"orcid"`
	DisplayName             string                `json:"display_name"`
	DisplayNameAlternatives []string              `json:"display_name_alternatives"`
	WorksCount              *int                  `json:"works_count"`
	CitedByCount            *int                  `json:"cited_by_count"`
	LastKnownInstitutions   []openAlexInstitution `json:"last_known_institutions"`
	Affiliations            []struct {
		Institution openAlexInstitution `json:"institution"`
		Years       []int               `json:"years"`
	} `json:"affiliations"`
	SummaryStats *struct {
		HIndex   *int `json:"h_index"`
		I10Index *int `json:"i10_index"`
	} `json:"summary_stats"`
	Topics []struct {
		DisplayName string `json:"display_name"`
	} `json:"topics"`
	XConcepts []struct {
		DisplayName string  `json:"display_name"`
		Score       float64 `json:"score"`
	} `json:"x_concepts"`
}

func (a openAlexAuthorResult) shortID() string {
	id, _ := NormalizeOpenAlexID(a.ID)
	return id
}

func (a openAlexAuthorResult) orcid() string {
	id, _ := NormalizeORCID(a.ORCID)
	return id
}

func (a openAlexAuthorResult) institutions() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		name = normalizeSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, inst := range a.LastKnownInstitutions {
		add(inst.DisplayName)
	}
	for _, aff := range a.Affiliations {
		add(aff.Institution.DisplayName)
	}
	return out
}

// nameMatch is the strongest match between a requested name and the
// profile's display name or any recorded alternative.
func (a openAlexAuthorResult) nameMatch(requested string) string {
	best := CompareNames(requested, a.DisplayName)
	if best == NameExact {
		return best
	}
	for _, alt := range a.DisplayNameAlternatives {
		switch CompareNames(requested, alt) {
		case NameExact:
			return NameExact
		case NameCompatible:
			best = NameCompatible
		}
	}
	return best
}

type openAlexAuthorResponse struct {
	Results []openAlexAuthorResult `json:"results"`
}

func fetchOpenAlexAuthor(ctx context.Context, requester *Requester, idPath string) (*openAlexAuthorResult, int, *ToolError) {
	doc, err := requester.GetJSON(ctx, "https://api.openalex.org/authors/"+idPath)
	if err != nil {
		return nil, 0, requestError("openalex", err)
	}
	if doc.Status == http.StatusNotFound {
		return nil, doc.Status, &ToolError{Code: CodeNoResults, Message: "openalex author not found"}
	}
	if doc.Status != http.StatusOK {
		return nil, doc.Status, statusError("openalex", doc)
	}
	var a openAlexAuthorResult
	if err := json.Unmarshal(doc.Body, &a); err != nil {
		return nil, doc.Status, &ToolError{Code: CodeParseFailed, Message: fmt.Sprintf("openalex author parse failed: %v", err)}
	}
	return &a, doc.Status, nil
}

func resolveAuthorByIdentifier(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	var (
		profile *openAlexAuthorResult
		oaErr   *ToolError
		method  string
	)
	if q.OpenAlexID != "" {
		method = "openalex_id"
		profile, _, oaErr = fetchOpenAlexAuthor(ctx, requester, q.OpenAlexID)
		if oaErr == nil && q.ORCID != "" && profile.orcid() != "" && profile.orcid() != q.ORCID {
			return nil, &ToolError{
				Code:    CodeIdentifierConflict,
				Message: fmt.Sprintf("OpenAlex author %s has ORCID %s, but orcid %s was supplied", q.OpenAlexID, profile.orcid(), q.ORCID),
				Hint:    "Check which identifier is correct and supply only that one.",
				Candidates: []Candidate{
					authorCandidate(*profile, nil),
					{ID: "orcid:" + q.ORCID, Label: "ORCID record " + q.ORCID, ORCID: q.ORCID, Source: "input"},
				},
			}
		}
	} else {
		method = "orcid"
		profile, _, oaErr = fetchOpenAlexAuthor(ctx, requester, "orcid:"+q.ORCID)
	}

	if oaErr != nil {
		if ctxErr := ContextError(ctx.Err(), "author lookup"); ctxErr != nil {
			return nil, ctxErr
		}
		if q.ORCID == "" {
			return nil, oaErr
		}
		// The ORCID registry itself is authoritative for an ORCID iD.
		author, orcidErr := orcidRecord(ctx, requester, q.ORCID)
		if orcidErr != nil {
			if MoreInformative(oaErr, orcidErr) {
				return nil, oaErr
			}
			return nil, orcidErr
		}
		author.Match = &MatchInfo{Method: "orcid", Confidence: confidenceHigh, Evidence: []string{"ORCID record retrieved by iD"}}
		author.Warnings = append(author.Warnings, "OpenAlex profile unavailable for this ORCID ("+oaErr.Message+"); metrics unknown")
		checkSuppliedName(author, q.Name)
		return author, nil
	}

	author, toolErr := buildOpenAlexAuthor(ctx, requester, *profile)
	if toolErr != nil {
		return nil, toolErr
	}
	evidence := []string{"OpenAlex author retrieved by " + strings.ReplaceAll(method, "_", " ")}
	if q.ORCID != "" && profile.orcid() == q.ORCID {
		evidence = append(evidence, "ORCID matches the OpenAlex profile")
	}
	author.Match = &MatchInfo{Method: method, Confidence: confidenceHigh, Evidence: evidence}
	if q.Name != "" && profile.nameMatch(q.Name) == NameMismatch {
		author.Warnings = append(author.Warnings, fmt.Sprintf("supplied author_name %q does not match profile name %q; the identifier was trusted", q.Name, author.Name))
	}
	return author, nil
}

func checkSuppliedName(author *AuthorInfo, name string) {
	if name != "" && author.Name != "" && CompareNames(name, author.Name) == NameMismatch {
		author.Warnings = append(author.Warnings, fmt.Sprintf("supplied author_name %q does not match record name %q; the identifier was trusted", name, author.Name))
	}
}

func resolveAuthorByName(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	author, best := resolveOpenAlexAuthorByName(ctx, requester, q)
	if author != nil {
		return author, nil
	}
	if best != nil && !isAuthorFallbackCode(best.Code) {
		return nil, best
	}
	attempts := []Attempt{{Provider: "openalex", Stage: "author_lookup", Outcome: best.Code, Message: best.Message}}

	type provider struct {
		name string
		fn   func(context.Context, *Requester, AuthorQuery) (*AuthorInfo, *ToolError)
	}
	// Identity-bearing sources first; Crossref has no author profiles, so it
	// is the last resort and its results are marked unverified.
	for _, p := range []provider{
		{"orcid", getAuthorInfoFromORCID},
		{"google_scholar", getAuthorInfoFromScholar},
		{"crossref", getAuthorInfoFromCrossref},
	} {
		if ctxErr := ContextError(ctx.Err(), "author lookup"); ctxErr != nil {
			ctxErr.Attempts = attempts
			return nil, ctxErr
		}
		a, err := p.fn(ctx, requester, q)
		if err == nil {
			a.Warnings = append(a.Warnings, fmt.Sprintf("resolved via %s fallback after OpenAlex: %s", p.name, best.Message))
			return a, nil
		}
		attempts = AppendAttempt(attempts, Attempt{Provider: p.name, Stage: "author_lookup", Outcome: err.Code, Message: err.Message})
		if !isAuthorFallbackCode(err.Code) {
			err.Attempts = attempts
			return nil, err
		}
		if MoreInformative(err, best) {
			best = err
		}
	}
	out := *best
	out.Attempts = attempts
	return nil, &out
}

// isAuthorFallbackCode reports failures that justify trying the next
// provider. Ambiguity and identifier conflicts are answers, not failures.
func isAuthorFallbackCode(code string) bool {
	switch code {
	case CodeNoResults, CodeUpstreamError, CodeParseFailed, CodeBlocked:
		return true
	default:
		return false
	}
}

type scoredAuthor struct {
	profile  openAlexAuthorResult
	name     string
	evidence []string
	paper    bool
	aff      bool
}

func resolveOpenAlexAuthorByName(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	params := url.Values{}
	params.Set("search", q.Name)
	params.Set("per-page", "25")
	doc, err := requester.GetJSON(ctx, "https://api.openalex.org/authors?"+params.Encode())
	if err != nil {
		return nil, requestError("openalex", err)
	}
	if doc.Status != http.StatusOK {
		return nil, statusError("openalex", doc)
	}
	var resp openAlexAuthorResponse
	if err := json.Unmarshal(doc.Body, &resp); err != nil {
		return nil, &ToolError{Code: CodeParseFailed, Message: fmt.Sprintf("openalex author parse failed: %v", err)}
	}

	candidates := make([]*scoredAuthor, 0, len(resp.Results))
	for _, r := range resp.Results {
		if m := r.nameMatch(q.Name); m != NameMismatch {
			candidates = append(candidates, &scoredAuthor{profile: r, name: m, evidence: []string{"name " + m}})
		}
	}

	var notes []string
	if q.KnownPaper != "" {
		authorIDs, paperTitle, paperErr := knownPaperAuthors(ctx, requester, q.KnownPaper)
		switch {
		case paperErr != nil:
			notes = append(notes, "known_paper could not be checked: "+paperErr.Message)
		default:
			found := false
			for _, c := range candidates {
				if _, ok := authorIDs[c.profile.shortID()]; ok {
					c.paper = true
					found = true
					c.evidence = append(c.evidence, fmt.Sprintf("listed as an author of %q", paperTitle))
				}
			}
			if !found {
				// The author may not be in the top search results: fetch any
				// name-compatible author of the known paper directly.
				for id, name := range authorIDs {
					if name == "" || CompareNames(q.Name, name) == NameMismatch {
						continue
					}
					profile, _, fetchErr := fetchOpenAlexAuthor(ctx, requester, id)
					if fetchErr != nil {
						continue
					}
					candidates = append(candidates, &scoredAuthor{
						profile: *profile, name: profile.nameMatch(q.Name), paper: true,
						evidence: []string{"name " + profile.nameMatch(q.Name), fmt.Sprintf("listed as an author of %q", paperTitle)},
					})
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, &ToolError{Code: CodeNoResults, Message: fmt.Sprintf("no OpenAlex author profile matches the name %q", q.Name)}
	}

	if q.Affiliation != "" {
		for _, c := range candidates {
			if inst := matchingInstitution(q.Affiliation, c.profile.institutions()); inst != "" {
				c.aff = true
				c.evidence = append(c.evidence, fmt.Sprintf("affiliation matches %q", inst))
			}
		}
	}

	choose := func(filter func(*scoredAuthor) bool) []*scoredAuthor {
		out := []*scoredAuthor{}
		for _, c := range candidates {
			if filter(c) {
				out = append(out, c)
			}
		}
		return out
	}

	var (
		chosen     *scoredAuthor
		method     string
		confidence string
	)
	paperMatches := choose(func(c *scoredAuthor) bool { return c.paper })
	affMatches := choose(func(c *scoredAuthor) bool { return c.aff })
	bothMatches := choose(func(c *scoredAuthor) bool { return c.paper && c.aff })

	switch {
	case len(bothMatches) == 1:
		chosen, method, confidence = bothMatches[0], "name+known_paper+affiliation", confidenceHigh
	case len(paperMatches) == 1:
		chosen, method, confidence = paperMatches[0], "name+known_paper", confidenceHigh
	case len(paperMatches) == 0 && len(affMatches) == 1:
		chosen, method, confidence = affMatches[0], "name+affiliation", confidenceMedium
	case len(paperMatches) == 0 && len(affMatches) == 0 && len(candidates) == 1:
		chosen, method = candidates[0], "name_unique"
		confidence = confidenceMedium
		if chosen.name != NameExact {
			confidence = confidenceLow
		}
		chosen.evidence = append(chosen.evidence, fmt.Sprintf("only name-compatible profile among %d OpenAlex search results", len(resp.Results)))
		if q.Affiliation != "" {
			notes = append(notes, fmt.Sprintf("affiliation %q does not match this profile's recorded institutions", q.Affiliation))
			confidence = confidenceLow
		}
		if q.KnownPaper != "" {
			notes = append(notes, "the known paper did not confirm this profile")
			confidence = confidenceLow
		}
	}

	var alternatives []Candidate
	if chosen == nil {
		pool, poolMethod := candidates, "name"
		switch {
		case len(paperMatches) > 1:
			pool, poolMethod = paperMatches, "name+known_paper"
		case len(affMatches) > 1:
			pool, poolMethod = affMatches, "name+affiliation"
		}
		top, others := dominantAuthor(pool)
		if top == nil {
			return nil, ambiguousAuthors(q, pool, notes)
		}
		chosen, method, confidence = top, poolMethod+"+dominance", confidenceMedium
		chosen.evidence = append(chosen.evidence, fmt.Sprintf("cited by %d with %d works: at least %dx every other matching profile on both", *top.profile.CitedByCount, *top.profile.WorksCount, DominanceRatio))
		if poolMethod == "name" && (q.Affiliation != "" || q.KnownPaper != "") {
			notes = append(notes, "the supplied affiliation / known_paper evidence did not confirm this profile")
			confidence = confidenceLow
		}
		alternatives = make([]Candidate, 0, len(others))
		for _, o := range others {
			if len(alternatives) >= maxAuthorCandidates {
				break
			}
			alternatives = append(alternatives, authorCandidate(o.profile, o.evidence))
		}
		notes = append(notes, dominanceWarning(q.Name, top, alternatives, len(others)))
	}

	author, toolErr := buildOpenAlexAuthor(ctx, requester, chosen.profile)
	if toolErr != nil {
		return nil, toolErr
	}
	author.Match = &MatchInfo{Method: method, Confidence: confidence, Evidence: chosen.evidence, Alternatives: alternatives}
	author.Warnings = append(author.Warnings, notes...)
	return author, nil
}

// minDominantAuthorWorks keeps the dominance tie-break from choosing between
// sparse profiles, where the counts say little about who is meant.
const minDominantAuthorWorks = 20

// dominantAuthor returns the profile whose citation and works totals are each
// at least DominanceRatio times every other profile's in pool (with at least
// minDominantAuthorWorks works), plus the others ordered by works count. It
// returns nil when no profile dominates or any total is unknown: a stray
// same-name profile is then told apart from the real one, while comparable
// same-name researchers stay ambiguous.
func dominantAuthor(pool []*scoredAuthor) (*scoredAuthor, []*scoredAuthor) {
	var top *scoredAuthor
	for _, c := range pool {
		if c.profile.CitedByCount == nil || c.profile.WorksCount == nil {
			return nil, nil
		}
		if top == nil || *c.profile.CitedByCount > *top.profile.CitedByCount {
			top = c
		}
	}
	if top == nil || *top.profile.WorksCount < minDominantAuthorWorks {
		return nil, nil
	}
	others := make([]*scoredAuthor, 0, len(pool)-1)
	for _, c := range pool {
		if c == top {
			continue
		}
		if !Dominates(*top.profile.CitedByCount, *c.profile.CitedByCount) || !Dominates(*top.profile.WorksCount, *c.profile.WorksCount) {
			return nil, nil
		}
		others = append(others, c)
	}
	sort.SliceStable(others, func(i, j int) bool {
		return *others[i].profile.WorksCount > *others[j].profile.WorksCount
	})
	return top, others
}

// dominanceWarning tells the caller that other profiles share the name and
// how to pick one of them instead.
func dominanceWarning(name string, top *scoredAuthor, alternatives []Candidate, total int) string {
	descs := make([]string, 0, len(alternatives))
	for _, a := range alternatives {
		parts := []string{}
		if a.ORCID != "" {
			parts = append(parts, "ORCID "+a.ORCID)
		}
		if a.Affiliation != "" {
			parts = append(parts, a.Affiliation)
		}
		parts = append(parts, fmt.Sprintf("%d works, %d citations", derefInt(a.WorksCount), derefInt(a.CitedByCount)))
		descs = append(descs, a.ID+" ("+strings.Join(parts, ", ")+")")
	}
	return fmt.Sprintf("%d other OpenAlex profile(s) also match %q: %s. Selected %s because its citations and works are each at least %dx theirs; call again with openalex_id or orcid to choose another (see match.alternatives).",
		total, name, strings.Join(descs, "; "), top.profile.shortID(), DominanceRatio)
}

func ambiguousAuthors(q AuthorQuery, pool []*scoredAuthor, notes []string) *ToolError {
	sort.SliceStable(pool, func(i, j int) bool {
		if (pool[i].name == NameExact) != (pool[j].name == NameExact) {
			return pool[i].name == NameExact
		}
		return derefInt(pool[i].profile.WorksCount) > derefInt(pool[j].profile.WorksCount)
	})
	out := make([]Candidate, 0, maxAuthorCandidates)
	for _, c := range pool {
		if len(out) >= maxAuthorCandidates {
			break
		}
		out = append(out, authorCandidate(c.profile, c.evidence))
	}
	msg := fmt.Sprintf("%d OpenAlex author profiles match %q and the supplied evidence does not single one out", len(pool), q.Name)
	if len(notes) > 0 {
		msg += " (" + strings.Join(notes, "; ") + ")"
	}
	return &ToolError{
		Code:       CodeAmbiguous,
		Message:    msg,
		Hint:       "Call again with orcid or openalex_id from a candidate, or add affiliation / known_paper evidence. Candidates are ordered by name exactness then works count; that order is not identity evidence.",
		Candidates: out,
	}
}

func authorCandidate(p openAlexAuthorResult, evidence []string) Candidate {
	insts := p.institutions()
	aff := ""
	if len(insts) > 0 {
		aff = insts[0]
	}
	return Candidate{
		ID:           p.shortID(),
		Label:        normalizeSpace(p.DisplayName),
		ORCID:        p.orcid(),
		Affiliation:  aff,
		WorksCount:   p.WorksCount,
		CitedByCount: p.CitedByCount,
		Evidence:     evidence,
		Source:       "openalex",
	}
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// matchingInstitution returns the first institution that matches the
// requested affiliation by containment or strong word overlap.
func matchingInstitution(requested string, institutions []string) string {
	want := NormalizeTitle(requested)
	if want == "" {
		return ""
	}
	for _, inst := range institutions {
		have := NormalizeTitle(inst)
		if have == "" {
			continue
		}
		if strings.Contains(" "+have+" ", " "+want+" ") || strings.Contains(" "+want+" ", " "+have+" ") || TitleSimilarity(want, have) >= 0.75 {
			return inst
		}
	}
	return ""
}

// knownPaperAuthors resolves a DOI or exact title to its OpenAlex authorship
// list (short author ID → display name).
func knownPaperAuthors(ctx context.Context, requester *Requester, paper string) (map[string]string, string, *ToolError) {
	var work *OpenAlexWork
	if doi, ok := NormalizeDOI(paper); ok {
		doc, err := requester.GetJSON(ctx, "https://api.openalex.org/works/https://doi.org/"+doi)
		if err != nil {
			return nil, "", requestError("openalex", err)
		}
		if doc.Status != http.StatusOK {
			return nil, "", statusError("openalex", doc)
		}
		var w OpenAlexWork
		if err := json.Unmarshal(doc.Body, &w); err != nil {
			return nil, "", &ToolError{Code: CodeParseFailed, Message: "openalex work parse failed"}
		}
		work = &w
	} else {
		params := url.Values{}
		params.Set("search", paper)
		params.Set("per-page", "10")
		doc, err := requester.GetJSON(ctx, "https://api.openalex.org/works?"+params.Encode())
		if err != nil {
			return nil, "", requestError("openalex", err)
		}
		if doc.Status != http.StatusOK {
			return nil, "", statusError("openalex", doc)
		}
		var resp openAlexWorksSearchResponse
		if err := json.Unmarshal(doc.Body, &resp); err != nil {
			return nil, "", &ToolError{Code: CodeParseFailed, Message: "openalex work search parse failed"}
		}
		// Every work whose title matches exactly contributes authors, so a
		// preprint and its published version both count.
		ids := map[string]string{}
		title := ""
		for _, w := range resp.Results {
			if TitleSimilarity(paper, w.DisplayTitle()) < StrongTitleSimilarity {
				continue
			}
			title = w.DisplayTitle()
			for _, a := range w.Authorships {
				if id, ok := NormalizeOpenAlexID(a.Author.ID); ok {
					ids[id] = a.Author.DisplayName
				}
			}
		}
		if title == "" {
			return nil, "", &ToolError{Code: CodeNoResults, Message: fmt.Sprintf("no OpenAlex work titled %q", paper)}
		}
		return ids, title, nil
	}
	ids := map[string]string{}
	for _, a := range work.Authorships {
		if id, ok := NormalizeOpenAlexID(a.Author.ID); ok {
			ids[id] = a.Author.DisplayName
		}
	}
	return ids, work.DisplayTitle(), nil
}

func buildOpenAlexAuthor(ctx context.Context, requester *Requester, r openAlexAuthorResult) (*AuthorInfo, *ToolError) {
	affiliation := "N/A"
	if insts := r.institutions(); len(insts) > 0 {
		affiliation = insts[0]
	}

	interests := make([]string, 0, 5)
	for _, t := range r.Topics {
		if len(interests) >= 5 {
			break
		}
		if topic := normalizeSpace(t.DisplayName); topic != "" {
			interests = append(interests, topic)
		}
	}
	for _, c := range r.XConcepts {
		if len(interests) >= 5 || len(r.Topics) > 0 {
			break
		}
		if topic := normalizeSpace(c.DisplayName); topic != "" {
			interests = append(interests, topic)
		}
	}

	metrics := AuthorMetrics{
		CitedByCount: r.CitedByCount,
		WorksCount:   r.WorksCount,
		Source:       "openalex",
		Scope:        "author_profile",
		Note:         "Totals reported by the OpenAlex author profile; OpenAlex may split or merge profiles.",
	}
	if r.SummaryStats != nil {
		metrics.HIndex = r.SummaryStats.HIndex
		metrics.I10Index = r.SummaryStats.I10Index
	}

	externalIDs := map[string]string{}
	if id := r.shortID(); id != "" {
		externalIDs["openalex"] = openAlexAuthorURL(id)
	}
	if id := r.orcid(); id != "" {
		externalIDs["orcid"] = orcidURL(id)
	}

	author := &AuthorInfo{
		Name:         normalizeSpace(r.DisplayName),
		Affiliation:  affiliation,
		Interests:    interests,
		Metrics:      metrics,
		Publications: []Publication{},
		Source:       "openalex",
		ExternalIDs:  externalIDs,
	}

	if id := r.shortID(); id != "" {
		params := url.Values{}
		params.Set("filter", "author.id:"+id)
		params.Set("sort", "cited_by_count:desc")
		params.Set("per-page", strconv.Itoa(authorSampleSize))
		params.Set("select", "id,doi,display_name,publication_year,cited_by_count")
		doc, err := requester.GetJSON(ctx, "https://api.openalex.org/works?"+params.Encode())
		switch {
		case err != nil || doc.Status != http.StatusOK:
			if ctxErr := ContextError(ctx.Err(), "author works lookup"); ctxErr != nil {
				return nil, ctxErr
			}
			author.Warnings = append(author.Warnings, "publication sample unavailable from OpenAlex")
		default:
			var works openAlexWorksSearchResponse
			if json.Unmarshal(doc.Body, &works) == nil {
				for _, w := range works.Results {
					author.Publications = append(author.Publications, publicationFromWork(w))
				}
			}
		}
		author.PublicationSample = &PublicationSample{
			Scope:  "top_cited_works",
			Source: "openalex",
			Count:  len(author.Publications),
			Note:   "The author's most-cited works; citations are per work, not author totals.",
		}
	}
	return author, nil
}

func publicationFromWork(w OpenAlexWork) Publication {
	year := "N/A"
	if w.PublicationYear > 0 {
		year = strconv.Itoa(w.PublicationYear)
	}
	title := w.DisplayTitle()
	if title == "" {
		title = "N/A"
	}
	doi, _ := NormalizeDOI(w.DOI)
	return Publication{Title: title, Year: year, Citations: derefInt(w.CitedByCount), DOI: doi}
}

// external_ids keep the URL forms earlier releases returned; candidates and
// match evidence use the bare identifiers.
func openAlexAuthorURL(id string) string { return "https://openalex.org/" + id }
func orcidURL(id string) string          { return "https://orcid.org/" + id }
func scholarProfileURL(userID string) string {
	return "https://scholar.google.com/citations?user=" + url.QueryEscape(userID)
}

// ---- ORCID ---------------------------------------------------------------

func orcidRecord(ctx context.Context, requester *Requester, orcidID string) (*AuthorInfo, *ToolError) {
	doc, err := requester.GetJSON(ctx, "https://pub.orcid.org/v3.0/"+orcidID+"/person")
	if err != nil {
		return nil, requestError("orcid", err)
	}
	if doc.Status == http.StatusNotFound {
		return nil, &ToolError{Code: CodeNoResults, Message: "ORCID iD not found: " + orcidID}
	}
	if doc.Status != http.StatusOK {
		return nil, statusError("orcid", doc)
	}
	body := doc.Body
	var person struct {
		Name *struct {
			GivenNames *struct {
				Value string `json:"value"`
			} `json:"given-names"`
			FamilyName *struct {
				Value string `json:"value"`
			} `json:"family-name"`
			CreditName *struct {
				Value string `json:"value"`
			} `json:"credit-name"`
		} `json:"name"`
	}
	if err := json.Unmarshal(body, &person); err != nil {
		return nil, &ToolError{Code: CodeParseFailed, Message: fmt.Sprintf("orcid record parse failed: %v", err)}
	}
	name := ""
	if person.Name != nil {
		if person.Name.CreditName != nil {
			name = person.Name.CreditName.Value
		}
		if name == "" {
			parts := []string{}
			if person.Name.GivenNames != nil {
				parts = append(parts, person.Name.GivenNames.Value)
			}
			if person.Name.FamilyName != nil {
				parts = append(parts, person.Name.FamilyName.Value)
			}
			name = strings.Join(parts, " ")
		}
	}
	return &AuthorInfo{
		Name:        normalizeSpace(name),
		Affiliation: "N/A",
		Metrics:     unknownMetrics("orcid", "ORCID records do not include citation metrics."),
		Source:      "orcid",
		ExternalIDs: map[string]string{"orcid": orcidURL(orcidID)},
	}, nil
}

func unknownMetrics(source, note string) AuthorMetrics {
	return AuthorMetrics{Source: source, Scope: "unavailable", Note: note}
}

func getAuthorInfoFromORCID(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	name := ParsePersonName(q.Name)
	var query string
	if len(name.Given) > 0 {
		query = fmt.Sprintf("family-name:(%s) AND given-names:(%s)", orcidQuote(name.Family), orcidQuote(strings.Join(name.Given, " ")))
	} else {
		query = fmt.Sprintf("family-name:(%s)", orcidQuote(name.Family))
	}
	fields := "orcid,given-names,family-name,current-institution-affiliation-name"
	searchURL := "https://pub.orcid.org/v3.0/csv-search/?q=" + url.QueryEscape(query) + "&fl=" + url.QueryEscape(fields) + "&rows=20"
	doc, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, requestError("orcid", err)
	}
	if doc.Status != http.StatusOK {
		return nil, statusError("orcid", doc)
	}
	body := doc.Body

	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		return nil, &ToolError{Code: CodeParseFailed, Message: fmt.Sprintf("orcid csv parse failed: %v", err)}
	}
	if len(records) < 2 {
		return nil, &ToolError{Code: CodeNoResults, Message: "orcid author not found"}
	}
	idx := map[string]int{}
	for i, h := range records[0] {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	get := func(row []string, key string) string {
		i, ok := idx[key]
		if !ok || i >= len(row) {
			return ""
		}
		return normalizeSpace(row[i])
	}

	type orcidHit struct {
		id, name, aff string
		affMatch      bool
	}
	hits := []orcidHit{}
	for _, row := range records[1:] {
		full := normalizeSpace(get(row, "given-names") + " " + get(row, "family-name"))
		id, ok := NormalizeORCID(get(row, "orcid"))
		if !ok || CompareNames(q.Name, full) == NameMismatch {
			continue
		}
		h := orcidHit{id: id, name: full, aff: get(row, "current-institution-affiliation-name")}
		if q.Affiliation != "" && matchingInstitution(q.Affiliation, strings.Split(h.aff, ",")) != "" {
			h.affMatch = true
		}
		hits = append(hits, h)
	}
	if len(hits) == 0 {
		return nil, &ToolError{Code: CodeNoResults, Message: "orcid author not found"}
	}

	var chosen *orcidHit
	method, confidence := "name_unique", confidenceLow
	affHits := []orcidHit{}
	for _, h := range hits {
		if h.affMatch {
			affHits = append(affHits, h)
		}
	}
	switch {
	case len(affHits) == 1:
		chosen, method, confidence = &affHits[0], "name+affiliation", confidenceMedium
	case len(hits) == 1 && q.Affiliation == "":
		chosen = &hits[0]
	}
	if chosen == nil {
		cands := []Candidate{}
		for _, h := range hits {
			if len(cands) >= maxAuthorCandidates {
				break
			}
			cands = append(cands, Candidate{ID: "orcid:" + h.id, Label: h.name, ORCID: h.id, Affiliation: h.aff, Source: "orcid"})
		}
		return nil, &ToolError{
			Code:       CodeAmbiguous,
			Message:    fmt.Sprintf("%d ORCID records match %q and the supplied evidence does not single one out", len(hits), q.Name),
			Hint:       "Call again with orcid from a candidate.",
			Candidates: cands,
		}
	}

	affiliation := chosen.aff
	if affiliation == "" {
		affiliation = "N/A"
	}
	evidence := []string{"name matches ORCID record"}
	if chosen.affMatch {
		evidence = append(evidence, "current affiliation matches")
	}
	return &AuthorInfo{
		Name:        chosen.name,
		Affiliation: affiliation,
		Metrics:     unknownMetrics("orcid", "ORCID records do not include citation metrics."),
		Source:      "orcid",
		ExternalIDs: map[string]string{"orcid": orcidURL(chosen.id)},
		Match:       &MatchInfo{Method: method, Confidence: confidence, Evidence: evidence},
	}, nil
}

// orcidQuote escapes a value for the ORCID (Solr) query syntax.
func orcidQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// ---- Crossref ------------------------------------------------------------

// getAuthorInfoFromCrossref is a last resort. Crossref has no author
// profiles, so the result is a sample of works whose author list contains a
// matching name; they may belong to different people, and no author-level
// metrics are reported.
func getAuthorInfoFromCrossref(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	searchURL := "https://api.crossref.org/works?query.author=" + url.QueryEscape(q.Name) + "&rows=20&select=DOI,title,author,issued,is-referenced-by-count"
	doc, err := requester.GetJSON(ctx, searchURL)
	if err != nil {
		return nil, requestError("crossref", err)
	}
	if doc.Status != http.StatusOK {
		return nil, statusError("crossref", doc)
	}
	var resp crossrefWorksResponse
	if err := json.Unmarshal(doc.Body, &resp); err != nil {
		return nil, &ToolError{Code: CodeParseFailed, Message: fmt.Sprintf("crossref parse failed: %v", err)}
	}

	publications := []Publication{}
	affiliations := map[string]int{}
	orcids := map[string]bool{}
	for _, item := range resp.Message.Items {
		var match *crossrefAuthor
		for i := range item.Author {
			if CompareNames(q.Name, item.Author[i].FullName()) != NameMismatch {
				match = &item.Author[i]
				break
			}
		}
		if match == nil {
			continue
		}
		if id, ok := NormalizeORCID(match.ORCID); ok {
			orcids[id] = true
		}
		for _, aff := range match.Affiliation {
			if a := normalizeSpace(aff.Name); a != "" {
				affiliations[a]++
			}
		}
		if len(publications) >= authorSampleSize {
			continue
		}
		title := item.title()
		if title == "" {
			title = "N/A"
		}
		year := "N/A"
		if y := item.year(); y > 0 {
			year = strconv.Itoa(y)
		}
		doi, _ := NormalizeDOI(item.DOI)
		publications = append(publications, Publication{Title: title, Year: year, Citations: derefInt(item.IsReferencedByCount), DOI: doi})
	}
	if len(publications) == 0 {
		return nil, &ToolError{Code: CodeNoResults, Message: "crossref has no works with a matching author name"}
	}

	warnings := []string{"Crossref has no author profiles: these works were matched by name only and may belong to different people with the same name."}
	if len(orcids) > 1 {
		warnings = append(warnings, fmt.Sprintf("the matched works carry %d different ORCID iDs, so at least %d different people share this name", len(orcids), len(orcids)))
	}
	affiliation := "N/A"
	if len(affiliations) == 1 {
		for a := range affiliations {
			affiliation = a
		}
	}
	externalIDs := map[string]string{}
	if len(orcids) == 1 {
		for id := range orcids {
			externalIDs["orcid"] = orcidURL(id)
		}
	}
	return &AuthorInfo{
		Name:         q.Name,
		Affiliation:  affiliation,
		Metrics:      unknownMetrics("crossref", "Crossref does not provide author-level metrics; per-work citation counts are not summed."),
		Publications: publications,
		PublicationSample: &PublicationSample{
			Scope:  "name_matched_works_unverified",
			Source: "crossref",
			Count:  len(publications),
			Note:   "Works whose author list contains a matching name; citations are per work (is-referenced-by-count).",
		},
		Source:      "crossref",
		ExternalIDs: externalIDs,
		Match:       &MatchInfo{Method: "name_only_works", Confidence: confidenceLow, Evidence: []string{"author name appears on matched works"}},
		Warnings:    warnings,
	}, nil
}

// ---- Google Scholar ------------------------------------------------------

type scholarProfileHit struct {
	author     AuthorInfo
	profileURL string
	userID     string
}

func getAuthorInfoFromScholar(ctx context.Context, requester *Requester, q AuthorQuery) (*AuthorInfo, *ToolError) {
	searchURL := "https://scholar.google.com/citations?view_op=search_authors&mauthors=" + url.QueryEscape(q.Name)
	doc, err := requester.Get(ctx, searchURL)
	if err != nil {
		return nil, requestError("google scholar", err)
	}
	body, status := doc.Body, doc.Status
	if status != http.StatusOK {
		if status == http.StatusForbidden || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			return nil, BuildBlockedError(doc)
		}
		return nil, &ToolError{Code: CodeUpstreamError, Message: fmt.Sprintf("author search failed with status %d", status)}
	}

	hits, parseErr := parseAuthorSearchResults(body)
	if parseErr != nil {
		return nil, &ToolError{Code: CodeParseFailed, Message: parseErr.Error()}
	}
	if len(hits) == 0 {
		if looksLikeBlocked(body) {
			return nil, &ToolError{Code: CodeBlocked, Message: "Google Scholar appears to have blocked this automated request", Retryable: true}
		}
		return nil, &ToolError{Code: CodeNoResults, Message: "author not found"}
	}

	matches := []scholarProfileHit{}
	for _, h := range hits {
		if CompareNames(q.Name, h.author.Name) != NameMismatch {
			matches = append(matches, h)
		}
	}
	if q.Affiliation != "" {
		affMatches := []scholarProfileHit{}
		for _, h := range matches {
			if matchingInstitution(q.Affiliation, []string{h.author.Affiliation}) != "" {
				affMatches = append(affMatches, h)
			}
		}
		if len(affMatches) > 0 {
			matches = affMatches
		}
	}
	if len(matches) == 0 {
		return nil, &ToolError{Code: CodeNoResults, Message: "no Google Scholar profile matches the name"}
	}
	if len(matches) > 1 {
		cands := []Candidate{}
		for _, h := range matches {
			if len(cands) >= maxAuthorCandidates {
				break
			}
			cands = append(cands, Candidate{ID: "scholar:" + h.userID, Label: h.author.Name, Affiliation: h.author.Affiliation, CitedByCount: h.author.Metrics.CitedByCount, Source: "google_scholar"})
		}
		return nil, &ToolError{Code: CodeAmbiguous, Message: fmt.Sprintf("%d Google Scholar profiles match %q", len(matches), q.Name), Hint: "Add affiliation evidence or use an ORCID / OpenAlex ID.", Candidates: cands}
	}

	hit := matches[0]
	author := hit.author
	author.Source = "google_scholar"
	author.ExternalIDs = map[string]string{}
	if hit.userID != "" {
		author.ExternalIDs["google_scholar"] = scholarProfileURL(hit.userID)
	}
	evidence := []string{"only name-compatible Google Scholar profile in search results"}
	confidence := confidenceLow
	if q.Affiliation != "" && matchingInstitution(q.Affiliation, []string{author.Affiliation}) != "" {
		evidence = append(evidence, "affiliation matches")
		confidence = confidenceMedium
	}
	author.Match = &MatchInfo{Method: "name_unique", Confidence: confidence, Evidence: evidence}

	if hit.profileURL != "" {
		if profile, profileErr := requester.Get(ctx, hit.profileURL); profileErr == nil && profile.Status == http.StatusOK {
			fillAuthorFromProfile(&author, profile.Body)
		} else {
			author.Warnings = append(author.Warnings, "Google Scholar profile page unavailable; publication sample omitted")
		}
	}
	return &author, nil
}

func parseAuthorSearchResults(html []byte) ([]scholarProfileHit, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, err
	}
	hits := []scholarProfileHit{}
	doc.Find("div.gsc_1usr").Each(func(_ int, sel *goquery.Selection) {
		name := normalizeSpace(sel.Find("h3.gs_ai_name a").First().Text())
		if name == "" {
			return
		}
		affiliation := normalizeSpace(sel.Find("div.gs_ai_aff").First().Text())
		if affiliation == "" {
			affiliation = "N/A"
		}
		interests := []string{}
		sel.Find("a.gs_ai_one_int").Each(func(_ int, s *goquery.Selection) {
			if t := normalizeSpace(s.Text()); t != "" {
				interests = append(interests, t)
			}
		})
		var citedBy *int
		citedText := normalizeSpace(sel.Find("div.gs_ai_cby").First().Text())
		if idx := strings.LastIndex(citedText, " "); idx > -1 {
			if v, err := strconv.Atoi(strings.TrimSpace(citedText[idx+1:])); err == nil {
				citedBy = IntPtr(v)
			}
		}
		href, _ := sel.Find("h3.gs_ai_name a").First().Attr("href")
		profileURL, userID := "", ""
		if strings.TrimSpace(href) != "" {
			profileURL = "https://scholar.google.com" + href
			if u, err := url.Parse(href); err == nil {
				userID = u.Query().Get("user")
			}
		}
		hits = append(hits, scholarProfileHit{
			author: AuthorInfo{
				Name:         name,
				Affiliation:  affiliation,
				Interests:    interests,
				Publications: []Publication{},
				Metrics: AuthorMetrics{
					CitedByCount: citedBy,
					Source:       "google_scholar",
					Scope:        "author_profile",
					Note:         "Total citations shown on the Google Scholar profile.",
				},
			},
			profileURL: profileURL,
			userID:     userID,
		})
	})
	return hits, nil
}

func fillAuthorFromProfile(author *AuthorInfo, html []byte) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return
	}

	if author.Affiliation == "N/A" {
		if aff := normalizeSpace(doc.Find(".gsc_prf_il").First().Text()); aff != "" {
			author.Affiliation = aff
		}
	}

	// The citation stats table lists All / Since-year columns for citations,
	// h-index and i10-index.
	doc.Find("#gsc_rsb_st tbody tr").Each(func(_ int, row *goquery.Selection) {
		label := strings.ToLower(normalizeSpace(row.Find("td").First().Text()))
		value, err := strconv.Atoi(normalizeSpace(row.Find("td.gsc_rsb_std").First().Text()))
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(label, "citations"):
			author.Metrics.CitedByCount = IntPtr(value)
		case strings.HasPrefix(label, "h-index"):
			author.Metrics.HIndex = IntPtr(value)
		case strings.HasPrefix(label, "i10-index"):
			author.Metrics.I10Index = IntPtr(value)
		}
	})

	publications := make([]Publication, 0, authorSampleSize)
	doc.Find("tr.gsc_a_tr").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if len(publications) >= authorSampleSize {
			return false
		}
		title := normalizeSpace(sel.Find("a.gsc_a_at").First().Text())
		if title == "" {
			title = "N/A"
		}
		citations := 0
		if c, err := strconv.Atoi(normalizeSpace(sel.Find("a.gsc_a_ac").First().Text())); err == nil {
			citations = c
		}
		year := normalizeSpace(sel.Find("span.gsc_a_h").First().Text())
		if year == "" {
			year = normalizeSpace(sel.Find("span.gsc_a_y").First().Text())
		}
		if year == "" {
			year = "N/A"
		}
		publications = append(publications, Publication{Title: title, Year: year, Citations: citations})
		return true
	})
	author.Publications = publications
	author.PublicationSample = &PublicationSample{
		Scope:  "scholar_profile_first_page",
		Source: "google_scholar",
		Count:  len(publications),
		Note:   "First rows of the Google Scholar profile (sorted by citations by default); citations are per work.",
	}
}
