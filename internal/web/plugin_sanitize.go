package web

import (
	"html"
	"html/template"
	"net/url"
	"strings"
)

// sanitizeText renders the "lite" subset: paragraphs, **bold**, *em*, `code`, "- " lists and https links; everything else is escaped.
func sanitizeText(s string) template.HTML {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	var b strings.Builder
	for _, block := range strings.Split(s, "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
			continue
		}
		if allListItems(lines) {
			b.WriteString("<ul>")
			for _, l := range lines {
				b.WriteString("<li>" + liteInline(strings.TrimPrefix(l, "- ")) + "</li>")
			}
			b.WriteString("</ul>")
			continue
		}
		parts := make([]string, len(lines))
		for i, l := range lines {
			parts[i] = liteInline(l)
		}
		b.WriteString("<p>" + strings.Join(parts, "<br>") + "</p>")
	}
	return template.HTML(b.String())
}

func allListItems(lines []string) bool {
	for _, l := range lines {
		if !strings.HasPrefix(l, "- ") {
			return false
		}
	}
	return len(lines) > 0
}

func liteInline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		switch {
		case rest[0] == '`':
			if j := strings.IndexByte(rest[1:], '`'); j >= 0 {
				b.WriteString("<code>" + html.EscapeString(rest[1:1+j]) + "</code>")
				i += j + 2
				continue
			}
		case strings.HasPrefix(rest, "**"):
			if j := strings.Index(rest[2:], "**"); j > 0 {
				b.WriteString("<strong>" + html.EscapeString(rest[2:2+j]) + "</strong>")
				i += j + 4
				continue
			}
		case rest[0] == '*':
			if j := strings.IndexByte(rest[1:], '*'); j > 0 {
				b.WriteString("<em>" + html.EscapeString(rest[1:1+j]) + "</em>")
				i += j + 2
				continue
			}
		case rest[0] == '[':
			if label, href, n, ok := parseLink(rest); ok {
				if safeHTTPS(href) {
					b.WriteString(`<a href="` + html.EscapeString(href) + `" rel="noopener noreferrer nofollow" target="_blank">` + html.EscapeString(label) + `</a>`)
				} else {
					b.WriteString(html.EscapeString(label))
				}
				i += n
				continue
			}
		}
		b.WriteString(html.EscapeString(rest[:1]))
		i++
	}
	return b.String()
}

func parseLink(s string) (label, href string, n int, ok bool) {
	close := strings.Index(s, "](")
	if close < 0 {
		return
	}
	end := strings.IndexByte(s[close+2:], ')')
	if end < 0 {
		return
	}
	return s[1:close], s[close+2 : close+2+end], close + 2 + end + 1, true
}

func safeHTTPS(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t\n\"'<>\\`") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
