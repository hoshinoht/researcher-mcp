package scholar

import (
	"reflect"
	"testing"
)

func TestSplitPeopleKeepsInvertedNamesWhole(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Li, Ying", []string{"Li, Ying"}},
		{"Ng, Andrew Y.", []string{"Ng, Andrew Y."}},
		{"van der Berg, Jan", []string{"van der Berg, Jan"}},
		{"Smith, J.", []string{"Smith, J."}},
		{"Ying Li, Lei Wu", []string{"Ying Li", "Lei Wu"}},
		{"Ying Li; Lei Wu", []string{"Ying Li", "Lei Wu"}},
		{"Ada Lovelace, Charles Babbage, Alan Turing", []string{"Ada Lovelace", "Charles Babbage", "Alan Turing"}},
		{"Ada Lovelace", []string{"Ada Lovelace"}},
	}
	for _, c := range cases {
		if got := SplitPeople(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitPeople(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisplayOrderAndCompareNames(t *testing.T) {
	if got := DisplayOrder("Li, Ying"); got != "Ying Li" {
		t.Fatalf("DisplayOrder = %q", got)
	}
	cases := []struct {
		a, b, want string
	}{
		{"Ying Li", "Ying Li", NameExact},
		{"Li, Ying", "Ying Li", NameExact},
		{"Li Ying", "Ying Li", NameExact},
		{"Andrew Ng", "Andrew Y. Ng", NameCompatible},
		{"A. Ng", "Andrew Y. Ng", NameCompatible},
		{"José García", "Jose Garcia", NameExact},
		{"Andrew Ng", "Xuedong Zhou", NameMismatch},
		{"Wei Wang", "Wei Zhang", NameMismatch},
		{"Bo Wang", "Wei Wang", NameMismatch},
	}
	for _, c := range cases {
		if got := CompareNames(c.a, c.b); got != c.want {
			t.Errorf("CompareNames(%q, %q) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

func TestTitleSimilarity(t *testing.T) {
	if s := TitleSimilarity("Attention Is All You Need", "attention is all you need."); s != 1 {
		t.Fatalf("normalized equal titles = %v, want 1", s)
	}
	if s := TitleSimilarity("Attention Is All You Need", "Attention Is All You Need: A Subtitle"); s != 0.9 {
		t.Fatalf("subtitle similarity = %v, want 0.9", s)
	}
	if s := TitleSimilarity("Attention Is All You Need", "Attention Is All You Need In Speech Separation"); s >= 0.9 {
		t.Fatalf("different paper similarity = %v, want < 0.9", s)
	}
	if s := TitleSimilarity("<i>In vivo</i> imaging", "In Vivo Imaging"); s != 1 {
		t.Fatalf("markup should be ignored, got %v", s)
	}
}

func TestNormalizeIdentifiers(t *testing.T) {
	if id, ok := NormalizeORCID("https://orcid.org/0000-0002-1825-0097"); !ok || id != "0000-0002-1825-0097" {
		t.Fatalf("NormalizeORCID valid = %q %v", id, ok)
	}
	if _, ok := NormalizeORCID("0000-0002-1825-0098"); ok {
		t.Fatal("NormalizeORCID accepted a bad checksum")
	}
	if id, ok := NormalizeOpenAlexID("https://openalex.org/A5023888391"); !ok || id != "A5023888391" {
		t.Fatalf("NormalizeOpenAlexID = %q %v", id, ok)
	}
	if id, ok := NormalizeDOI("https://doi.org/10.1109/CVPR.2016.90"); !ok || id != "10.1109/cvpr.2016.90" {
		t.Fatalf("NormalizeDOI = %q %v", id, ok)
	}
}
