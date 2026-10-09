package scholar

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	htmlTagPattern    = regexp.MustCompile(`<[^>]+>`)
	subtitleDelimiter = regexp.MustCompile(`\s*(?::|\?|\s[-–—]\s|\.\s)\s*`)
)

// NormalizeTitle folds a title to lowercase letters and digits separated by
// single spaces, dropping markup and punctuation, for identity comparison.
func NormalizeTitle(title string) string {
	title = htmlTagPattern.ReplaceAllString(title, " ")
	b := strings.Builder{}
	for _, r := range strings.ToLower(title) {
		r = foldDiacritic(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// StrongTitleSimilarity is the score at or above which two titles are taken
// to name the same work.
const StrongTitleSimilarity = 0.9

// unseparatedExtensionCap bounds the score of a title that is the other plus
// extra leading or trailing words without a subtitle delimiter, keeping it
// below StrongTitleSimilarity however many words the titles share.
const unseparatedExtensionCap = 0.85

// TitleSimilarity scores two titles in [0,1]. 1 means the normalized titles
// are identical; a title that is the other plus a subtitle scores 0.9; other
// pairs get the Dice coefficient of their word multisets, capped below the
// strong threshold when one title merely extends the other.
func TitleSimilarity(a, b string) float64 {
	na, nb := NormalizeTitle(a), NormalizeTitle(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	// "Title: Subtitle" matches "Title" only across an explicit subtitle
	// delimiter, so "Attention Is All You Need In Speech Separation" is not
	// taken for "Attention Is All You Need".
	short, longRaw := na, b
	if len(na) > len(nb) {
		short, longRaw = nb, a
	}
	if len(strings.Fields(short)) >= 3 {
		if head := subtitleDelimiter.Split(longRaw, 2)[0]; head != longRaw && NormalizeTitle(head) == short {
			return StrongTitleSimilarity
		}
	}

	ta, tb := strings.Fields(na), strings.Fields(nb)
	counts := map[string]int{}
	for _, t := range ta {
		counts[t]++
	}
	overlap := 0
	for _, t := range tb {
		if counts[t] > 0 {
			counts[t]--
			overlap++
		}
	}
	dice := 2 * float64(overlap) / float64(len(ta)+len(tb))
	if dice > unseparatedExtensionCap && unseparatedExtension(short, longRaw) {
		return unseparatedExtensionCap
	}
	return dice
}

// unseparatedExtension reports whether the normalized title short is the
// leading or trailing word run of longRaw without a subtitle delimiter at
// the boundary ("a b c d e" inside "a b c d e f").
func unseparatedExtension(short, longRaw string) bool {
	long := NormalizeTitle(longRaw)
	prefix := strings.HasPrefix(long, short+" ")
	suffix := strings.HasSuffix(long, " "+short)
	if !prefix && !suffix {
		return false
	}
	for _, loc := range subtitleDelimiter.FindAllStringIndex(longRaw, -1) {
		if (prefix && NormalizeTitle(longRaw[:loc[0]]) == short) || (suffix && NormalizeTitle(longRaw[loc[1]:]) == short) {
			return false
		}
	}
	return true
}

// PersonName is a parsed personal name.
type PersonName struct {
	Given  []string
	Family string
}

// Tokens returns the canonical lowercase tokens of the name.
func (p PersonName) Tokens() []string {
	return append(append([]string{}, p.Given...), p.Family)
}

func (p PersonName) String() string {
	return strings.Join(p.Tokens(), " ")
}

var surnameParticles = map[string]bool{
	"van": true, "von": true, "der": true, "den": true, "de": true, "del": true, "della": true,
	"da": true, "di": true, "dos": true, "das": true, "du": true, "la": true, "le": true, "ter": true, "ten": true, "bin": true, "al": true,
}

// ParsePersonName parses "Given Family" or inverted "Family, Given" forms.
func ParsePersonName(raw string) PersonName {
	raw = normalizeSpace(raw)
	if left, right, ok := strings.Cut(raw, ","); ok && !strings.Contains(right, ",") {
		family := canonicalTokens(left)
		given := canonicalTokens(right)
		if len(family) > 0 && len(given) > 0 {
			return PersonName{Given: given, Family: strings.Join(family, " ")}
		}
	}
	tokens := canonicalTokens(raw)
	if len(tokens) == 0 {
		return PersonName{}
	}
	// Keep lowercase particles with the family name: "Jan van der Berg".
	cut := len(tokens) - 1
	for cut > 1 && surnameParticles[tokens[cut-1]] {
		cut--
	}
	return PersonName{Given: tokens[:cut], Family: strings.Join(tokens[cut:], " ")}
}

func canonicalTokens(s string) []string {
	return strings.Fields(canonicalPersonName(s))
}

// looksInverted reports whether "left, right" is a single inverted name
// ("Li, Ying", "van der Berg, Jan", "Ng, Andrew Y.") rather than two people.
func looksInverted(left, right string) bool {
	lt, rt := strings.Fields(normalizeSpace(left)), strings.Fields(normalizeSpace(right))
	if len(lt) == 0 || len(rt) == 0 || len(rt) > 3 {
		return false
	}
	if len(lt) == 1 {
		return true
	}
	allParticles := true
	for _, t := range lt[:len(lt)-1] {
		if !surnameParticles[strings.ToLower(t)] {
			allParticles = false
		}
	}
	if allParticles {
		return true
	}
	if len(lt) <= 2 {
		for _, t := range rt {
			if isInitial(t) {
				return true
			}
		}
	}
	return false
}

func isInitial(token string) bool {
	t := strings.Trim(token, ".")
	if t == "" {
		return false
	}
	letters := 0
	for _, r := range t {
		if unicode.IsLetter(r) {
			letters++
		} else if r != '.' && r != '-' {
			return false
		}
	}
	return letters <= 2 && strings.ToUpper(t) == t
}

// SplitPeople splits an author_name input that names several people
// ("Ying Li; Lei Wu", "Ying Li, Lei Wu") while keeping an inverted single
// name ("Li, Ying") whole.
func SplitPeople(raw string) []string {
	raw = normalizeSpace(raw)
	if raw == "" {
		return nil
	}
	var parts []string
	switch {
	case strings.Contains(raw, ";"):
		parts = strings.Split(raw, ";")
	case strings.Count(raw, ",") == 1:
		left, right, _ := strings.Cut(raw, ",")
		if looksInverted(left, right) {
			return []string{raw}
		}
		parts = []string{left, right}
	case strings.Contains(raw, ","):
		parts = strings.Split(raw, ",")
	default:
		parts = []string{raw}
	}

	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		name := normalizeSpace(part)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// DisplayOrder rewrites an inverted single name ("Li, Ying") into given
// name first order ("Ying Li"), preserving the original casing.
func DisplayOrder(raw string) string {
	raw = normalizeSpace(raw)
	if left, right, ok := strings.Cut(raw, ","); ok && !strings.Contains(right, ",") {
		if l, r := normalizeSpace(left), normalizeSpace(right); l != "" && r != "" {
			return r + " " + l
		}
	}
	return raw
}

// Name match strengths, strongest first.
const (
	NameExact      = "exact"
	NameCompatible = "compatible"
	NameMismatch   = "mismatch"
)

// CompareNames compares a requested name with a candidate's name. Exact
// means the same tokens in any order (covers family-first conventions);
// compatible means the family names agree and every requested given name
// agrees with a candidate given name in full or as an initial.
func CompareNames(requested, candidate string) string {
	rq, cd := ParsePersonName(requested), ParsePersonName(candidate)
	if rq.Family == "" || cd.Family == "" {
		return NameMismatch
	}
	if sameTokenSet(rq.Tokens(), cd.Tokens()) {
		return NameExact
	}
	if rq.Family != cd.Family {
		return NameMismatch
	}
	if len(rq.Given) == 0 {
		return NameCompatible
	}
	for _, g := range rq.Given {
		matched := false
		for _, cg := range cd.Given {
			if g == cg || (len(g) == 1 && strings.HasPrefix(cg, g)) || (len(cg) == 1 && strings.HasPrefix(g, cg)) {
				matched = true
				break
			}
		}
		if !matched {
			return NameMismatch
		}
	}
	return NameCompatible
}

func sameTokenSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, t := range a {
		counts[t]++
	}
	for _, t := range b {
		counts[t]--
		if counts[t] < 0 {
			return false
		}
	}
	return true
}

// FamilyName returns the canonical family name of a person name.
func FamilyName(raw string) string { return ParsePersonName(raw).Family }

func canonicalPersonName(name string) string {
	name = normalizeSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}

	b := strings.Builder{}
	for _, r := range name {
		r = foldDiacritic(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}

	return normalizeSpace(b.String())
}

// foldDiacritic maps common Latin letters with diacritics to their base
// letter, so "García" and "Garcia" compare equal.
func foldDiacritic(r rune) rune {
	if r < 0xC0 {
		return r
	}
	if base, ok := diacriticFold[r]; ok {
		return base
	}
	return r
}

var diacriticFold = func() map[rune]rune {
	groups := map[rune]string{
		'a': "àáâãäåāăą", 'c': "çćĉċč", 'd': "ďđ", 'e': "èéêëēĕėęě", 'g': "ĝğġģ", 'h': "ĥħ",
		'i': "ìíîïĩīĭįı", 'j': "ĵ", 'k': "ķ", 'l': "ĺļľŀł", 'n': "ñńņňŉ", 'o': "òóôõöøōŏő",
		'r': "ŕŗř", 's': "śŝşšș", 't': "ţťŧț", 'u': "ùúûüũūŭůűų", 'w': "ŵ", 'y': "ýÿŷ", 'z': "źżž",
	}
	m := map[rune]rune{}
	for base, variants := range groups {
		for _, v := range variants {
			m[v] = base
		}
	}
	return m
}()
