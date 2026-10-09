package fulltext

import (
	"strings"
	"testing"
)

func TestConvertHTMLKeepsArticleDropsChrome(t *testing.T) {
	markdown, err := convertHTML(loadFixture(t, "arxiv_fragment.html"), "https://arxiv.org/html/0000.00000")
	if err != nil {
		t.Fatalf("convertHTML error: %v", err)
	}

	for _, want := range []string{"A Sample Paper on Testing", "1 Introduction", "2 Method", "introduction paragraph", "$x$"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, markdown)
		}
	}
	for _, junk := range []string{"NAVJUNK", "HEADERJUNK", "FOOTERJUNK", "SCRIPTJUNK"} {
		if strings.Contains(markdown, junk) {
			t.Errorf("markdown contains stripped content %q\n---\n%s", junk, markdown)
		}
	}
	if !strings.Contains(markdown, "https://arxiv.org/abs/1706.03762") {
		t.Errorf("root-relative link not resolved\n---\n%s", markdown)
	}
}

func TestConvertHTMLResolvesRelativeLinksAgainstFullURL(t *testing.T) {
	conv, err := convertHTMLDetailed(loadFixture(t, "paper_full.html"), "https://journal.example.org/articles/2024/paper.html?view=full")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://journal.example.org/articles/2024/figures/fig1.png",
		"https://journal.example.org/articles/supplement.pdf",
	} {
		if !strings.Contains(conv.Markdown, want) {
			t.Errorf("markdown missing resolved link %q\n---\n%s", want, conv.Markdown)
		}
	}
	if !strings.Contains(conv.Markdown, "| Model | Score |") || !strings.Contains(conv.Markdown, "| A     | 0.91  |") {
		t.Errorf("table not converted to markdown\n---\n%s", conv.Markdown)
	}
}

func TestConvertHTMLHonoursBaseHref(t *testing.T) {
	html := []byte(`<html><head><base href="/static/v2/"></head><body><article><p><a href="fig.png">fig</a></p></article></body></html>`)
	markdown, err := convertHTML(html, "https://example.org/a/b/page")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, "https://example.org/static/v2/fig.png") {
		t.Fatalf("markdown = %q", markdown)
	}
}

func TestConvertHTMLPreservesMath(t *testing.T) {
	conv, err := convertHTMLDetailed(loadFixture(t, "latexml_math.html"), "https://arxiv.org/html/2401.00001")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$x_{i}^{2}$", "$$E=mc^{2}$$ (1)", "$y$"} {
		if !strings.Contains(conv.Markdown, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, conv.Markdown)
		}
	}
	if len(conv.Limitations) == 0 || !strings.Contains(conv.Limitations[0], "1 of 3 math expressions") {
		t.Errorf("limitations = %v, want note about the unannotated formula", conv.Limitations)
	}
}

func TestConvertHTMLFallsBackToBody(t *testing.T) {
	html := []byte("<html><body><p>Just a plain page without an article element.</p></body></html>")
	markdown, err := convertHTML(html, "https://example.org/x")
	if err != nil {
		t.Fatalf("convertHTML error: %v", err)
	}
	if !strings.Contains(markdown, "plain page") {
		t.Fatalf("markdown = %q", markdown)
	}
}

func TestEmbeddedFullTextLinks(t *testing.T) {
	html := []byte(`<html><head>
<meta name="citation_pdf_url" content="pdf/main.pdf">
<meta name="eprints.document_url" content="https://eprints.example.ac.uk/1/1/paper.pdf">
<link rel="alternate" type="application/pdf" href="/alt.pdf">
<meta name="citation_fulltext_html_url" content="https://journal.example.org/full/1">
</head><body></body></html>`)
	links := embeddedFullTextLinks(html, "https://journal.example.org/article/1/abstract")
	want := []string{
		"https://journal.example.org/article/1/pdf/main.pdf",
		"https://eprints.example.ac.uk/1/1/paper.pdf",
		"https://journal.example.org/alt.pdf",
		"https://journal.example.org/full/1",
	}
	if len(links) != len(want) {
		t.Fatalf("links = %+v", links)
	}
	for i, w := range want {
		if links[i].URL != w {
			t.Errorf("links[%d] = %q, want %q", i, links[i].URL, w)
		}
	}
}

func TestClassifyHTML(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{"paper_full.html", StatusFullText},
		{"landing_long_abstract.html", StatusAbstractOnly},
		{"login_wall.html", statusAccessRestricted},
		{"challenge.html", statusChallenge},
	}
	for _, c := range cases {
		body := loadFixture(t, c.fixture)
		markdown, err := convertHTML(body, "https://example.org/x")
		if err != nil {
			t.Fatal(err)
		}
		if got := classifyHTML(body, markdown); got.Status != c.want {
			t.Errorf("%s: status = %s (%v), want %s", c.fixture, got.Status, got.Reasons, c.want)
		}
	}
	// The long abstract page is well past the old 500-character threshold.
	markdown, _ := convertHTML(loadFixture(t, "landing_long_abstract.html"), "https://example.org/x")
	if len(markdown) < 3000 {
		t.Fatalf("fixture should be long; got %d chars", len(markdown))
	}
}

func TestClassifyPDF(t *testing.T) {
	if got := classifyPDF("tiny", 1); got.Status != statusEmpty {
		t.Fatalf("empty = %s", got.Status)
	}
	short := strings.Repeat("An extended abstract sentence. ", 40)
	if got := classifyPDF(short, 1); got.Status != StatusPartial {
		t.Fatalf("short = %s", got.Status)
	}
	full := "## 1 Introduction\n" + short + "\n## 2 Methods\n" + short + "\n## 3 Results\n" + short + "\n## References\n[1] x"
	if got := classifyPDF(full, 2); got.Status != StatusFullText {
		t.Fatalf("full = %s", got.Status)
	}
}
