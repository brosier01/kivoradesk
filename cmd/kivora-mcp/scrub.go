package main

import (
	"encoding/json"
	"html"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// minScrubbed is the shortest value worth hiding: replacing shorter ones would
// garble ordinary output.
const minScrubbed = 4

// scrubber hides the values of the credentials filled in this session from
// what the model gets back. A site can display what it was sent: an echo of the
// form, the URL of a search. It is a safety net: it matches the value as is
// and its common encodings, and cannot do anything about screenshots.
type scrubber struct {
	mu       sync.Mutex
	hidden   map[string]string // text to hide -> replacement
	replacer *strings.Replacer
}

func (c *scrubber) remember(name, value string) {
	if len(value) < minScrubbed {
		return
	}
	quoted, _ := json.Marshal(value)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hidden == nil {
		c.hidden = make(map[string]string)
	}
	for _, form := range []string{value, url.QueryEscape(value), url.PathEscape(value), html.EscapeString(value), string(quoted[1 : len(quoted)-1])} {
		c.hidden[form] = "[secret " + name + " hidden]"
	}
	// A replacer tries its pairs in order: longest first, so that a value
	// inside a longer encoded form does not hide only a part of it.
	forms := make([]string, 0, len(c.hidden))
	for form := range c.hidden {
		forms = append(forms, form)
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	pairs := make([]string, 0, 2*len(forms))
	for _, form := range forms {
		pairs = append(pairs, form, c.hidden[form])
	}
	c.replacer = strings.NewReplacer(pairs...)
}

func (c *scrubber) clean(s string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.replacer == nil {
		return s
	}
	return c.replacer.Replace(s)
}
