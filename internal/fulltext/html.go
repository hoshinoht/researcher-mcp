package fulltext

import (
	"bytes"
	"net/url"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/PuerkitoBio/goquery"
)

// mainContentSelectors are tried in order; the first non-empty match is the
// article body. article.ltx_document is the arXiv/ar5iv LaTeXML container.
var mainContentSelectors = []string{"article.ltx_document", "article", "main", "body"}

// strippedSelectors is chrome and duplicate content removed before
// conversion. annotation/annotation-xml hold LaTeX/MathML source duplicates.
const strippedSelectors = "script, style, nav, header, footer, aside, form, noscript, iframe, annotation, annotation-xml, .ltx_page_footer, .ltx_page_header"

func convertHTML(body []byte, pageURL string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	var sel *goquery.Selection
	for _, selector := range mainContentSelectors {
		if s := doc.Find(selector).First(); s.Length() > 0 {
			sel = s
			break
		}
	}
	if sel == nil {
		sel = doc.Selection
	}
	sel.Find(strippedSelectors).Remove()

	fragment, err := goquery.OuterHtml(sel)
	if err != nil {
		return "", err
	}

	opts := []converter.ConvertOptionFunc{}
	if domain := domainOf(pageURL); domain != "" {
		opts = append(opts, converter.WithDomain(domain))
	}

	markdown, err := htmltomarkdown.ConvertString(fragment, opts...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(markdown), nil
}

func domainOf(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
