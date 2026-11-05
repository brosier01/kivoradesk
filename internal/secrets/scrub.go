package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"html"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Replacement stands for a value in a cleaned text. It does not say which one.
const Replacement = "[secret hidden]"

// minAnywhere is the shortest value hidden wherever it occurs. A shorter one,
// such as a PIN, would garble ordinary text: it is hidden as a whole word only.
const minAnywhere = 4

// CutMark marks where a text was cut. Cut in two, a value is no longer
// recognized: next to a mark, Clean also hides what may be the start or the end
// of one, from minAnywhere characters. Any "[... note ...]" is a mark, such as the
// "[... 120 bytes omitted ...]" of a command's output, with a line break or not
// between it and the text. Cut a text after cleaning it where possible: a part
// shorter than minAnywhere is not hidden.
const CutMark = "[... cut ...]"

// maxMarkNote bounds the note inside a cut mark.
const maxMarkNote = 64

// maxEscape is the most bytes an encoded character takes for a byte of the value:
// an entity such as &#0000065; for a letter.
const maxEscape = 12

// A Cleaner hides values from a text. A site can display what it was sent, as is
// or encoded: an echo of the form, the URL of a search, a script's own encoding.
// It is a safety net, for the encodings that sites and browsers commonly use,
// not a guarantee: a value can always be transformed some other way.
type Cleaner struct {
	values [][]rune
	short  [][]rune // under minAnywhere
	exact  []string // base64, where case matters
	folded []string // hexadecimal
	first  [256]bool
}

// NewCleaner returns a Cleaner for values.
func NewCleaner(values []string) *Cleaner {
	c := &Cleaner{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		runes := []rune(value)
		if len(value) < minAnywhere {
			c.short = append(c.short, runes)
			c.markFirst(runes[0])
			continue
		}
		c.values = append(c.values, runes)
		c.markFirst(runes[0])
		for _, form := range base64Forms(value) {
			c.exact = append(c.exact, form)
			c.first[form[0]] = true
		}
		form := hex.EncodeToString([]byte(value))
		c.folded = append(c.folded, form)
		c.first[form[0]] = true
		c.first[unicode.ToUpper(rune(form[0]))] = true
	}
	if len(c.values)+len(c.short) > 0 {
		// Where an encoded character starts, and a cut mark.
		c.first['%'], c.first['&'], c.first['\\'], c.first['['] = true, true, true, true
	}
	return c
}

func (c *Cleaner) markFirst(r rune) {
	for _, form := range []rune{r, unicode.ToLower(r), unicode.ToUpper(r)} {
		var buf [utf8.UTFMax]byte
		utf8.EncodeRune(buf[:], form)
		c.first[buf[0]] = true
	}
	if r == ' ' || r == '+' {
		c.first[' '], c.first['+'] = true, true
	}
}

// Clean returns text with every value replaced. What it does not hide comes out
// byte for byte, valid UTF-8 or not.
func (c *Cleaner) Clean(text string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		if !c.first[text[i]] {
			i++
			continue
		}
		if mark := cutMarkAt(text, i); mark > 0 {
			start, before := c.startBefore(text, last, i)
			after, end := c.endAfter(text, mark)
			if start < 0 && end < 0 {
				i = mark
				continue
			}
			if start >= 0 {
				b.WriteString(text[last:start])
				b.WriteString(Replacement)
				b.WriteString(text[before:mark])
			} else {
				b.WriteString(text[last:mark])
			}
			i, last = mark, mark
			if end >= 0 {
				b.WriteString(text[mark:after])
				b.WriteString(Replacement)
				i, last = end, end
			}
			continue
		}
		end := c.longestAt(text, i)
		if end <= i {
			i++
			continue
		}
		b.WriteString(text[last:i])
		b.WriteString(Replacement)
		i, last = end, end
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// longestAt returns where the longest match starting at i ends, or -1: a value
// inside a longer encoded form must not hide only a part of it.
func (c *Cleaner) longestAt(text string, i int) int {
	end := -1
	for _, value := range c.values {
		end = max(end, matchValue(text, i, value))
	}
	for _, value := range c.short {
		if e := matchValue(text, i, value); e > end && wholeWord(text, i, e) {
			end = e
		}
	}
	for _, form := range c.exact {
		if strings.HasPrefix(text[i:], form) {
			end = max(end, i+len(form))
		}
	}
	for _, form := range c.folded {
		if len(text)-i >= len(form) && strings.EqualFold(text[i:i+len(form)], form) {
			end = max(end, i+len(form))
		}
	}
	return end
}

func wholeWord(text string, start, end int) bool {
	before, _ := utf8.DecodeLastRuneInString(text[:start])
	after, _ := utf8.DecodeRuneInString(text[end:])
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	return !word(before) && !word(after)
}

// matchValue returns where value ends if it starts at text[i], each character
// being written as is, in another case or in a common encoding, or -1.
func matchValue(text string, i int, value []rune) int {
	for k, want := range value {
		if i >= len(text) {
			return -1
		}
		// An encoded character also starts with a character a value may hold, so
		// every reading can be right: "%2525" may be "%", or "%25", or a percent
		// sign followed by digits.
		for _, escape := range escapesAt(text[i:]) {
			if escape.size > 0 && sameRune(escape.r, want) {
				if end := matchValue(text, i+escape.size, value[k+1:]); end >= 0 {
					return end
				}
			}
		}
		r, n := utf8.DecodeRuneInString(text[i:])
		if !sameRune(r, want) {
			return -1
		}
		i += n
	}
	return i
}

// sameRune compares without case. A space and a plus sign are the same in a
// query string, and a non-breaking space looks like a space on a page.
func sameRune(a, b rune) bool {
	return unicode.ToLower(normalSpace(a)) == unicode.ToLower(normalSpace(b))
}

func normalSpace(r rune) rune {
	if r == '+' || r == ' ' {
		return ' '
	}
	return r
}

// escape is one encoded character; size is 0 if there is none.
type escape struct {
	r    rune
	size int
}

// escapesAt reads an encoded character at the start of s: percent-encoding, once
// or twice over, an HTML entity or a JSON escape.
func escapesAt(s string) [2]escape {
	var found [2]escape
	switch s[0] {
	case '%':
		found[0].r, found[0].size = decodePercent(s, 3)
		found[1].r, found[1].size = decodePercent(s, 5)
	case '&':
		found[0].r, found[0].size = decodeEntity(s)
	case '\\':
		found[0].r, found[0].size = decodeJSONEscape(s)
	}
	return found
}

func decodeEntity(s string) (rune, int) {
	end := strings.IndexByte(s[:min(len(s), 12)], ';')
	if end < 0 {
		return 0, 0
	}
	decoded := html.UnescapeString(s[:end+1])
	if decoded == s[:end+1] {
		return 0, 0
	}
	r, size := utf8.DecodeRuneInString(decoded)
	if size != len(decoded) {
		return 0, 0
	}
	return r, end + 1
}

// decodePercent reads one percent-encoded character. Each byte takes width
// characters: 3 for %HH, 5 for %25HH, which is encoded twice.
func decodePercent(s string, width int) (rune, int) {
	first, n := percentByte(s, width)
	if n == 0 {
		return 0, 0
	}
	size := 0
	switch {
	case first < utf8.RuneSelf:
		return rune(first), n
	case first>>5 == 0b110:
		size = 2
	case first>>4 == 0b1110:
		size = 3
	case first>>3 == 0b11110:
		size = 4
	default:
		return 0, 0
	}
	buf := []byte{first}
	for len(buf) < size {
		b, m := percentByte(s[n:], width)
		if m == 0 {
			return 0, 0
		}
		buf = append(buf, b)
		n += m
	}
	r, got := utf8.DecodeRune(buf)
	if r == utf8.RuneError || got != size {
		return 0, 0
	}
	return r, n
}

func percentByte(s string, width int) (byte, int) {
	prefix := "%"
	if width == 5 {
		prefix = "%25"
	}
	if !strings.HasPrefix(s, prefix) {
		return 0, 0
	}
	b, ok := hexByte(s[len(prefix):])
	if !ok {
		return 0, 0
	}
	return b, width
}

func hexByte(s string) (byte, bool) {
	if len(s) < 2 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[:2], 16, 8)
	return byte(v), err == nil
}

func decodeJSONEscape(s string) (rune, int) {
	if len(s) < 2 {
		return 0, 0
	}
	switch s[1] {
	case '"', '\\', '/', '\'':
		return rune(s[1]), 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'u':
		high, ok := unicodeEscape(s)
		if !ok {
			return 0, 0
		}
		if utf16.IsSurrogate(high) {
			if low, ok := unicodeEscape(s[6:]); ok {
				if r := utf16.DecodeRune(high, low); r != unicode.ReplacementChar {
					return r, 12
				}
			}
			return 0, 0
		}
		return high, 6
	}
	return 0, 0
}

func unicodeEscape(s string) (rune, bool) {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return 0, false
	}
	v, err := strconv.ParseUint(s[2:6], 16, 16)
	return rune(v), err == nil
}

// base64Forms lists the base64 forms of a value. It may also be a part of what
// was encoded, as in user:password: at each alignment, the characters that only
// depend on the value are hidden.
func base64Forms(value string) []string {
	var forms []string
	add := func(form string) {
		forms = append(forms, form, strings.NewReplacer("+", "-", "/", "_").Replace(form))
	}
	add(base64.StdEncoding.EncodeToString([]byte(value)))
	add(base64.RawStdEncoding.EncodeToString([]byte(value)))
	const minPart = 8 // shorter, it would match ordinary base64 by chance
	for offset := range 3 {
		data := append(make([]byte, offset), value...)
		encoded := base64.RawStdEncoding.EncodeToString(data)
		start := map[int]int{0: 0, 1: 2, 2: 3}[offset]
		end := len(encoded)
		if len(data)%3 != 0 {
			end--
		}
		if end-start >= minPart {
			add(encoded[start:end])
		}
	}
	return forms
}

// cutMarkAt returns where a cut mark starting at text[i] ends, or -1.
func cutMarkAt(text string, i int) int {
	const open, close = "[... ", " ...]"
	if !strings.HasPrefix(text[i:], open) {
		return -1
	}
	rest := text[i+len(open):]
	rest = rest[:min(len(rest), maxMarkNote+len(close))]
	end := strings.Index(rest, close)
	if end < 0 || strings.ContainsAny(rest[:end], "[]\n") {
		return -1
	}
	return i + len(open) + end + len(close)
}

// startBefore looks for the start of a value that a cut left just before a mark at
// text[at], no further back than from. It returns where that part starts and
// ends, or -1.
func (c *Cleaner) startBefore(text string, from, at int) (start, end int) {
	end = at
	if end > from && text[end-1] == '\n' {
		end--
	}
	// A cut in the middle of a character leaves its first bytes.
	stop := end
	for k := 1; k < utf8.UTFMax && stop-k >= from; k++ {
		if utf8.RuneStart(text[stop-k]) {
			if !utf8.FullRuneInString(text[stop-k : stop]) {
				stop -= k
			}
			break
		}
	}
	start = -1
	earliest := func(s int) {
		if start < 0 || s < start {
			start = s
		}
	}
	for _, value := range c.values {
		lo := max(from, stop-maxEscape*len(string(value))-maxEscape)
		for s := lo; s <= stop-minAnywhere && (start < 0 || s < start); s++ {
			if c.first[text[s]] && startsValue(text, s, stop, value, 0) {
				earliest(s)
				break
			}
		}
	}
	for _, form := range c.exact {
		for s := max(from, stop-len(form)); s <= stop-minAnywhere; s++ {
			if strings.HasPrefix(form, text[s:stop]) {
				earliest(s)
				break
			}
		}
	}
	for _, form := range c.folded {
		for s := max(from, stop-len(form)); s <= stop-minAnywhere; s++ {
			if strings.EqualFold(form[:stop-s], text[s:stop]) {
				earliest(s)
				break
			}
		}
	}
	if start < 0 {
		return -1, -1
	}
	return start, end
}

// startsValue reports whether text[i:end] is how value starts, read as matchValue
// does, with at least minAnywhere characters of it counting those before.
func startsValue(text string, i, end int, value []rune, before int) bool {
	for k, want := range value {
		if i >= end {
			return before+k >= minAnywhere
		}
		for _, escape := range escapesAt(text[i:end]) {
			if escape.size > 0 && sameRune(escape.r, want) && startsValue(text, i+escape.size, end, value[k+1:], before+k+1) {
				return true
			}
		}
		r, n := utf8.DecodeRuneInString(text[i:end])
		if !sameRune(r, want) {
			// The cut may have left the first bytes of an encoded character.
			return strings.IndexByte(`%&\`, text[i]) >= 0 && end-i < maxEscape && before+k >= minAnywhere
		}
		i += n
	}
	return i == end
}

// endAfter looks for the end of a value that a cut left just after a mark ending
// at text[at]. It returns where that part starts and ends, or -1.
func (c *Cleaner) endAfter(text string, at int) (start, end int) {
	start = at
	if start < len(text) && text[start] == '\n' {
		start++
	}
	// A cut in the middle of a character leaves its last bytes.
	from := start
	for k := 0; k < utf8.UTFMax-1 && from < len(text) && !utf8.RuneStart(text[from]); k++ {
		from++
	}
	end = -1
	for _, value := range c.values {
		for k := range value {
			if len(value)-k < minAnywhere {
				break
			}
			end = max(end, matchValue(text, from, value[k:]))
		}
	}
	for _, form := range c.exact {
		for k := range len(form) - minAnywhere + 1 {
			if strings.HasPrefix(text[from:], form[k:]) {
				end = max(end, from+len(form)-k)
				break
			}
		}
	}
	for _, form := range c.folded {
		for k := range len(form) - minAnywhere + 1 {
			if n := len(form) - k; len(text)-from >= n && strings.EqualFold(text[from:from+n], form[k:]) {
				end = max(end, from+n)
				break
			}
		}
	}
	if end < 0 {
		return -1, -1
	}
	return start, end
}
