package scholar

import "testing"

func TestParseSearchResultsHTML(t *testing.T) {
	html := []byte(`
<div class="gs_ri">
  <h3 class="gs_rt"><a href="https://example.org/p1">Paper One</a></h3>
  <div class="gs_a">A Author, B Author - 2024</div>
  <div class="gs_rs">This is an abstract snippet...</div>
</div>
<div class="gs_ri">
  <h3 class="gs_rt">Paper Two</h3>
  <div class="gs_a">C Author - 2023</div>
  <div class="gs_rs">Second abstract</div>
</div>
`)

	results, err := parseSearchResultsHTML(html, 5)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Title != "Paper One" {
		t.Fatalf("unexpected title: %s", results[0].Title)
	}
	if !results[0].SnippetTruncated {
		t.Fatalf("expected snippet_truncated=true for first record")
	}
	if results[1].URL != "No link available" {
		t.Fatalf("expected fallback URL for second record, got: %s", results[1].URL)
	}
}
