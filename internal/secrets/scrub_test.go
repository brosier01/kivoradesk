package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanHidesEveryCommonForm(t *testing.T) {
	const value = `Abc~1!x y"<&'é€`
	escaped, _ := json.Marshal(value)
	raw := []byte(value)
	cases := map[string]string{
		"as is":                      value,
		"Go query":                   url.QueryEscape(value),
		"Go path":                    url.PathEscape(value),
		"browser form":               `Abc%7E1%21x+y%22%3C%26%27%C3%A9%E2%82%AC`,
		"encodeURIComponent":         `Abc~1!x%20y%22%3C%26'%C3%A9%E2%82%AC`,
		"lower case percent":         `Abc%7e1%21x+y%22%3c%26%27%c3%a9%e2%82%ac`,
		"encoded twice":              `Abc%257E1%2521x%2520y%2522%253C%2526%2527%25C3%25A9%25E2%2582%25AC`,
		"Go HTML":                    html.EscapeString(value),
		"named entities":             `Abc~1!x y&quot;&lt;&amp;&apos;&eacute;&euro;`,
		"numeric entities":           `&#65;bc&#126;1&#33;x&#32;y&#34;&#60;&#38;&#39;&#233;&#8364;`,
		"hexadecimal entities":       `Abc~1!x y&#x22;&#x3C;&#x26;&#x27;&#xE9;&#x20AC;`,
		"Go JSON":                    string(escaped[1 : len(escaped)-1]),
		"site JSON, no HTML escapes": `Abc~1!x y\"<&'é€`,
		"JSON unicode escapes":       `Abc~1!x y"<&'é€`,
		"upper case":                 strings.ToUpper(value),
		"base64":                     base64.StdEncoding.EncodeToString(raw),
		"base64 without padding":     base64.RawStdEncoding.EncodeToString(raw),
		"base64 for URLs":            base64.URLEncoding.EncodeToString(raw),
		"hexadecimal":                hex.EncodeToString(raw),
		"upper case hexadecimal":     strings.ToUpper(hex.EncodeToString(raw)),
	}
	c := NewCleaner([]string{value})
	for name, form := range cases {
		got := c.Clean("before " + form + " after")
		if got != "before "+Replacement+" after" {
			t.Errorf("%s: Clean(%q) = %q", name, form, got)
		}
	}
}

// In user:password, the base64 of the password starts anywhere.
func TestCleanHidesAValueInsideBase64(t *testing.T) {
	const value = "hunter2-long-enough"
	c := NewCleaner([]string{value})
	for _, prefix := range []string{"", "a", "ab", "user:"} {
		encoded := base64.StdEncoding.EncodeToString([]byte(prefix + value + ":more"))
		if got := c.Clean(encoded); !strings.Contains(got, Replacement) {
			t.Errorf("prefix %q: Clean(%q) = %q", prefix, encoded, got)
		}
	}
}

// The longest form goes first: hiding only the value inside it would leave the
// rest of the encoded form visible.
func TestCleanPrefersTheLongestForm(t *testing.T) {
	c := NewCleaner([]string{"abcd", "abcd+more", "abcd%20more"})
	for in, want := range map[string]string{
		"abcd+more":       Replacement,
		"abcd more":       Replacement,
		"abcd%20more":     Replacement,
		"abcd%2520more":   Replacement,
		"abcd%2Bmore":     Replacement,
		"abcd-other":      Replacement + "-other",
		"xabcdx abcd%20m": "x" + Replacement + "x " + Replacement + "%20m",
	} {
		if got := c.Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanReadsAPercentSignBothWays(t *testing.T) {
	c := NewCleaner([]string{"100%25off"})
	for _, in := range []string{"100%25off", "100%2525off", "100%25%6Fff"} {
		if got := c.Clean(in); got != Replacement {
			t.Errorf("Clean(%q) = %q", in, got)
		}
	}
}

func TestCleanLeavesOtherTextAlone(t *testing.T) {
	c := NewCleaner([]string{"hunter2", "   "})
	for _, in := range []string{"", "nothing here", "hunter", "%41 &amp; \\n %", "bad \xff bytes", "   spaces   ", "&#xZZ; &unknown; %ZZ \\uZZZZ \\"} {
		if got := c.Clean(in); got != in {
			t.Errorf("Clean(%q) = %q", in, got)
		}
	}
	if got := NewCleaner(nil).Clean("anything %41"); got != "anything %41" {
		t.Errorf("no values: %q", got)
	}
}

// A short value would garble ordinary text: it only goes as a whole word.
func TestCleanHidesShortValuesAsWords(t *testing.T) {
	c := NewCleaner([]string{"123"})
	for in, want := range map[string]string{
		"your PIN is 123.": "your PIN is " + Replacement + ".",
		"123":              Replacement,
		"1234 and 0123":    "1234 and 0123",
		"a123b":            "a123b",
	} {
		if got := c.Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanSeveralValuesAndInvalidUTF8(t *testing.T) {
	c := NewCleaner([]string{"first-value", "second-value"})
	got := c.Clean("\xff first-value \xfe SECOND-VALUE \xfd")
	if want := fmt.Sprintf("\xff %s \xfe %s \xfd", Replacement, Replacement); got != want {
		t.Fatalf("Clean = %q, want %q", got, want)
	}
}

func FuzzClean(f *testing.F) {
	f.Add("hunter2", "x hunter2 y")
	f.Add("a b", "a+b a%20b &#97; \\u0061")
	f.Add("é", "%C3%A9 %c3 % %2")
	f.Fuzz(func(t *testing.T, value, text string) {
		c := NewCleaner([]string{value})
		got := c.Clean(text)
		// The store only holds valid text.
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || len(value) < minAnywhere || !strings.Contains(text, value) {
			return
		}
		if strings.Contains(got, value) && !strings.Contains(Replacement, value) {
			t.Fatalf("Clean(%q) with %q = %q", text, value, got)
		}
	})
}
