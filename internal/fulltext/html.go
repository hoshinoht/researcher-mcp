package fulltext

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	baseplugin "github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/PuerkitoBio/goquery"
)

// mainContentSelectors are tried in order; the first non-empty match is the
// article body. article.ltx_document is the arXiv/ar5iv LaTeXML container.
var mainContentSelectors = []string{"article.ltx_document", "article", "main", "[role=main]", "body"}

// strippedSelectors is chrome removed before conversion.
const strippedSelectors = "script, style, nav, header, footer, aside, form, noscript, iframe, button, .ltx_page_footer, .ltx_page_header"

// htmlConversion is converted markdown plus what the converter could not
// faithfully represent.
type htmlConversion struct {
	Markdown    string
	Limitations []string
}

func convertHTML(body []byte, pageURL string) (string, error) {
	conv, err := convertHTMLDetailed(body, pageURL)
	if err != nil {
		return "", err
	}
	return conv.Markdown, nil
}

func convertHTMLDetailed(body []byte, pageURL string) (*htmlConversion, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	base := documentBaseURL(doc, pageURL)
	absolutizeLinks(doc, base)

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

	limitations := []string{}
	math := protectMath(sel)
	if math.count > 0 && math.withoutTeX > 0 {
		limitations = append(limitations, fmt.Sprintf("%d of %d math expressions had no TeX source and were reduced to plain text", math.withoutTeX, math.count))
	}
	if n := sel.Find("img, svg, figure canvas").Length(); n > 0 {
		limitations = append(limitations, "figures are kept as image links only; their visual content is not extracted")
	}
	if sel.Find("td[rowspan], td[colspan], th[rowspan], th[colspan]").Length() > 0 {
		limitations = append(limitations, "tables with merged cells were flattened (merged cells repeated)")
	}

	fragment, err := goquery.OuterHtml(sel)
	if err != nil {
		return nil, err
	}

	conv := converter.NewConverter(converter.WithPlugins(
		baseplugin.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(
			table.WithSpanCellBehavior(table.SpanBehaviorMirror),
			table.WithNewlineBehavior(table.NewlineBehaviorPreserve),
		),
	))
	opts := []converter.ConvertOptionFunc{}
	if base != nil {
		opts = append(opts, converter.WithDomain(base.Scheme+"://"+base.Host))
	}
	markdown, err := conv.ConvertString(fragment, opts...)
	if err != nil {
		return nil, err
	}
	markdown = math.restore(markdown)
	return &htmlConversion{Markdown: strings.TrimSpace(markdown), Limitations: limitations}, nil
}

// documentBaseURL is the final page URL, overridden by a <base href> that
// resolves against it.
func documentBaseURL(doc *goquery.Document, pageURL string) *url.URL {
	page, err := url.Parse(pageURL)
	if err != nil || page.Scheme == "" || page.Host == "" {
		page = nil
	}
	if href, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if ref, err := url.Parse(strings.TrimSpace(href)); err == nil {
			if page != nil {
				return page.ResolveReference(ref)
			}
			if ref.IsAbs() {
				return ref
			}
		}
	}
	return page
}

// absolutizeLinks resolves relative href/src values against the complete
// base URL (path included), so "figures/x.png" on /html/2401.00001v1/ points
// below that path rather than at the site root.
func absolutizeLinks(doc *goquery.Document, base *url.URL) {
	if base == nil {
		return
	}
	resolve := func(attr string) func(int, *goquery.Selection) {
		return func(_ int, s *goquery.Selection) {
			v, ok := s.Attr(attr)
			v = strings.TrimSpace(v)
			if !ok || v == "" || strings.HasPrefix(v, "#") {
				return
			}
			ref, err := url.Parse(v)
			if err != nil || ref.Scheme == "javascript" || ref.Scheme == "data" || ref.Scheme == "mailto" {
				return
			}
			s.SetAttr(attr, base.ResolveReference(ref).String())
		}
	}
	doc.Find("a[href]").Each(resolve("href"))
	doc.Find("img[src]").Each(resolve("src"))
}

// mathProtector swaps math for placeholders before markdown conversion (so
// TeX is not escaped) and restores it afterwards.
type mathProtector struct {
	replacements []string
	count        int
	withoutTeX   int
}

func mathPlaceholder(i int) string { return fmt.Sprintf("RMCPMATH%dX", i) }

// protectMath converts LaTeXML/MathML equations to $...$ / $$...$$ using the
// alttext or TeX annotation, keeping equation numbers.
func protectMath(sel *goquery.Selection) *mathProtector {
	p := &mathProtector{}

	// LaTeXML display equations are laid out in tables; collapse each row to
	// one display formula with its tag before generic table conversion.
	sel.Find("table.ltx_equation, table.ltx_equationgroup").Each(func(_ int, t *goquery.Selection) {
		parts := []string{}
		t.Find("math").Each(func(_ int, m *goquery.Selection) {
			if tex, ok := mathTeX(m); ok {
				parts = append(parts, tex)
			} else {
				p.withoutTeX++
				parts = append(parts, strings.TrimSpace(m.Text()))
			}
			p.count++
		})
		if len(parts) == 0 {
			return
		}
		tag := strings.TrimSpace(t.Find(".ltx_tag_equation").First().Text())
		formula := "$$" + strings.Join(parts, " \\\\ ") + "$$"
		if tag != "" {
			formula += " " + tag
		}
		p.replacements = append(p.replacements, formula)
		t.ReplaceWithHtml("<p>" + mathPlaceholder(len(p.replacements)-1) + "</p>")
	})

	sel.Find("math").Each(func(_ int, m *goquery.Selection) {
		p.count++
		tex, ok := mathTeX(m)
		if !ok {
			p.withoutTeX++
			tex = strings.TrimSpace(m.Text())
		}
		delim := "$"
		if display, _ := m.Attr("display"); display == "block" {
			delim = "$$"
		}
		p.replacements = append(p.replacements, delim+tex+delim)
		m.ReplaceWithHtml(mathPlaceholder(len(p.replacements) - 1))
	})
	return p
}

func mathTeX(m *goquery.Selection) (string, bool) {
	if alt, ok := m.Attr("alttext"); ok && strings.TrimSpace(alt) != "" {
		return strings.TrimSpace(alt), true
	}
	if ann := m.Find(`annotation[encoding="application/x-tex"]`).First(); ann.Length() > 0 {
		if tex := strings.TrimSpace(ann.Text()); tex != "" {
			return tex, true
		}
	}
	return "", false
}

func (p *mathProtector) restore(markdown string) string {
	// Replace in reverse so RMCPMATH1X does not clobber RMCPMATH10X.
	for i := len(p.replacements) - 1; i >= 0; i-- {
		markdown = strings.ReplaceAll(markdown, mathPlaceholder(i), p.replacements[i])
	}
	return markdown
}
