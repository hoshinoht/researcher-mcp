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

	for _, want := range []string{"A Sample Paper on Testing", "1 Introduction", "2 Method", "introduction paragraph"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, markdown)
		}
	}
	for _, junk := range []string{"NAVJUNK", "HEADERJUNK", "FOOTERJUNK", "SCRIPTJUNK", "MATHJUNK"} {
		if strings.Contains(markdown, junk) {
			t.Errorf("markdown contains stripped content %q\n---\n%s", junk, markdown)
		}
	}
	if !strings.Contains(markdown, "https://arxiv.org/abs/1706.03762") {
		t.Errorf("relative link not resolved against domain\n---\n%s", markdown)
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
