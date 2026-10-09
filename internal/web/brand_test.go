package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Trinetra brand palette (see the rename design's global constraints).
const (
	brandInk       = "#0E1114"
	brandGraphite  = "#1C2127"
	brandSlate     = "#8A949E"
	brandAsh       = "#ECE8E1"
	brandEmber     = "#FF5B1F"
	brandVerdigris = "#3FBFA6"
	brandAmber     = "#F2B23A"
)

// TestBrandAssetsServed pins that the copied brand kit files and the embedded Outfit font
// are served from the embedded asset tree with a deterministic Content-Type.
func TestBrandAssetsServed(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	cases := map[string]string{
		"/assets/brand/favicon.ico":                                 "image/x-icon",
		"/assets/brand/icon-32.png":                                 "image/png",
		"/assets/brand/icon-180.png":                                "image/png",
		"/assets/brand/icon-192.png":                                "image/png",
		"/assets/brand/icon-512.png":                                "image/png",
		"/assets/brand/trinetra-wordmark-ash.png":                   "image/png",
		"/assets/brand/trinetra-wordmark-ink.png":                   "image/png",
		"/assets/fonts/outfit-latin-wght-normal.6c18d579fd87.woff2": "font/woff2",
		"/assets/fonts/OFL.txt":                                     "text/plain; charset=utf-8",
	}
	for path, wantCT := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != wantCT {
			t.Errorf("GET %s Content-Type = %q, want %q", path, ct, wantCT)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
}

// brandPages renders the four branded surfaces: the signed-in app shell
// (sidebar), login, enroll and the anonymous public status page.
func brandPages(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}

	d := enrollTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	get := func(name string, req *http.Request, hh http.Handler) {
		rec := httptest.NewRecorder()
		hh.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, rec.Code)
		}
		out[name] = rec.Body.String()
	}
	get("dashboard", seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"), h)
	get("login", httptest.NewRequest(http.MethodGet, "/login", nil), h)
	get("enroll", httptest.NewRequest(http.MethodGet, "/enroll", nil), h)

	pd, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	pd.Snapshot = func() DashboardView { return publicTestSnapshot() }
	get("public", httptest.NewRequest(http.MethodGet, "/", nil), newHandler(pd))
	return out
}

// TestPagesCarryWordmarkAndFavicons pins the visual identity on every branded surface: both
// theme variants of the wordmark (ash for dark, ink for light) with alt="trinetra".
func TestPagesCarryWordmarkAndFavicons(t *testing.T) {
	for name, body := range brandPages(t) {
		for _, want := range []string{
			`src="/assets/brand/trinetra-wordmark-ash.png?v=` + assetVersion + `"`,
			`src="/assets/brand/trinetra-wordmark-ink.png?v=` + assetVersion + `"`,
			`alt="trinetra"`,
			`class="wordmark wm-ash"`,
			`class="wordmark wm-ink"`,
			`rel="icon" href="/assets/brand/favicon.ico?v=` + assetVersion + `"`,
			`rel="icon" type="image/png" sizes="32x32" href="/assets/brand/icon-32.png?v=` + assetVersion + `"`,
			`rel="apple-touch-icon" href="/assets/brand/icon-180.png?v=` + assetVersion + `"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s page missing %s", name, want)
			}
		}
		if strings.Contains(body, `class="lamp"`) {
			t.Errorf("%s page still renders the old .lamp brand dot", name)
		}
		if strings.Contains(body, `rel="manifest"`) {
			t.Errorf("%s page links a web manifest; the setup is manifest-free", name)
		}
	}
}

// externalAssetURL matches an absolute or protocol-relative URL ("https://x", "http://x",
// "//x") that LOADS something: an HTML src/href attribute (or a JS .src= assignment).
var externalAssetURL = regexp.MustCompile(`(?i)(\b(src|href)\s*=\s*["'\x60]?\s*(https?:)?//|url\(\s*["']?\s*(https?:)?//|@import\s+(url\()?\s*["']?\s*(https?:)?//|\b(fetch|EventSource|import)\s*\(\s*["'\x60](https?:)?//)`)

// srcsetAttr captures a srcset attribute's value so every candidate -- not
// just the first -- can be checked.
var srcsetAttr = regexp.MustCompile(`(?i)\bsrcset\s*=\s*("([^"]*)"|'([^']*)')`)

// externalLoads returns every external load reference in src.
func externalLoads(src string) []string {
	out := externalAssetURL.FindAllString(src, -1)
	for _, m := range srcsetAttr.FindAllStringSubmatch(src, -1) {
		val := m[2] + m[3]
		for _, cand := range strings.Split(val, ",") {
			f := strings.Fields(cand)
			if len(f) == 0 {
				continue
			}
			u := strings.ToLower(f[0])
			if strings.HasPrefix(u, "//") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
				out = append(out, "srcset candidate "+f[0])
			}
		}
	}
	return out
}

// TestNoExternalAssetURLs pins "no runtime external requests": every template, stylesheet
// and app.js must load its fonts, images, scripts, styles and data from this server.
func TestNoExternalAssetURLs(t *testing.T) {
	check := func(fsys fs.FS, root string, exts ...string) int {
		n := 0
		_ = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			ok := false
			for _, e := range exts {
				ok = ok || strings.HasSuffix(p, e)
			}
			if !ok || strings.HasSuffix(p, ".min.js") {
				return nil
			}
			n++
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			for _, m := range externalLoads(string(b)) {
				t.Errorf("%s loads an external resource: %q", p, m)
			}
			return nil
		})
		return n
	}
	if n := check(templatesFS, "templates", ".html"); n == 0 {
		t.Fatal("no templates scanned")
	}
	if n := check(assetsFS, "assets", ".css", "app.js"); n < 2 {
		t.Fatalf("scanned %d stylesheets/app.js, want style.css + uPlot.min.css + app.js", n)
	}
	// The matcher itself must catch the shapes it claims to...
	for _, bad := range []string{
		`<img src="https://x/y.png">`,
		`<script src=//cdn.example/x.js></script>`,
		`@font-face{src:url(https://fonts.gstatic.com/a.woff2)}`,
		`@font-face{src:url("//fonts.gstatic.com/a.woff2")}`,
		`@import "http://x/a.css";`,
		`@import url(//x/a.css);`,
		`<link href='https://x/a.css'>`,
		`<img srcset="/a.png 1x, https://x/b.png 2x">`,
		`<img srcset="/a.png 1x, //x/b.png 2x">`,
		`img.src='https://x/p.gif'`,
		`fetch('https://api.example/v1')`,
		"fetch(`//api.example/v1`)",
		`new EventSource("https://x/events")`,
	} {
		if len(externalLoads(bad)) == 0 {
			t.Errorf("matcher misses %q", bad)
		}
	}
	// ...and must not flag same-origin loads, comments or placeholders.
	for _, good := range []string{
		`<img src="/assets/a.png">`,
		`<img srcset="/a.png 1x, /b.png 2x">`,
		`fetch('/api/series?metric=cpu')`,
		`  // a JS line comment -- not a URL`,
		`<input placeholder="https://ntfy.sh">`,
		`url(/assets/fonts/x.woff2)`,
	} {
		if got := externalLoads(good); len(got) != 0 {
			t.Errorf("matcher falsely flags %q: %v", good, got)
		}
	}
}

// cssThemeVars parses the custom properties declared in style.css's
// `:root[data-theme="<theme>"]{...}` block.
func cssThemeVars(t *testing.T, css, theme string) map[string]string {
	t.Helper()
	sel := `:root[data-theme="` + theme + `"]{`
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatalf("style.css has no %s block", sel)
	}
	body := css[i+len(sel):]
	body = body[:strings.Index(body, "}")]
	vars := map[string]string{}
	for _, decl := range strings.Split(body, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(decl), ":")
		if ok && strings.HasPrefix(k, "--") {
			vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return vars
}

// relLum is the WCAG 2.x relative luminance of an opaque #RRGGBB colour.
func relLum(t *testing.T, hex string) float64 {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("not an opaque #RRGGBB colour: %q", hex)
	}
	ch := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q: %v", hex, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[1:3]) + 0.7152*ch(hex[3:5]) + 0.0722*ch(hex[5:7])
}

func contrast(t *testing.T, a, b string) float64 {
	la, lb := relLum(t, a), relLum(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func readStyleCSS(t *testing.T) string {
	t.Helper()
	b, err := assetsFS.ReadFile("assets/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	return string(b)
}

// TestStyleCSSUsesBrandPalette pins the token remap: ink/graphite/ash surfaces and text,
// ember for --signal/--crit, verdigris for --ok and amber for --warn in the dark theme.
func TestStyleCSSUsesBrandPalette(t *testing.T) {
	css := readStyleCSS(t)
	dark := cssThemeVars(t, css, "dark")
	light := cssThemeVars(t, css, "light")
	want := []struct {
		theme map[string]string
		name  string
		k, v  string
	}{
		{dark, "dark", "--bg", brandInk},
		{dark, "dark", "--surface", brandGraphite},
		{dark, "dark", "--text", brandAsh},
		{dark, "dark", "--muted", brandSlate},
		{dark, "dark", "--signal", brandEmber},
		{dark, "dark", "--crit", brandEmber},
		{dark, "dark", "--ok", brandVerdigris},
		{dark, "dark", "--warn", brandAmber},
		{light, "light", "--bg", brandAsh},
		{light, "light", "--text", brandInk},
		{light, "light", "--signal", brandEmber},
	}
	for _, w := range want {
		if got := w.theme[w.k]; !strings.EqualFold(got, w.v) {
			t.Errorf("%s %s = %q, want %s", w.name, w.k, got, w.v)
		}
	}
}

// TestStyleCSSContrast checks WCAG contrast of the text tokens on every opaque surface in
// both themes: body text and --muted at least 4.5:1, --faint.
func TestStyleCSSContrast(t *testing.T) {
	css := readStyleCSS(t)
	for _, theme := range []string{"dark", "light"} {
		v := cssThemeVars(t, css, theme)
		for _, surf := range []string{"--bg", "--surface", "--elev"} {
			for _, tc := range []struct {
				tok string
				min float64
			}{{"--text", 4.5}, {"--muted", 4.5}, {"--faint", 3.0}, {"--crit", 4.5}, {"--ok", 4.5}, {"--warn", 4.5}} {
				r := contrast(t, v[tc.tok], v[surf])
				t.Logf("%-5s %-7s on %-9s %s on %s = %.2f:1", theme, tc.tok, surf, v[tc.tok], v[surf], r)
				if r < tc.min {
					t.Errorf("%s: %s %s on %s %s = %.2f:1, want >= %.1f:1", theme, tc.tok, v[tc.tok], surf, v[surf], r, tc.min)
				}
			}
		}
		// The data-series tokens are allowed only where contrast doesn't apply (lines, fills,
		// swatches); if one is ever used as text it must meet 4.5:1 like any other text token.
		for _, tok := range dataSeriesTextUses(t) {
			for _, surf := range []string{"--bg", "--surface", "--elev"} {
				if r := contrast(t, v[tok], v[surf]); r < 4.5 {
					t.Errorf("%s: data-series token %s %s is used as text but is %.2f:1 on %s", theme, tok, v[tok], r, surf)
				}
			}
		}
		// The primary button draws its label on --signal; the mobile alert
		// count badge draws its number on --crit.
		for _, p := range [][2]string{{"--on-signal", "--signal"}, {"--on-crit", "--crit"}} {
			r := contrast(t, v[p[0]], v[p[1]])
			t.Logf("%-5s %s %s on %s %s = %.2f:1", theme, p[0], v[p[0]], p[1], v[p[1]], r)
			if r < 4.5 {
				t.Errorf("%s: %s on %s = %.2f:1, want >= 4.5:1", theme, p[0], p[1], r)
			}
		}
	}
}

// TestStyleCSSEmbedsOutfitForHeadings pins the display face: Outfit is declared via
// @font-face from the embedded woff2 and used for headings only.
func TestStyleCSSEmbedsOutfitForHeadings(t *testing.T) {
	css := readStyleCSS(t)
	for _, want := range []string{
		`@font-face{font-family:"Outfit"`,
		`url(/assets/fonts/outfit-latin-wght-normal.6c18d579fd87.woff2) format("woff2")`,
		`--font-display:"Outfit",`,
		`h1,h2,h3{font-family:var(--font-display)`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %s", want)
		}
	}
	if strings.Contains(css, "body{background:var(--bg); color:var(--text); font-family:var(--font-display)") {
		t.Error("body text must not use the display face")
	}
}

// dataSeriesText matches a text-colour use (color:, not background-color: /
// stop-color: / border-color:) of a data-series token.
var dataSeriesText = regexp.MustCompile(`(?:^|[^-a-zA-Z])color\s*:\s*var\(\s*(--info|--violet|--cyan|--pink)\s*\)`)

// dataSeriesTextUses returns the data-series tokens (--info/--violet/--cyan/ --pink) that
// style.css, a template or app.js uses as text colour.
func dataSeriesTextUses(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	scan := func(fsys fs.FS, p string) {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, m := range dataSeriesText.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	scan(assetsFS, "assets/style.css")
	scan(assetsFS, "assets/app.js")
	_ = fs.WalkDir(templatesFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			scan(templatesFS, p)
		}
		return err
	})
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// TestChromeUsesNeutralTokens pins that links, the role badge, the avatar and on-switches
// use neutral brand tokens, not the data-series palette or verdigris.
func TestChromeUsesNeutralTokens(t *testing.T) {
	if got := dataSeriesTextUses(t); len(got) != 0 {
		t.Errorf("data-series tokens used as text colour: %v", got)
	}
	css := readStyleCSS(t)
	for _, want := range []string{
		`a{color:var(--text); text-decoration:underline; text-decoration-color:var(--faint)`,
		`.badge.role{color:var(--text);border-color:var(--border)}`,
		`.avatar{width:26px;height:26px;border-radius:50%;background:var(--elev);border:1px solid var(--border);`,
		`.switch.on{background:var(--text);`,
		`input.switch:checked{background:var(--text);`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %s", want)
		}
	}
	for _, gone := range []string{"conic-gradient(from 210deg,var(--info)", ".mockbar", "#gSig", "var(--ok) 60%"} {
		if strings.Contains(css, gone) {
			t.Errorf("style.css still contains %q", gone)
		}
	}
	if b, _ := assetsFS.ReadFile("assets/app.js"); strings.Contains(string(b), "'gSig'") {
		t.Error("app.js still emits the unused gSig gradient")
	}
}

// fontURL captures the font files style.css loads.
var fontURL = regexp.MustCompile(`url\(/assets/fonts/([^)]+)\)`)

// hashedFontName captures the 12-hex content hash embedded in a font name.
var hashedFontName = regexp.MustCompile(`\.([0-9a-f]{12})\.woff2$`)

// TestOutfitFontNameIsContentHashed pins cache safety for the font: /assets/ is served
// immutable and style.css (static) can't use the ?v= helper.
func TestOutfitFontNameIsContentHashed(t *testing.T) {
	css := readStyleCSS(t)
	refs := fontURL.FindAllStringSubmatch(css, -1)
	if len(refs) == 0 {
		t.Fatal("style.css references no /assets/fonts/ file")
	}
	referenced := map[string]bool{}
	for _, r := range refs {
		referenced[r[1]] = true
	}
	entries, err := fs.ReadDir(assetsFS, "assets/fonts")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".woff2") {
			continue
		}
		if !referenced[name] {
			t.Errorf("fonts/%s is embedded but style.css does not reference it", name)
		}
		delete(referenced, name)
		m := hashedFontName.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("fonts/%s has no .<12-hex sha256>.woff2 suffix", name)
			continue
		}
		b, err := assetsFS.ReadFile("assets/fonts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:])[:12]; got != m[1] {
			t.Errorf("fonts/%s content hash is %s; rename the file to match", name, got)
		}
	}
	for name := range referenced {
		t.Errorf("style.css references fonts/%s which is not embedded", name)
	}
}

// cssRule is one style rule, with the @media condition (if any) it sits in.
type cssRule struct {
	media, selector, decls string
	order                  int
}

// parseCSSRules is a minimal parser, good enough for style.css: top-level
// rules plus one level of @media nesting (comments stripped).
func parseCSSRules(css string) []cssRule {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var out []cssRule
	var walk func(s, media string)
	walk = func(s, media string) {
		for {
			open := strings.Index(s, "{")
			if open < 0 {
				return
			}
			head := strings.TrimSpace(s[:open])
			if strings.HasPrefix(head, "@media") {
				depth, i := 1, open+1
				for ; i < len(s) && depth > 0; i++ {
					switch s[i] {
					case '{':
						depth++
					case '}':
						depth--
					}
				}
				walk(s[open+1:i-1], strings.TrimSpace(strings.TrimPrefix(head, "@media")))
				s = s[i:]
				continue
			}
			end := strings.Index(s[open:], "}")
			if end < 0 {
				return
			}
			for _, sel := range strings.Split(head, ",") {
				out = append(out, cssRule{media: media, selector: strings.TrimSpace(sel), decls: s[open+1 : open+end], order: len(out)})
			}
			s = s[open+end+1:]
		}
	}
	walk(css, "")
	return out
}

// TestWordmarkThemeSwap evaluates style.css's wordmark display rules for every theme state
// -- data-theme unset/dark/light x OS dark/light.
func TestWordmarkThemeSwap(t *testing.T) {
	var rules []cssRule
	for _, r := range parseCSSRules(readStyleCSS(t)) {
		if strings.Contains(r.selector, "wm-") || strings.Contains(r.selector, ".wordmark") {
			if strings.Contains(r.decls, "display") {
				rules = append(rules, r)
			}
		}
	}
	if len(rules) == 0 {
		t.Fatal("no wordmark display rules found")
	}
	// A rule's prefix (everything before the wordmark class) must be one of
	// these root conditions; specificity counts :root, [attr] and :not(...).
	type cond struct {
		spec  int
		match func(attr string) bool
	}
	prefixes := map[string]cond{
		"":                          {0, func(string) bool { return true }},
		`:root[data-theme="dark"]`:  {2, func(a string) bool { return a == "dark" }},
		`:root[data-theme="light"]`: {2, func(a string) bool { return a == "light" }},
		`:root:not([data-theme])`:   {2, func(a string) bool { return a == "" }},
	}
	medias := map[string]func(os string) bool{
		"":                              func(string) bool { return true },
		"(prefers-color-scheme:light)":  func(os string) bool { return os == "light" },
		"(prefers-color-scheme: light)": func(os string) bool { return os == "light" },
		"(prefers-color-scheme:dark)":   func(os string) bool { return os == "dark" },
		"(prefers-color-scheme: dark)":  func(os string) bool { return os == "dark" },
	}
	display := regexp.MustCompile(`display\s*:\s*([a-z-]+)`)
	visible := func(class, attr, os string) bool {
		best, bestSpec, bestOrder := "inline", -1, -1
		for _, r := range rules {
			mf, ok := medias[r.media]
			if !ok {
				t.Fatalf("wordmark rule in unhandled @media %q", r.media)
			}
			sel := r.selector
			var target, prefix string
			switch {
			case strings.HasSuffix(sel, ".wordmark"):
				target, prefix = "wordmark", strings.TrimSpace(strings.TrimSuffix(sel, ".wordmark"))
			case strings.HasSuffix(sel, ".wm-ash"):
				target, prefix = "wm-ash", strings.TrimSpace(strings.TrimSuffix(sel, ".wm-ash"))
			case strings.HasSuffix(sel, ".wm-ink"):
				target, prefix = "wm-ink", strings.TrimSpace(strings.TrimSuffix(sel, ".wm-ink"))
			default:
				// e.g. ".brand .wordmark{width:...}" -- only a problem if it
				// sets display (filtered above), so reject it.
				if strings.HasSuffix(sel, " .wordmark") || strings.Contains(sel, "wm-") {
					t.Fatalf("unhandled wordmark selector %q", sel)
				}
				continue
			}
			if target != "wordmark" && target != class {
				continue
			}
			c, ok := prefixes[prefix]
			if !ok {
				if target == "wordmark" {
					continue // scoped layout rule like ".brand .wordmark"
				}
				t.Fatalf("unhandled wordmark selector prefix %q in %q", prefix, sel)
			}
			if !mf(os) || !c.match(attr) {
				continue
			}
			m := display.FindStringSubmatch(r.decls)
			if m == nil {
				continue
			}
			spec := 1 + c.spec
			if spec > bestSpec || (spec == bestSpec && r.order > bestOrder) {
				best, bestSpec, bestOrder = m[1], spec, r.order
			}
		}
		return best != "none"
	}
	for _, attr := range []string{"", "dark", "light"} {
		for _, os := range []string{"dark", "light"} {
			effective := attr
			if effective == "" {
				effective = os
			}
			ash, ink := visible("wm-ash", attr, os), visible("wm-ink", attr, os)
			if ash == ink {
				t.Errorf("data-theme=%q os=%s: ash visible=%v ink visible=%v, want exactly one", attr, os, ash, ink)
				continue
			}
			if ash != (effective == "dark") {
				t.Errorf("data-theme=%q os=%s: shows %s, want %s wordmark", attr, os, map[bool]string{true: "ash", false: "ink"}[ash], map[bool]string{true: "ash", false: "ink"}[effective == "dark"])
			}
		}
	}
}
