package web

import (
	"html/template"
	"strings"
)

// navIcons are 24x24 stroke icons drawn in currentColor, keyed by name.
var navIcons = map[string]string{
	"fleet":     `<rect x="3" y="4" width="18" height="6" rx="2"/><rect x="3" y="14" width="18" height="6" rx="2"/><path d="M7 7h.01M7 17h.01"/>`,
	"incidents": `<path d="M10.3 3.9 2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/><path d="M12 9v4M12 17h.01"/>`,
	"announce":  `<path d="M3 10v4l13 5V5L3 10z"/><path d="M7 15.5V18a2 2 0 0 0 4 0v-1"/><path d="M19.5 9.5a3.5 3.5 0 0 1 0 5"/>`,
	"silence":   `<path d="M8.7 3.6A6 6 0 0 1 18 8c0 3 .6 5 1.3 6.4M17 17H3s3-2 3-9c0-.6.1-1.2.3-1.8"/><path d="M10.3 21a1.9 1.9 0 0 0 3.4 0"/><path d="m3 3 18 18"/>`,
	"dashboard": `<rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/>`,
	"pulse":     `<path d="M3 12h4l3-8 4 16 3-8h4"/>`,
	"host":      `<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>`,
	"bell":      `<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.9 1.9 0 0 0 3.4 0"/>`,
	"history":   `<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5M12 7v5l3 2"/>`,
	"sliders":   `<path d="M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1"/><circle cx="15" cy="6" r="2"/><circle cx="9" cy="12" r="2"/><circle cx="17" cy="18" r="2"/>`,
	"send":      `<path d="M22 2 11 13"/><path d="M22 2 15 22l-4-9-9-4 20-7z"/>`,
	"users":     `<circle cx="9" cy="8" r="4"/><path d="M2 21a7 7 0 0 1 14 0"/><path d="M16 4.1a4 4 0 0 1 0 7.8M22 21a7 7 0 0 0-4-6.3"/>`,
	"globe":     `<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18 14 14 0 0 1 0-18"/>`,
	"status":    `<circle cx="12" cy="12" r="9"/><path d="m8 12 3 3 5-6"/>`,
	"update":    `<circle cx="12" cy="12" r="9"/><path d="M12 16V8M8 12l4-4 4 4"/>`,
	"zap":       `<path d="M13 2 4 14h7l-1 8 9-12h-7l1-8z"/>`,
	"shield":    `<path d="M12 3 4 6v6c0 5 3.5 8 8 9 4.5-1 8-4 8-9V6l-8-3z"/>`,
	"file":      `<path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6M8 13h8M8 17h5"/>`,
	"list":      `<path d="M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01"/>`,
	"more":      `<path d="M5 12h.01M12 12h.01M19 12h.01" stroke-width="3"/>`,
	// service states on the status page
	"operational": `<circle cx="12" cy="12" r="9"/><path d="m8 12 3 3 5-6"/>`,
	"degraded":    `<path d="M10.3 3.9 2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/><path d="M12 9v4M12 17h.01"/>`,
	"outage":      `<circle cx="12" cy="12" r="9"/><path d="m15 9-6 6M9 9l6 6"/>`,
	"maintenance": `<path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.6 2.6-2.4-.6-.6-2.4z"/>`,
}

// icon renders the named nav icon as inline SVG; unknown names render nothing.
func icon(name string) template.HTML {
	body, ok := navIcons[name]
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<svg class="i" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">`)
	b.WriteString(body)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// orDash renders an unknown value as an em dash instead of a blank.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func init() {
	funcMap["icon"] = icon
	funcMap["orDash"] = orDash
}
