package web

import (
	"encoding/json"
	"html/template"
)

// Attribution for the anonymous pages (public status, history, login, enroll): a small
// visible footer with followed links.
const (
	attributionProductURL = "https://www.infodivelabs.com/products/trinetra"
	attributionRepoURL    = "https://github.com/InfoDiveLabs/trinetra"
	attributionSiteURL    = "https://www.infodivelabs.com"
)

// attributionJSONLD is computed once from constants, so marking it as
// trusted JS for the <script type="application/ld+json"> block is safe.
var attributionJSONLD = func() template.JS {
	b, err := json.Marshal(map[string]any{
		"@context":            "https://schema.org",
		"@type":               "SoftwareApplication",
		"name":                "trinetra",
		"url":                 attributionProductURL,
		"applicationCategory": "DeveloperApplication",
		"operatingSystem":     "Linux",
		"sameAs":              []string{attributionSiteURL, attributionRepoURL},
	})
	if err != nil {
		panic(err)
	}
	return template.JS(b)
}()

// attributionFuncs are merged into funcMap.
var attributionFuncs = template.FuncMap{
	"attributionJSONLD":     func() template.JS { return attributionJSONLD },
	"attributionProductURL": func() string { return attributionProductURL },
	"attributionRepoURL":    func() string { return attributionRepoURL },
	"attributionSiteURL":    func() string { return attributionSiteURL },
}

func init() {
	for k, v := range attributionFuncs {
		funcMap[k] = v
	}
}
