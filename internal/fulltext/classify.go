package fulltext

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// Content statuses, from most to least complete.
const (
	// StatusFullText: structural evidence (sections, references, a LaTeXML
	// article, or a multi-page PDF) indicates the complete paper.
	StatusFullText = "full_text"
	// StatusUnverified: substantial text without the structure needed to
	// confirm it is the complete paper.
	StatusUnverified = "unverified"
	// StatusPartial: some body text, but clearly not the complete paper
	// (e.g. a preview, a short excerpt or an extended abstract).
	StatusPartial = "partial"
	// StatusAbstractOnly: an abstract (from a landing page or metadata).
	StatusAbstractOnly = "abstract_only"
	// StatusLandingPage: a publisher landing page without paper text.
	StatusLandingPage = "landing_page"

	// Non-content outcomes; never returned as content.
	statusAccessRestricted = "access_restricted"
	statusChallenge        = "challenge"
	statusEmpty            = "empty"
)

// statusRank orders returnable statuses for choosing the best partial result.
var statusRank = map[string]int{
	StatusFullText:     5,
	StatusUnverified:   4,
	StatusPartial:      3,
	StatusAbstractOnly: 2,
	StatusLandingPage:  1,
}

func isReturnableStatus(status string) bool {
	return statusRank[status] >= statusRank[StatusAbstractOnly]
}

type classification struct {
	Status  string
	Reasons []string
}

var (
	paperSectionPattern = regexp.MustCompile(`(?im)^(?:#{1,6}\s*|\*\*)?(?:[ivx\d]+(?:\.\d+)*\.?\s+)?(abstract|introduction|background|related work|preliminaries|methods?|methodology|materials and methods|approach|experiments?|experimental setup|results?|results and discussion|discussion|evaluation|analysis|conclusions?|concluding remarks|limitations|future work|references|bibliography|literature cited|acknowledge?ments?|appendix)\b`)
	referencesPattern   = regexp.MustCompile(`(?im)^(?:#{1,6}\s*|\*\*)?(?:[ivx\d]+\.?\s+)?(references|bibliography|literature cited|works cited)\b`)
	abstractPattern     = regexp.MustCompile(`(?im)^(?:#{1,6}\s*|\*\*)?(abstract|summary)\b`)

	challengeMarkers = []string{
		"cf-browser-verification", "challenge-platform", "cf-challenge", "<title>just a moment",
		"attention required! | cloudflare", "verify you are human", "are you a robot",
		"please enable js and disable any ad blocker", "unusual traffic from your computer",
		"g-recaptcha", "h-captcha", "captcha-delivery.com", "perimeterx", "_incapsula_resource",
		"enable javascript and cookies to continue", "checking your browser before accessing",
	}
	accessMarkers = []string{
		"access through your institution", "institutional login", "institutional access",
		"purchase this article", "buy this article", "purchase pdf", "rent this article",
		"get access", "subscribe to journal", "log in to access", "sign in to access",
		"login to view", "full text access", "access options", "you do not have access",
		"this content is only available", "check access",
	}
)

// backMatter sections appear on landing pages too, so they are not counted
// as evidence of a paper body.
var backMatter = map[string]bool{
	"abstract": true, "references": true, "bibliography": true, "literature cited": true,
	"acknowledgements": true, "acknowledgments": true, "acknowledgement": true, "acknowledgment": true, "appendix": true,
}

// sectionCount counts distinct recognised body sections in markdown text.
func sectionCount(markdown string) int {
	seen := map[string]bool{}
	for _, m := range paperSectionPattern.FindAllStringSubmatch(markdown, -1) {
		name := strings.ToLower(m[1])
		if !backMatter[name] {
			seen[name] = true
		}
	}
	return len(seen)
}

// detectChallenge reports bot-check / CAPTCHA interstitials.
func detectChallenge(rawHTML []byte) bool {
	head := rawHTML
	if len(head) > 200_000 {
		head = head[:200_000]
	}
	lower := strings.ToLower(string(head))
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// classifyHTML decides what an HTML page's extracted markdown represents.
func classifyHTML(rawHTML []byte, markdown string) classification {
	textLen := utf8.RuneCountInString(markdown)
	sections := sectionCount(markdown)
	hasRefs := referencesPattern.MatchString(markdown)

	doc, _ := goquery.NewDocumentFromReader(bytes.NewReader(rawHTML))
	latexml, hasPDFMeta, hasAbstractMeta, passwordField := false, false, false, false
	if doc != nil {
		latexml = doc.Find("article.ltx_document, .ltx_document").Length() > 0
		hasPDFMeta = doc.Find(`meta[name="citation_pdf_url"]`).Length() > 0
		hasAbstractMeta = doc.Find(`meta[name="citation_abstract"], meta[name="dc.description"], meta[name="DC.Description"], meta[property="og:description"]`).Length() > 0
		passwordField = doc.Find(`input[type="password"]`).Length() > 0
		if doc.Find(".ltx_bibliography, section.references, #references, .references, ol.references").Length() > 0 {
			hasRefs = true
		}
	}

	if detectChallenge(rawHTML) && textLen < 5000 {
		return classification{Status: statusChallenge, Reasons: []string{"bot-check / challenge page"}}
	}
	if textLen < 200 {
		return classification{Status: statusEmpty, Reasons: []string{"almost no text extracted"}}
	}

	lower := strings.ToLower(markdown)
	accessHits := 0
	for _, m := range accessMarkers {
		if strings.Contains(lower, m) {
			accessHits++
		}
	}
	if (accessHits >= 2 && sections < 3) || (passwordField && sections < 2) {
		return classification{Status: statusAccessRestricted, Reasons: []string{"login, purchase or subscription wall"}}
	}

	reasons := []string{}
	switch {
	case sections >= 3:
		reasons = append(reasons, "multiple paper sections detected")
	case sections >= 2 && hasRefs:
		reasons = append(reasons, "paper sections and a reference list detected")
	case latexml && textLen >= 3000:
		reasons = append(reasons, "LaTeXML article body")
	case hasRefs && sections >= 1 && textLen >= 15000:
		reasons = append(reasons, "long text with a reference list")
	}
	if len(reasons) > 0 {
		return classification{Status: StatusFullText, Reasons: reasons}
	}

	hasAbstract := abstractPattern.MatchString(markdown) || hasAbstractMeta
	switch {
	case hasAbstract && textLen < 15000:
		return classification{Status: StatusAbstractOnly, Reasons: []string{"abstract without paper body sections"}}
	case hasPDFMeta && textLen < 15000:
		return classification{Status: StatusLandingPage, Reasons: []string{"landing page linking to a PDF"}}
	case textLen >= 15000:
		return classification{Status: StatusUnverified, Reasons: []string{"long text but no section structure or references detected"}}
	case textLen >= 2000:
		return classification{Status: StatusPartial, Reasons: []string{"some text but no paper structure"}}
	default:
		return classification{Status: StatusLandingPage, Reasons: []string{"short page without paper structure"}}
	}
}

// classifyPDF decides what extracted PDF text represents.
func classifyPDF(text string, pages int) classification {
	textLen := utf8.RuneCountInString(text)
	sections := sectionCount(text)
	hasRefs := referencesPattern.MatchString(text)

	switch {
	case textLen < 200:
		return classification{Status: statusEmpty, Reasons: []string{"no text layer (possibly a scanned PDF; OCR is not supported)"}}
	case sections >= 3, hasRefs && sections >= 1 && textLen >= 5000, pages >= 4 && textLen >= 8000:
		return classification{Status: StatusFullText, Reasons: []string{"multi-page document with paper structure"}}
	case pages <= 2 && textLen < 6000:
		return classification{Status: StatusPartial, Reasons: []string{"short PDF; may be an abstract, poster or excerpt"}}
	default:
		return classification{Status: StatusUnverified, Reasons: []string{"PDF text without recognisable paper structure"}}
	}
}
