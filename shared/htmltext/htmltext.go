// Package htmltext extracts readable text from HTML documents for agent
// consumption (TG-3, docs/tool-gateway.md §6.1 "html → readable
// text"). It is a deliberately small, dependency-free state machine:
//
//   - drops <script>, <style>, <head>, template blocks and comments
//   - preserves <title> text (it is readable content)
//   - inserts newlines at block-level tag boundaries
//   - decodes the common named entities and numeric &#...; references
//   - collapses runs of whitespace and blank lines
//
// It is NOT a full HTML parser; malformed markup degrades gracefully to
// "tags stripped, text kept".
package htmltext

import (
	"html"
	"strings"
	"unicode"
)

// skipTags are dropped entirely (tag and content).
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true,
	"template": true, "svg": true, "iframe": true,
}

// blockTags get a newline boundary so paragraphs/sections don't glue
// together into one unreadable line.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "header": true, "footer": true,
	"blockquote": true, "pre": true, "table": true, "tr": true,
	"ul": true, "ol": true, "nav": true, "main": true, "figure": true,
	"figcaption": true, "dd": true, "dt": true,
}

// Extract returns readable text for an HTML document. Empty input
// yields empty output.
func Extract(doc string) string {
	if strings.TrimSpace(doc) == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(doc) / 2)

	lower := strings.ToLower(doc)
	inSkip := ""     // tag name of the open skip block
	inHead := false  // <head> — keep only <title>
	inTitle := false

	for i := 0; i < len(doc); {
		c := doc[i]
		if c != '<' {
			if inSkip != "" {
				i++
				continue
			}
			if inHead && !inTitle {
				i++
				continue
			}
			b.WriteByte(c)
			i++
			continue
		}
		// c == '<'
		// Comment?
		if strings.HasPrefix(lower[i:], "<!--") {
			if end := strings.Index(lower[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
			} else {
				i = len(doc) // unterminated comment: drop the rest
			}
			continue
		}
		end := strings.IndexByte(doc[i:], '>')
		if end < 0 {
			// Dangling '<': emit it and stop tag processing.
			b.WriteByte(c)
			i++
			continue
		}
		tagRaw := doc[i+1 : i+end]
		name := tagName(tagRaw)
		closing := strings.HasPrefix(tagRaw, "/")

		switch {
		case inSkip != "":
			if closing && name == inSkip {
				inSkip = ""
			}
		case skipTags[name]:
			if !closing {
				inSkip = name
			}
		case name == "head":
			inHead = !closing
		case name == "title":
			if inHead || !closing {
				inTitle = !closing
				if !closing {
					b.WriteString("\n")
				}
			}
		default:
			if blockTags[name] {
				b.WriteString("\n")
			}
		}
		i += end + 1
	}

	return normalize(b.String())
}

// tagName extracts the lowercase tag name from raw tag content
// (handles "/div", "div class=...", "br/").
func tagName(raw string) string {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "/")
	j := 0
	for j < len(s) {
		c := s[j]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' || c == '.' {
			break
		}
		j++
	}
	return strings.ToLower(s[:j])
}

// normalize decodes entities and collapses whitespace.
func normalize(s string) string {
	s = html.UnescapeString(s)
	nb := strings.NewReplacer("\u00a0", " ", "\u200b", "", "\ufeff", "")
	s = nb.Replace(s)

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	prevBlank := true
	for _, ln := range lines {
		ln = strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return ' '
			}
			return r
		}, ln)
		ln = strings.TrimSpace(ln)
		if ln == "" {
			if !prevBlank && len(out) > 0 {
				out = append(out, "")
				prevBlank = true
			}
			continue
		}
		ln = strings.Join(strings.Fields(ln), " ")
		out = append(out, ln)
		prevBlank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
